package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	db  *sql.DB
	log *slog.Logger
}

func OpenStore(ctx context.Context, dsn string, maxConns int, log *slog.Logger) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connessione al database: %w", err)
	}
	return &Store{db: db, log: log}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		stmt = stripComments(stmt)
		if stmt == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrazione: %w\n%s", err, stmt)
		}
	}
	return nil
}

func stripComments(stmt string) string {
	var b strings.Builder
	for _, line := range strings.Split(stmt, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// trunc taglia una stringa a n caratteri senza spezzare UTF-8, per non far
// fallire l'INSERT in strict mode su valori anomali.
func trunc(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func nz(t time.Time, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}

// SaveReport salva un report in un'unica transazione: host, stato corrente
// dei servizi (upsert + rimozione di quelli spariti dal monitrc) ed eventi.
func (s *Store) SaveReport(ctx context.Context, r *Report, remoteAddr string) error {
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		err = s.saveReportTx(ctx, r, remoteAddr)
		if err == nil || !retryable(err) || ctx.Err() != nil {
			return err
		}
		s.log.Debug("conflitto sul database, ritento", "host", r.Server.LocalHostname, "tentativo", attempt, "err", err)
		time.Sleep(time.Duration(attempt*attempt) * 20 * time.Millisecond)
	}
	return err
}

// retryable: deadlock, lock wait timeout, o due registrazioni concorrenti
// dello stesso monit_id (duplicate key sull'INSERT dell'host).
func retryable(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1213, 1205, 1062:
			return true
		}
	}
	return false
}

func (s *Store) saveReportTx(ctx context.Context, r *Report, remoteAddr string) error {
	// Precisione al millisecondo: deve coincidere con DATETIME(3), perché
	// usiamo updated_at per riconoscere i servizi non più presenti.
	now := time.Now().UTC().Truncate(time.Millisecond)

	// READ COMMITTED: niente gap lock, che con REPEATABLE READ mandano in
	// deadlock le registrazioni concorrenti di host nuovi.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	hostID, err := s.upsertHost(ctx, tx, r, remoteAddr, now)
	if err != nil {
		return fmt.Errorf("host: %w", err)
	}
	if err := s.upsertServices(ctx, tx, hostID, r.Services, now); err != nil {
		return fmt.Errorf("servizi: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM services WHERE host_id = ? AND updated_at <> ?`, hostID, now); err != nil {
		return fmt.Errorf("pulizia servizi: %w", err)
	}
	for _, e := range r.Events {
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO events
			  (host_id, service, service_type, event_id, state, action, message, collected_at, received_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			hostID, trunc(e.Service, 255), e.ServiceType, e.ID, e.State, e.Action,
			e.Message, nz(e.CollectedAt, now), now); err != nil {
			return fmt.Errorf("evento: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) upsertHost(ctx context.Context, tx *sql.Tx, r *Report, remoteAddr string, now time.Time) (uint64, error) {
	var (
		id       uint64
		prevName string
		prevInc  int64
		prevAddr string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, hostname, incarnation, remote_addr FROM hosts WHERE monit_id = ? FOR UPDATE`,
		r.MonitID).Scan(&id, &prevName, &prevInc, &prevAddr)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	hostname := trunc(r.Server.LocalHostname, 255)
	args := []any{
		hostname, r.Incarnation, trunc(r.Version, 32), r.Server.Poll, r.Server.Uptime,
		trunc(r.Server.ControlFile, 512),
		trunc(r.Platform.Name, 64), trunc(r.Platform.Release, 128), trunc(r.Platform.Version, 255),
		trunc(r.Platform.Machine, 32), r.Platform.CPU, r.Platform.MemoryKB, r.Platform.SwapKB,
		trunc(r.Server.HttpdAddress, 255), r.Server.HttpdPort, r.Server.HttpdSSL,
		trunc(remoteAddr, 45), len(r.Services), r.FailedCount(), now,
	}

	if !found {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO hosts
			  (hostname, incarnation, monit_version, poll_interval, monit_uptime, control_file,
			   os_name, os_release, os_version, machine, cpu_count, memory_kb, swap_kb,
			   httpd_address, httpd_port, httpd_ssl, remote_addr, services_total, services_failed,
			   last_seen, monit_id, first_seen)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			append(args, r.MonitID, now)...)
		if err != nil {
			return 0, err
		}
		lid, _ := res.LastInsertId()
		s.log.Info("nuovo host registrato", "host", hostname, "monit_id", r.MonitID, "remote", remoteAddr)
		return uint64(lid), nil
	}

	// Stesso monit_id ma hostname diverso: quasi sempre una VM clonata con lo
	// stesso ~/.monit.id. Due host che si "rubano" la stessa riga.
	if prevName != hostname {
		s.log.Warn("hostname cambiato per lo stesso monit_id (VM clonata? rigenerare .monit.id con 'monit -r')",
			"monit_id", r.MonitID, "prima", prevName, "ora", hostname,
			"remote_prima", prevAddr, "remote_ora", remoteAddr)
	}
	if prevInc != r.Incarnation {
		s.log.Info("monit riavviato", "host", hostname, "incarnation", r.Incarnation)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE hosts SET
		  hostname=?, incarnation=?, monit_version=?, poll_interval=?, monit_uptime=?, control_file=?,
		  os_name=?, os_release=?, os_version=?, machine=?, cpu_count=?, memory_kb=?, swap_kb=?,
		  httpd_address=?, httpd_port=?, httpd_ssl=?, remote_addr=?, services_total=?, services_failed=?,
		  last_seen=?
		WHERE id=?`, append(args, id)...)
	return id, err
}

const serviceCols = 12

func (s *Store) upsertServices(ctx context.Context, tx *sql.Tx, hostID uint64, svcs []Service, now time.Time) error {
	const chunk = 500
	for start := 0; start < len(svcs); start += chunk {
		end := min(start+chunk, len(svcs))
		part := svcs[start:end]

		var q strings.Builder
		q.WriteString(`INSERT INTO services
			(host_id, name, type, status, status_hint, monitor, monitor_mode, pending_action,
			 collected_at, status_changed_at, data, updated_at) VALUES `)
		args := make([]any, 0, len(part)*serviceCols)
		for i, sv := range part {
			if i > 0 {
				q.WriteByte(',')
			}
			q.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?)")
			data, err := json.Marshal(sv.Data)
			if err != nil {
				return fmt.Errorf("serializzazione %q: %w", sv.Name, err)
			}
			args = append(args, hostID, trunc(sv.Name, 255), sv.Type, sv.Status, sv.StatusHint,
				sv.Monitor, sv.MonitorMode, sv.PendingAction, nz(sv.CollectedAt, now), now,
				string(data), now)
		}
		// L'ordine conta: status_changed_at va valutato PRIMA di aggiornare status.
		q.WriteString(` ON DUPLICATE KEY UPDATE
			status_changed_at = IF(status <> VALUES(status), VALUES(status_changed_at), status_changed_at),
			type = VALUES(type), status = VALUES(status), status_hint = VALUES(status_hint),
			monitor = VALUES(monitor), monitor_mode = VALUES(monitor_mode),
			pending_action = VALUES(pending_action), collected_at = VALUES(collected_at),
			data = VALUES(data), updated_at = VALUES(updated_at)`)
		if _, err := tx.ExecContext(ctx, q.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// PurgeEvents cancella gli eventi più vecchi di retention, a blocchi per non
// tenere lock lunghi.
func (s *Store) PurgeEvents(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention)
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE collected_at < ? LIMIT 5000`, cutoff)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 5000 {
			return total, nil
		}
	}
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
