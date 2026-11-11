// Bmon collector: riceve i report che gli agent Monit inviano con
// `set mmonit http://utente:password@host:porta/collector` e li salva su MySQL/MariaDB.
// È compatibile con il protocollo di M/Monit: sugli host basta cambiare l'URL.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Name e Version identificano il programma. Version si può sovrascrivere in
// build con -ldflags "-X main.Version=0.1.0-dev+abc123".
const Name = "Bmon collector"

var Version = "0.1.0"

type config struct {
	listen         string
	dsn            string
	auth           []credential
	trustedProxies []*net.IPNet
	maxBody        int64
	maxConns       int
	dbTimeout      time.Duration
	eventRetention time.Duration
	logLevel       string
}

type credential struct{ user, pass string }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() (*config, error) {
	c := &config{}
	var auth, proxies string
	var retentionDays int
	flag.StringVar(&c.listen, "listen", env("BMON_LISTEN", "127.0.0.1:8080"), "indirizzo di ascolto (env BMON_LISTEN)")
	flag.StringVar(&c.dsn, "dsn", env("BMON_DSN", ""), "DSN MySQL, es. user:pass@tcp(127.0.0.1:3306)/monit (env BMON_DSN)")
	flag.StringVar(&auth, "auth", env("BMON_AUTH", ""), "credenziali accettate, 'utente:password[,utente2:password2]' (env BMON_AUTH)")
	flag.StringVar(&proxies, "trusted-proxies", env("BMON_TRUSTED_PROXIES", "127.0.0.1/32,::1/128"),
		"CIDR dei reverse proxy di cui fidarsi per X-Real-IP / X-Forwarded-For (env BMON_TRUSTED_PROXIES)")
	flag.Int64Var(&c.maxBody, "max-body", 16<<20, "dimensione massima del payload in byte")
	flag.IntVar(&c.maxConns, "db-conns", 20, "connessioni massime al database")
	flag.DurationVar(&c.dbTimeout, "db-timeout", 10*time.Second, "timeout per il salvataggio di un report")
	flag.IntVar(&retentionDays, "event-retention-days", 180, "giorni di storico eventi (0 = per sempre)")
	flag.StringVar(&c.logLevel, "log-level", env("BMON_LOG_LEVEL", "info"), "debug|info|warn|error")
	showVersion := flag.Bool("version", false, "stampa la versione ed esce")
	flag.Parse()
	if *showVersion {
		fmt.Printf("%s %s\n", Name, Version)
		os.Exit(0)
	}

	if c.dsn == "" {
		return nil, errors.New("manca il DSN (-dsn o BMON_DSN)")
	}
	c.dsn = normalizeDSN(c.dsn)
	if auth == "" {
		return nil, errors.New("mancano le credenziali (-auth o BMON_AUTH): un collector senza autenticazione accetta dati da chiunque")
	}
	for _, p := range strings.Split(auth, ",") {
		u, pw, ok := strings.Cut(strings.TrimSpace(p), ":")
		if !ok || u == "" {
			return nil, fmt.Errorf("credenziale non valida %q (atteso utente:password)", p)
		}
		c.auth = append(c.auth, credential{u, pw})
	}
	for _, p := range strings.Split(proxies, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("trusted-proxies: %w", err)
		}
		c.trustedProxies = append(c.trustedProxies, n)
	}
	c.eventRetention = time.Duration(retentionDays) * 24 * time.Hour
	return c, nil
}

// normalizeDSN forza i parametri che il codice presuppone (orari UTC, utf8mb4).
func normalizeDSN(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	for _, kv := range []string{"parseTime=true", "loc=UTC", "time_zone=%27%2B00%3A00%27", "charset=utf8mb4"} {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.Contains(dsn, key+"=") {
			dsn += sep + kv
			sep = "&"
		}
	}
	return dsn
}

type stats struct {
	reports, events, rejected, failed atomic.Int64
}

type server struct {
	cfg   *config
	store *Store
	log   *slog.Logger
	stats stats
}

func (s *server) checkAuth(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	match := 0
	for _, c := range s.cfg.auth {
		// confronto a tempo costante su entrambi i campi
		uok := subtle.ConstantTimeCompare([]byte(u), []byte(c.user))
		pok := subtle.ConstantTimeCompare([]byte(p), []byte(c.pass))
		match |= uok & pok
	}
	return match == 1
}

func (s *server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	trusted := false
	for _, n := range s.cfg.trustedProxies {
		if ip != nil && n.Contains(ip) {
			trusted = true
			break
		}
	}
	if !trusted {
		return host
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		parts := strings.Split(v, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	return host
}

func (s *server) handleCollector(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.checkAuth(r) {
		s.stats.rejected.Add(1)
		s.log.Warn("autenticazione fallita", "remote", ip, "ua", r.UserAgent())
		w.Header().Set("WWW-Authenticate", `Basic realm="bmon-collector"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.maxBody))
	if err != nil {
		s.stats.rejected.Add(1)
		s.log.Warn("lettura payload fallita", "remote", ip, "err", err)
		http.Error(w, "bad request", http.StatusRequestEntityTooLarge)
		return
	}
	rep, err := ParseReport(body)
	if err != nil {
		s.stats.rejected.Add(1)
		s.log.Warn("payload non valido", "remote", ip, "ua", r.UserAgent(), "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.dbTimeout)
	defer cancel()
	start := time.Now()
	if err := s.store.SaveReport(ctx, rep, ip); err != nil {
		// 5xx: Monit considera fallito l'invio e, se ha la eventqueue, ritenta gli eventi.
		s.stats.failed.Add(1)
		s.log.Error("salvataggio fallito", "host", rep.Server.LocalHostname, "remote", ip, "err", err)
		http.Error(w, "storage error", http.StatusServiceUnavailable)
		return
	}
	s.stats.reports.Add(1)
	s.stats.events.Add(int64(len(rep.Events)))

	s.log.Debug("report salvato", "host", rep.Server.LocalHostname, "remote", ip,
		"services", len(rep.Services), "failed", rep.FailedCount(), "events", len(rep.Events),
		"ms", time.Since(start).Milliseconds())
	for _, e := range rep.Events {
		s.log.Info("evento", "host", rep.Server.LocalHostname, "service", e.Service,
			"state", e.State, "event_id", e.ID, "msg", e.Message)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "db down: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "ok\nversion %s\nreports %d\nevents %d\nrejected %d\nstorage_errors %d\n",
		Version, s.stats.reports.Load(), s.stats.events.Load(), s.stats.rejected.Load(), s.stats.failed.Load())
}

func (s *server) purgeLoop(ctx context.Context) {
	if s.cfg.eventRetention <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := s.store.PurgeEvents(ctx, s.cfg.eventRetention)
		if err != nil && ctx.Err() == nil {
			s.log.Error("pulizia eventi fallita", "err", err)
		} else if n > 0 {
			s.log.Info("eventi vecchi eliminati", "n", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "errore:", err)
		os.Exit(2)
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	store, err := OpenStore(startCtx, cfg.dsn, cfg.maxConns, log)
	if err == nil {
		err = store.Migrate(startCtx)
	}
	cancel()
	if err != nil {
		log.Error("avvio fallito", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	s := &server{cfg: cfg, store: store, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("/collector", s.handleCollector)
	mux.HandleFunc("/healthz", s.handleHealth)

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           withServerHeader(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	go s.purgeLoop(ctx)
	go func() {
		<-ctx.Done()
		log.Info("arresto in corso")
		shCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		srv.Shutdown(shCtx)
	}()

	log.Info(Name+" in ascolto", "versione", Version, "addr", cfg.listen, "credenziali", len(cfg.auth),
		"retention_eventi_giorni", strconv.Itoa(int(cfg.eventRetention.Hours()/24)))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server terminato", "err", err)
		os.Exit(1)
	}
}

func withServerHeader(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "bmon-collector/"+Version)
		h.ServeHTTP(w, r)
	})
}
