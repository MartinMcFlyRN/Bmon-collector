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
	// Colonne aggiunte dopo la 0.1.0 su database già esistenti.
	if err := s.ensureColumn(ctx, "hosts", "offline_since", "DATETIME(3) NULL DEFAULT NULL AFTER last_seen"); err != nil {
		return fmt.Errorf("migrazione hosts.offline_since: %w", err)
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

	hostID, offlineSince, err := s.upsertHost(ctx, tx, r, remoteAddr, now)
	if err != nil {
		return fmt.Errorf("host: %w", err)
	}
	if offlineSince != nil {
		msg := fmt.Sprintf("Monit ha ripreso a inviare report dopo %s di silenzio", fmtDur(now.Sub(*offlineSince)))
		if err := insertBmonEvent(ctx, tx, hostID, StateSucceeded, msg, now); err != nil {
			return fmt.Errorf("evento online: %w", err)
		}
	}
	if err := s.upsertServices(ctx, tx, hostID, r.Services, now); err != nil {
		return fmt.Errorf("servizi: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM services WHERE host_id = ? AND updated_at <> ?`, hostID, now); err != nil {
		return fmt.Errorf("pulizia servizi: %w", err)
	}
	if err := s.replaceGroups(ctx, tx, hostID, r.Groups); err != nil {
		return fmt.Errorf("gruppi: %w", err)
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
	if err := tx.Commit(); err != nil {
		return err
	}
	if offlineSince != nil {
		s.log.Info("host di nuovo online", "host", r.Server.LocalHostname, "remote", remoteAddr,
			"offline_da", offlineSince.Format(time.RFC3339), "durata", fmtDur(now.Sub(*offlineSince)))
	}
	return nil
}

// upsertHost restituisce anche offline_since se l'host era segnato offline
// (e con questo report torna online).
func (s *Store) upsertHost(ctx context.Context, tx *sql.Tx, r *Report, remoteAddr string, now time.Time) (uint64, *time.Time, error) {
	var (
		id       uint64
		prevName string
		prevInc  int64
		prevAddr string
		offline  sql.NullTime
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, hostname, incarnation, remote_addr, offline_since FROM hosts WHERE monit_id = ? FOR UPDATE`,
		r.MonitID).Scan(&id, &prevName, &prevInc, &prevAddr, &offline)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, err
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
			return 0, nil, err
		}
		lid, _ := res.LastInsertId()
		s.log.Info("nuovo host registrato", "host", hostname, "monit_id", r.MonitID, "remote", remoteAddr)
		return uint64(lid), nil, nil
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
		  last_seen=?, offline_since=NULL
		WHERE id=?`, append(args, id)...)
	if err != nil {
		return 0, nil, err
	}
	if offline.Valid {
		t := offline.Time.UTC()
		return id, &t, nil
	}
	return id, nil, nil
}

// Eventi generati da Bmon stesso (non da Monit): service = "Monit", event_id = 0.
const (
	BmonEventService = "Monit"
	BmonEventID      = 0
	StateSucceeded   = 0
	StateFailed      = 1
)

func insertBmonEvent(ctx context.Context, ex interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, hostID uint64, state int, msg string, now time.Time) error {
	_, err := ex.ExecContext(ctx, `
		INSERT IGNORE INTO events
		  (host_id, service, service_type, event_id, state, action, message, collected_at, received_at)
		VALUES (?, ?, 5, ?, ?, 1, ?, ?, ?)`,
		hostID, BmonEventService, BmonEventID, state, msg, now, now)
	return err
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h >= 24 {
		return fmt.Sprintf("%dg %dh %dm", h/24, h%24, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm %ds", m, int(d.Seconds())%60)
}

// OfflinePolicy: un host è offline se non manda report da più di
// max(MinSilence, Cycles × poll_interval). Prima che il collector sia attivo
// da altrettanto tempo non si segnala niente (dopo un riavvio del collector
// tutti gli host risulterebbero "vecchi").
type OfflinePolicy struct {
	Cycles     int
	MinSilence time.Duration
	Started    time.Time
}

func (p OfflinePolicy) threshold(poll int64) time.Duration {
	t := time.Duration(int64(p.Cycles)*poll) * time.Second
	if t < p.MinSilence {
		t = p.MinSilence
	}
	return t
}

// CheckOffline segna offline gli host silenziosi e restituisce quanti ne ha
// segnati in questo giro.
func (s *Store) CheckOffline(ctx context.Context, p OfflinePolicy) (int, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, hostname, remote_addr, poll_interval, last_seen
		FROM hosts WHERE offline_since IS NULL AND last_seen < ?`, now.Add(-p.MinSilence))
	if err != nil {
		return 0, err
	}
	type cand struct {
		id       uint64
		name     string
		addr     string
		poll     int64
		lastSeen time.Time
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.name, &c.addr, &c.poll, &c.lastSeen); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	marked := 0
	for _, c := range cands {
		thr := p.threshold(c.poll)
		ref := c.lastSeen
		if p.Started.After(ref) {
			ref = p.Started
		}
		if now.Sub(ref) < thr {
			continue
		}
		// La condizione su last_seen evita di segnare offline un host il cui
		// report è arrivato tra la SELECT e questo UPDATE.
		res, err := s.db.ExecContext(ctx,
			`UPDATE hosts SET offline_since = ? WHERE id = ? AND offline_since IS NULL AND last_seen = ?`,
			c.lastSeen, c.id, c.lastSeen)
		if err != nil {
			return marked, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		silence := now.Sub(c.lastSeen)
		msg := fmt.Sprintf("Nessun report da Monit da %s (ultimo report %s UTC, soglia %s)",
			fmtDur(silence), c.lastSeen.Format("2006-01-02 15:04:05"), fmtDur(thr))
		if err := insertBmonEvent(ctx, s.db, c.id, StateFailed, msg, now); err != nil {
			return marked, err
		}
		marked++
		s.log.Warn("host offline: nessun report da Monit", "host", c.name, "remote", c.addr,
			"ultimo_report", c.lastSeen.Format(time.RFC3339), "silenzio", fmtDur(silence), "soglia", fmtDur(thr))
	}
	return marked, nil
}

func (s *Store) CountOffline(ctx context.Context) (total, offline int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(offline_since IS NOT NULL), 0) FROM hosts`).Scan(&total, &offline)
	return
}

// ensureColumn aggiunge una colonna se manca (MySQL non ha ADD COLUMN IF NOT EXISTS).
func (s *Store) ensureColumn(ctx context.Context, table, column, def string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	s.log.Info("aggiornamento schema", "tabella", table, "colonna", column)
	_, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` %s", table, column, def))
	return err
}

// replaceGroups riscrive i gruppi dell'host. Sono poche righe per host,
// quindi delete+insert è più semplice (e altrettanto economico) di un diff.
func (s *Store) replaceGroups(ctx context.Context, tx *sql.Tx, hostID uint64, groups map[string][]string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM service_groups WHERE host_id = ?`, hostID); err != nil {
		return err
	}
	var q strings.Builder
	var args []any
	for g, svcs := range groups {
		for _, sv := range svcs {
			if len(args) == 0 {
				q.WriteString(`INSERT IGNORE INTO service_groups (host_id, group_name, service) VALUES (?,?,?)`)
			} else {
				q.WriteString(`,(?,?,?)`)
			}
			args = append(args, hostID, trunc(g, 255), trunc(sv, 255))
		}
	}
	if len(args) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, q.String(), args...)
	return err
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
