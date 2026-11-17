// Bmon collector: receives the reports that Monit agents send with
// `set mmonit http://user:password@host:port/collector` and stores them in MySQL/MariaDB.
// It speaks the M/Monit protocol: on the monitored hosts only the URL changes.
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

// Name and Version identify the program. Version can be overridden at build
// time with -ldflags "-X main.Version=0.1.0-dev+abc123".
const Name = "Bmon collector"

var Version = "0.8.0"

type config struct {
	listen         string
	dsn            string
	auth           []credential
	trustedProxies []*net.IPNet
	maxBody        int64
	maxConns       int
	dbTimeout      time.Duration
	maxInflight    int
	queueWait      time.Duration
	eventRetention time.Duration
	logLevel       string
	offline        OfflinePolicy
	offlineEvery   time.Duration
	storeCreds     bool
	credsKey       string
	slackWebhook   string
	slackMention   string
	slackKinds     map[Kind]bool
	slackTest      bool
	emergencyHosts int
	emergencyWin   time.Duration
	emergencyUpd   time.Duration
	alwaysGroups   []string
	officeGroups   []string
	ungrouped      Policy
	officeHours    *OfficeHours
}

type credential struct{ user, pass string }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func loadConfig() (*config, error) {
	c := &config{}
	var auth, proxies string
	var retentionDays int
	flag.StringVar(&c.listen, "listen", env("BMON_LISTEN", "127.0.0.1:8080"), "listen address (env BMON_LISTEN)")
	flag.StringVar(&c.dsn, "dsn", env("BMON_DSN", ""), "MySQL DSN, e.g. user:pass@tcp(127.0.0.1:3306)/monit (env BMON_DSN)")
	flag.StringVar(&auth, "auth", env("BMON_AUTH", ""), "accepted credentials, 'user:password[,user2:password2]' (env BMON_AUTH)")
	flag.StringVar(&proxies, "trusted-proxies", env("BMON_TRUSTED_PROXIES", "127.0.0.1/32,::1/128"),
		"CIDRs of reverse proxies trusted for X-Real-IP / X-Forwarded-For (env BMON_TRUSTED_PROXIES)")
	flag.Int64Var(&c.maxBody, "max-body", 16<<20, "maximum payload size in bytes")
	flag.IntVar(&c.maxConns, "db-conns", envInt("BMON_DB_CONNS", 20), "maximum database connections (env BMON_DB_CONNS)")
	flag.DurationVar(&c.dbTimeout, "db-timeout", envDur("BMON_DB_TIMEOUT", 3*time.Second),
		"time budget for storing a single report, retries included (env BMON_DB_TIMEOUT)")
	flag.IntVar(&c.maxInflight, "max-inflight", envInt("BMON_MAX_INFLIGHT", 0),
		"reports stored at the same time; 0 = same as -db-conns (env BMON_MAX_INFLIGHT)")
	flag.DurationVar(&c.queueWait, "queue-wait", envDur("BMON_QUEUE_WAIT", time.Second),
		"how long a report may wait for a free slot before getting 503 (env BMON_QUEUE_WAIT)")
	flag.IntVar(&retentionDays, "event-retention-days", 180, "days of event history to keep (0 = forever)")
	flag.IntVar(&c.offline.Cycles, "offline-cycles", envInt("BMON_OFFLINE_CYCLES", 3),
		"mark a host offline after this many poll cycles without a report (env BMON_OFFLINE_CYCLES)")
	flag.DurationVar(&c.offline.MinSilence, "offline-min", envDur("BMON_OFFLINE_MIN", 3*time.Minute),
		"minimum silence before a host is marked offline, regardless of its poll interval (env BMON_OFFLINE_MIN)")
	flag.DurationVar(&c.offlineEvery, "offline-check-every", 30*time.Second, "how often to check for silent hosts")
	flag.BoolVar(&c.storeCreds, "store-credentials", envBool("BMON_STORE_CREDENTIALS", true),
		"store the Monit httpd credentials sent in the reports, so that Bmon-web can send actions (env BMON_STORE_CREDENTIALS)")
	flag.StringVar(&c.credsKey, "credentials-key", env("BMON_CREDENTIALS_KEY", ""),
		"base64 32-byte key to encrypt the stored credentials; empty = stored in clear (env BMON_CREDENTIALS_KEY)")
	var slackNotify string
	flag.StringVar(&c.slackWebhook, "slack-webhook", env("BMON_SLACK_WEBHOOK", ""),
		"Slack incoming webhook URL; empty = Slack notifications disabled (env BMON_SLACK_WEBHOOK)")
	flag.StringVar(&c.slackMention, "slack-mention", env("BMON_SLACK_MENTION", "<!channel>"),
		"mention added to host down messages, e.g. <!channel>, <!here>, <@U012AB3CD>; empty = none (env BMON_SLACK_MENTION)")
	flag.StringVar(&slackNotify, "slack-notify", env("BMON_SLACK_NOTIFY", "all"),
		"notifications to send: all, or a list of service_failed,service_ok,monit_stopped,monit_started,host_down,host_up (env BMON_SLACK_NOTIFY)")
	flag.IntVar(&c.emergencyHosts, "emergency-hosts", envInt("BMON_EMERGENCY_HOSTS", 10),
		"hosts down within the emergency window that start an emergency; 0 = disabled (env BMON_EMERGENCY_HOSTS)")
	flag.DurationVar(&c.emergencyWin, "emergency-window", envDur("BMON_EMERGENCY_WINDOW", 5*time.Minute),
		"window for the emergency threshold (env BMON_EMERGENCY_WINDOW)")
	flag.DurationVar(&c.emergencyUpd, "emergency-update", envDur("BMON_EMERGENCY_UPDATE", 3*time.Minute),
		"minimum interval between emergency progress updates (env BMON_EMERGENCY_UPDATE)")
	var alwaysGroups, officeGroups, ungrouped, officeHours, tz string
	flag.StringVar(&alwaysGroups, "slack-always-groups", env("BMON_SLACK_ALWAYS_GROUPS", ""),
		"service groups notified at any time, e.g. H24 (env BMON_SLACK_ALWAYS_GROUPS)")
	flag.StringVar(&officeGroups, "slack-office-groups", env("BMON_SLACK_OFFICE_GROUPS", ""),
		"service groups notified only during office hours, e.g. workday (env BMON_SLACK_OFFICE_GROUPS)")
	flag.StringVar(&ungrouped, "slack-ungrouped", env("BMON_SLACK_UNGROUPED", "always"),
		"services in none of those groups: always, office or never (env BMON_SLACK_UNGROUPED)")
	flag.StringVar(&officeHours, "office-hours", env("BMON_OFFICE_HOURS", "mon-fri 08:00-13:00,14:00-18:00"),
		"office hours (env BMON_OFFICE_HOURS)")
	flag.StringVar(&tz, "timezone", env("BMON_TIMEZONE", "Europe/Rome"), "timezone of the office hours (env BMON_TIMEZONE)")
	flag.BoolVar(&c.slackTest, "slack-test", false, "send one sample message of each kind to Slack and exit")
	flag.StringVar(&c.logLevel, "log-level", env("BMON_LOG_LEVEL", "info"), "debug|info|warn|error")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if c.offline.Cycles < 1 {
		c.offline.Cycles = 1
	}
	if *showVersion {
		fmt.Printf("%s %s\n", Name, Version)
		os.Exit(0)
	}
	split := func(s string) []string {
		var out []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	c.alwaysGroups, c.officeGroups = split(alwaysGroups), split(officeGroups)
	pol, err := ParsePolicy(ungrouped)
	if err != nil {
		return nil, fmt.Errorf("slack-ungrouped: %w", err)
	}
	c.ungrouped = pol
	if c.officeHours, err = ParseOfficeHours(officeHours, tz); err != nil {
		return nil, err
	}
	kinds, err := ParseKinds(slackNotify)
	if err != nil {
		return nil, fmt.Errorf("slack-notify: %w", err)
	}
	c.slackKinds = kinds
	if c.slackWebhook != "" && !strings.HasPrefix(c.slackWebhook, "https://") &&
		!strings.HasPrefix(c.slackWebhook, "http://127.0.0.1") && !strings.HasPrefix(c.slackWebhook, "http://localhost") {
		return nil, errors.New("slack-webhook must be an https:// URL")
	}
	if c.slackTest {
		if c.slackWebhook == "" {
			return nil, errors.New("-slack-test needs a webhook (-slack-webhook or BMON_SLACK_WEBHOOK)")
		}
		return c, nil // no database needed
	}

	if c.dsn == "" {
		return nil, errors.New("missing DSN (-dsn or BMON_DSN)")
	}
	c.dsn = normalizeDSN(c.dsn)
	if auth == "" {
		return nil, errors.New("missing credentials (-auth or BMON_AUTH): a collector without authentication accepts data from anyone")
	}
	for _, p := range strings.Split(auth, ",") {
		u, pw, ok := strings.Cut(strings.TrimSpace(p), ":")
		if !ok || u == "" {
			return nil, fmt.Errorf("invalid credential %q (expected user:password)", p)
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
	if c.maxInflight <= 0 || c.maxInflight > c.maxConns {
		c.maxInflight = c.maxConns
	}
	return c, nil
}

// normalizeDSN enforces the parameters the code relies on (UTC times, utf8mb4).
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
	reports, events, rejected, failed, busy atomic.Int64
}

type server struct {
	cfg       *config
	store     *Store
	log       *slog.Logger
	stats     stats
	emergency *Emergency
	slots     chan struct{} // admission control: one token per report being stored
	inflight  atomic.Int64
	lastBusy  atomic.Int64 // unix seconds of the last "busy" log line
}

func (s *server) checkAuth(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	match := 0
	for _, c := range s.cfg.auth {
		// constant-time comparison on both fields
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
		user, _, hasAuth := r.BasicAuth() // the password is never logged
		if !hasAuth {
			user = "(no credentials)"
		}
		s.log.Warn("authentication failed", "remote", ip, "user", user, "ua", r.UserAgent())
		w.Header().Set("WWW-Authenticate", `Basic realm="bmon-collector"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.maxBody))
	if err != nil {
		s.stats.rejected.Add(1)
		s.log.Warn("failed to read payload", "remote", ip, "err", err)
		http.Error(w, "bad request", http.StatusRequestEntityTooLarge)
		return
	}
	rep, err := ParseReport(body)
	if err != nil {
		s.stats.rejected.Add(1)
		s.log.Warn("invalid payload", "remote", ip, "ua", r.UserAgent(), "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Admission control. Monit waits only a few seconds for an answer (the
	// mmonit timeout, 5 s by default): during a burst (all hosts reconnecting
	// after an outage, eventqueue backlogs) it is better to answer 503 at once
	// and let Monit retry on its next cycle than to pile up requests that Monit
	// has already given up on.
	wait := time.NewTimer(s.cfg.queueWait)
	select {
	case s.slots <- struct{}{}:
		wait.Stop()
	case <-wait.C:
		n := s.stats.busy.Add(1)
		if now := time.Now().Unix(); s.lastBusy.Swap(now) != now { // at most one line per second
			s.log.Warn("collector busy, report refused with 503 (Monit will retry)",
				"host", rep.Server.LocalHostname, "inflight", s.inflight.Load(), "refused_total", n)
		}
		w.Header().Set("Retry-After", "30")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	case <-r.Context().Done():
		wait.Stop()
		return
	}
	s.inflight.Add(1)
	defer func() { s.inflight.Add(-1); <-s.slots }()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.dbTimeout)
	defer cancel()
	start := time.Now()
	if err := s.store.SaveReport(ctx, rep, ip); err != nil {
		// 5xx: Monit treats the delivery as failed and, with an eventqueue, retries the events.
		s.stats.failed.Add(1)
		s.log.Error("failed to store report", "host", rep.Server.LocalHostname, "remote", ip, "err", err)
		http.Error(w, "storage error", http.StatusServiceUnavailable)
		return
	}
	s.stats.reports.Add(1)
	s.stats.events.Add(int64(len(rep.Events)))

	s.log.Debug("report stored", "host", rep.Server.LocalHostname, "remote", ip,
		"services", len(rep.Services), "failed", rep.FailedCount(), "events", len(rep.Events),
		"ms", time.Since(start).Milliseconds())
	for _, e := range rep.Events {
		s.log.Info("event", "host", rep.Server.LocalHostname, "service", e.Service,
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
	total, offline, _ := s.store.CountOffline(ctx)
	em := 0
	if s.emergency != nil && s.emergency.Active() {
		em = 1
	}
	fmt.Fprintf(w, "ok\nversion %s\nemergency %d\nhosts %d\nhosts_offline %d\nreports %d\nevents %d\nrejected %d\nstorage_errors %d\nbusy_503 %d\ninflight %d\n",
		Version, em, total, offline, s.stats.reports.Load(), s.stats.events.Load(), s.stats.rejected.Load(), s.stats.failed.Load(),
		s.stats.busy.Load(), s.inflight.Load())
}

func (s *server) offlineLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.offlineEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.store.CheckOffline(ctx, s.cfg.offline); err != nil && ctx.Err() == nil {
			s.log.Error("offline host check failed", "err", err)
		}
	}
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
			s.log.Error("event purge failed", "err", err)
		} else if n > 0 {
			s.log.Info("old events purged", "n", n)
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
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.slackTest {
		os.Exit(slackTest(ctx, cfg, log))
	}

	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	store, err := OpenStore(startCtx, cfg.dsn, cfg.maxConns, log)
	if err == nil {
		err = store.Migrate(startCtx)
	}
	cancel()
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	sealer, err := newCredentialSealer(cfg.credsKey)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	store.SetCredentials(cfg.storeCreds, sealer)
	switch {
	case !cfg.storeCreds:
		log.Info("Monit httpd credentials are not stored: Bmon-web cannot send actions")
	case sealer.Encrypted():
		log.Info("Monit httpd credentials are stored encrypted")
	default:
		log.Warn("Monit httpd credentials are stored in clear: set BMON_CREDENTIALS_KEY to encrypt them")
	}

	notifierDone := make(chan struct{})
	var slack *SlackNotifier
	if cfg.slackWebhook != "" {
		slack = NewSlackNotifier(cfg.slackWebhook, cfg.slackMention, cfg.slackKinds, log)
		go func() { slack.Run(ctx); close(notifierDone) }()
		var enabled []string
		for _, name := range kindOrder {
			for k, n := range kindNames {
				if n == name && cfg.slackKinds[k] {
					enabled = append(enabled, name)
					break
				}
			}
		}
		log.Info("Slack notifications enabled", "notify", strings.Join(enabled, ","), "mention", cfg.slackMention)
	} else {
		close(notifierDone)
		log.Info("Slack notifications disabled (BMON_SLACK_WEBHOOK not set)")
	}
	// The emergency detector runs even without Slack: it still logs emergencies.
	var inner Notifier
	if slack != nil {
		inner = slack
		// Per-group schedule (H24 / office hours), only if some group is configured.
		if len(cfg.alwaysGroups)+len(cfg.officeGroups) > 0 || cfg.ungrouped != PolicyAlways {
			sched := NewGroupSchedule(slack, cfg.alwaysGroups, cfg.officeGroups, cfg.ungrouped, cfg.officeHours, log)
			go sched.Run(ctx)
			inner = sched
			log.Info("Slack schedule by service group", "always", strings.Join(cfg.alwaysGroups, ","),
				"office", strings.Join(cfg.officeGroups, ","), "ungrouped", []string{"always", "office", "never"}[cfg.ungrouped],
				"office_hours", cfg.officeHours.String())
		}
	}
	emergency := NewEmergency(inner, cfg.emergencyHosts, cfg.emergencyWin, log)
	emergency.Update = cfg.emergencyUpd
	store.SetNotifier(emergency)
	go emergency.Run(ctx)
	if cfg.emergencyHosts > 0 {
		log.Info("emergency detection enabled", "hosts", cfg.emergencyHosts, "window", cfg.emergencyWin)
	}

	s := &server{cfg: cfg, store: store, log: log, emergency: emergency, slots: make(chan struct{}, cfg.maxInflight)}
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
	cfg.offline.Started = time.Now().UTC()
	go s.purgeLoop(ctx)
	go s.offlineLoop(ctx)
	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		shCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		srv.Shutdown(shCtx)
	}()

	log.Info(Name+" listening", "version", Version, "addr", cfg.listen, "credentials", len(cfg.auth),
		"event_retention_days", strconv.Itoa(int(cfg.eventRetention.Hours()/24)),
		"offline_after_cycles", cfg.offline.Cycles, "offline_min", cfg.offline.MinSilence,
		"max_inflight", cfg.maxInflight, "queue_wait", cfg.queueWait, "db_timeout", cfg.dbTimeout)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
	// Give the notifier the chance to deliver what is still queued.
	select {
	case <-notifierDone:
	case <-time.After(10 * time.Second):
	}
}

func slackTest(ctx context.Context, cfg *config, log *slog.Logger) int {
	// No mention in the test, so the whole channel is not pinged.
	slack := NewSlackNotifier(cfg.slackWebhook, "", cfg.slackKinds, log)
	var batch []Notification
	for _, n := range append(SampleNotifications(), SampleEmergency()...) {
		if cfg.slackKinds[n.Kind] {
			batch = append(batch, n)
		}
	}
	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := slack.SendNow(tctx, batch); err != nil {
		fmt.Fprintln(os.Stderr, "Slack test failed:", err)
		return 1
	}
	fmt.Printf("Slack test sent: %d sample notifications (without the %q mention).\n", len(batch), cfg.slackMention)
	return 0
}

func withServerHeader(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "bmon-collector/"+Version)
		h.ServeHTTP(w, r)
	})
}
