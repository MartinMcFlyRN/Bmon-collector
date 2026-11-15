package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Kind is the type of a notification.
type Kind int

const (
	KindServiceFailed Kind = iota
	KindServiceOK
	KindMonitStopped
	KindMonitStarted
	KindHostDown
	KindHostUp
	KindEmergency       // many hosts down in a short time
	KindEmergencyUpdate // progress while the emergency lasts
	KindEmergencyOver   // emergency finished
	KindHostsUp         // many hosts back up at once, outside an emergency
)

var kindNames = map[Kind]string{
	KindServiceFailed:   "service_failed",
	KindServiceOK:       "service_ok",
	KindMonitStopped:    "monit_stopped",
	KindMonitStarted:    "monit_started",
	KindHostDown:        "host_down",
	KindHostUp:          "host_up",
	KindHostsUp:         "host_up",
	KindEmergency:       "emergency",
	KindEmergencyUpdate: "emergency",
	KindEmergencyOver:   "emergency",
}

// kindOrder lists the configurable names, in display order.
var kindOrder = []string{"service_failed", "service_ok", "monit_stopped", "monit_started", "host_down", "host_up", "emergency"}

func (k Kind) String() string { return kindNames[k] }

// Problem reports whether the notification is about something going wrong
// (red) rather than something getting back to normal (green).
func (k Kind) Problem() bool {
	return k == KindServiceFailed || k == KindMonitStopped || k == KindHostDown ||
		k == KindEmergency || k == KindEmergencyUpdate
}

// ParseKinds parses a comma-separated list of kind names; "all" or "" means every kind.
func ParseKinds(s string) (map[Kind]bool, error) {
	out := map[Kind]bool{}
	s = strings.TrimSpace(s)
	if s == "" || s == "all" {
		for k := range kindNames {
			out[k] = true
		}
		return out, nil
	}
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		found := false
		for k, n := range kindNames {
			if n == name {
				out[k] = true
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown notification kind %q (valid: %s, all)", name, strings.Join(kindOrder, ", "))
		}
	}
	return out, nil
}

// Notification is a single thing worth telling a human about.
type Notification struct {
	Kind        Kind
	Host        string
	Remote      string
	Service     string
	ServiceType int64
	EventID     int64
	Message     string
	Time        time.Time
	LastReport  time.Time     // host down: last report received
	Duration    time.Duration // host down: silence so far; host up: outage length
	Threshold   time.Duration // host down: offline threshold; emergency: window

	// emergency / hosts up
	Hosts      []string // hosts currently down (emergency) or back up (hosts up)
	Count      int      // len(Hosts)
	Involved   int      // hosts that went down during the emergency
	Recovered  int      // hosts back up during the emergency
	Suppressed int      // service/Monit notifications not sent
	Previous   int      // hosts down in the previous emergency message
}

// Notifier receives notifications. Implementations must not block.
type Notifier interface {
	Notify(Notification)
}

// Monit event types (Event_* in Monit's event.h) used for classification and display.
const (
	EventInstance = 0x10000
	EventAction   = 0x20000
)

// Monit actions (Action_* in Monit) carried by instance events.
const (
	ActionStop  = 3
	ActionStart = 6
)

var eventNames = map[int64]string{
	0x1: "Checksum", 0x2: "Resource", 0x4: "Timeout", 0x8: "Timestamp", 0x10: "Size",
	0x20: "Connection", 0x40: "Permission", 0x80: "UID", 0x100: "GID",
	0x200: "Does not exist", 0x400: "Invalid type", 0x800: "Data access",
	0x1000: "Execution", 0x2000: "Filesystem flags", 0x4000: "ICMP", 0x8000: "Content",
	0x10000: "Monit instance", 0x20000: "Action", 0x40000: "PID", 0x80000: "PPID",
	0x100000: "Heartbeat", 0x200000: "Status", 0x400000: "Uptime", 0x800000: "Link",
	0x1000000: "Speed", 0x2000000: "Saturation", 0x4000000: "Download bytes",
	0x8000000: "Upload bytes", 0x10000000: "Download packets", 0x20000000: "Upload packets",
	0x40000000: "Exists",
}

func eventName(id int64) string {
	if n, ok := eventNames[id]; ok {
		return n
	}
	return "event 0x" + strconv.FormatInt(id, 16)
}

// classifyEvent decides whether a Monit event becomes a notification.
// prevStatus is the service status stored before this report (known=false
// for a service seen for the first time).
//
// After every start or reload Monit re-sends a "failed" event for each
// service that is still failing. To avoid flooding the channel on each
// reload, a failure is notified only when that specific failure bit was not
// already set on the service.
func classifyEvent(e Event, prevStatus int64, known bool) (Kind, bool) {
	switch e.ID {
	case EventInstance:
		switch {
		case e.Action == ActionStop:
			return KindMonitStopped, true
		case e.Action == ActionStart && !strings.Contains(strings.ToLower(e.Message), "reload"):
			return KindMonitStarted, true
		}
		return 0, false // reload and other instance events
	case EventAction:
		return 0, false // "restart action done" and similar: user-triggered
	}
	switch e.State {
	case StateFailed, StateChanged:
		if known && prevStatus&e.ID != 0 {
			return 0, false // already failing for this reason
		}
		return KindServiceFailed, true
	case StateSucceeded, StateChangedNot:
		return KindServiceOK, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Slack

const (
	slackRed   = "#E01E5A"
	slackGreen = "#2EB67D"

	slackQueueSize = 2000
	slackBatchMax  = 10              // attachments per message
	slackBatchWait = 2 * time.Second // collect bursts into one message
	slackInterval  = 1100 * time.Millisecond
)

// SlackNotifier posts notifications to a Slack incoming webhook. Notify never
// blocks: messages are queued and sent by Run, batching bursts and respecting
// Slack's rate limits.
type SlackNotifier struct {
	url     string
	mention string
	kinds   map[Kind]bool
	queue   chan Notification
	client  *http.Client
	log     *slog.Logger
	dropped atomic.Int64
	sent    atomic.Int64
	failed  atomic.Int64
}

func NewSlackNotifier(url, mention string, kinds map[Kind]bool, log *slog.Logger) *SlackNotifier {
	return &SlackNotifier{
		url:     url,
		mention: strings.TrimSpace(mention),
		kinds:   kinds,
		queue:   make(chan Notification, slackQueueSize),
		client:  &http.Client{Timeout: 15 * time.Second},
		log:     log,
	}
}

func (s *SlackNotifier) Notify(n Notification) {
	if !s.kinds[n.Kind] {
		return
	}
	select {
	case s.queue <- n:
	default:
		if s.dropped.Add(1)%100 == 1 {
			s.log.Error("slack queue full, dropping notifications", "dropped_total", s.dropped.Load())
		}
	}
}

// Run sends queued notifications until ctx is done, then flushes what is left
// for a few seconds.
func (s *SlackNotifier) Run(ctx context.Context) {
	for {
		var first Notification
		select {
		case first = <-s.queue:
		case <-ctx.Done():
			s.flush()
			return
		}
		batch := []Notification{first}
		timer := time.NewTimer(slackBatchWait)
	collect:
		for len(batch) < slackBatchMax {
			select {
			case n := <-s.queue:
				batch = append(batch, n)
			case <-timer.C:
				break collect
			case <-ctx.Done():
				break collect
			}
		}
		timer.Stop()
		s.send(context.Background(), batch)
		select {
		case <-time.After(slackInterval):
		case <-ctx.Done():
		}
	}
}

func (s *SlackNotifier) flush() {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var batch []Notification
	drain:
		for len(batch) < slackBatchMax {
			select {
			case n := <-s.queue:
				batch = append(batch, n)
			default:
				break drain
			}
		}
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		s.send(ctx, batch)
		cancel()
	}
}

// SendNow sends a batch synchronously (used by -slack-test).
func (s *SlackNotifier) SendNow(ctx context.Context, batch []Notification) error {
	return s.send(ctx, batch)
}

func (s *SlackNotifier) send(ctx context.Context, batch []Notification) error {
	payload := s.buildPayload(batch, true)
	err := s.post(ctx, payload)
	var se *slackError
	if errors.As(err, &se) && se.status == http.StatusBadRequest {
		// Rejected layout: retry once with plain attachments so the alert is not lost.
		s.log.Warn("slack rejected the message layout, retrying as plain text", "err", err)
		err = s.post(ctx, s.buildPayload(batch, false))
	}
	if err != nil {
		s.failed.Add(int64(len(batch)))
		s.log.Error("slack notification failed", "notifications", len(batch), "err", err)
		return err
	}
	s.sent.Add(int64(len(batch)))
	s.log.Debug("slack notification sent", "notifications", len(batch))
	return nil
}

type slackError struct {
	status int
	body   string
}

func (e *slackError) Error() string { return fmt.Sprintf("slack returned %d: %s", e.status, e.body) }

func (s *SlackNotifier) post(ctx context.Context, payload []byte) error {
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("User-Agent", "bmon-collector/"+Version)
		resp, err := s.client.Do(req)
		var wait time.Duration
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusOK:
				return nil
			case resp.StatusCode == http.StatusTooManyRequests:
				wait = 30 * time.Second
				if ra, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && ra > 0 {
					wait = time.Duration(ra) * time.Second
				}
				err = &slackError{resp.StatusCode, strings.TrimSpace(string(body))}
			case resp.StatusCode >= 500:
				err = &slackError{resp.StatusCode, strings.TrimSpace(string(body))}
			default:
				// 400 invalid_payload, 403 invalid_token, 404 no_service...: retrying will not help
				return &slackError{resp.StatusCode, strings.TrimSpace(string(body))}
			}
		}
		if attempt >= len(backoff) {
			return err
		}
		if wait == 0 {
			wait = backoff[attempt]
		}
		s.log.Warn("slack delivery failed, retrying", "in", wait, "err", err)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// slackEscape escapes the three characters Slack treats as control characters.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func slackDate(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return fmt.Sprintf("<!date^%d^{date_short_pretty} {time_secs}|%s>",
		t.Unix(), t.UTC().Format("2006-01-02 15:04:05 UTC"))
}

func quote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "\n>" + strings.ReplaceAll(slackEscape(s), "\n", "\n>")
}

// hostList formats host names, keeping the text well below Slack's 3000
// characters per section.
func hostList(hosts []string) string {
	const max = 2200
	var b strings.Builder
	for i, h := range hosts {
		item := slackEscape(h)
		if i > 0 {
			item = ", " + item
		}
		if b.Len()+len(item) > max {
			fmt.Fprintf(&b, " … and %d more", len(hosts)-i)
			break
		}
		b.WriteString(item)
	}
	if b.Len() == 0 {
		return "—"
	}
	return b.String()
}

// render returns the main text, the context line and the plain fallback of a notification.
func render(n Notification) (main, context, fallback string) {
	host := "*" + slackEscape(n.Host) + "*"
	svc := "`" + slackEscape(n.Service) + "`"
	typ := ServiceTypeNames[n.ServiceType]
	if typ != "" {
		typ = strings.ToUpper(typ[:1]) + typ[1:]
	}
	ctx := []string{}
	switch n.Kind {
	case KindServiceFailed:
		main = ":red_circle: *Service failed* · " + svc + " on " + host + quote(n.Message)
		ctx = append(ctx, typ, eventName(n.EventID))
		fallback = fmt.Sprintf("FAILED %s on %s: %s", n.Service, n.Host, n.Message)
	case KindServiceOK:
		main = ":large_green_circle: *Service OK* · " + svc + " on " + host + quote(n.Message)
		ctx = append(ctx, typ, eventName(n.EventID))
		fallback = fmt.Sprintf("OK %s on %s: %s", n.Service, n.Host, n.Message)
	case KindMonitStopped:
		main = ":octagonal_sign: *Monit stopped* on " + host + quote(n.Message)
		fallback = fmt.Sprintf("Monit stopped on %s", n.Host)
	case KindMonitStarted:
		main = ":arrow_forward: *Monit started* on " + host + quote(n.Message)
		fallback = fmt.Sprintf("Monit started on %s", n.Host)
	case KindHostDown:
		main = fmt.Sprintf(":rotating_light: *HOST DOWN* · %s\nNo report from Monit for *%s* (threshold %s).",
			host, fmtDur(n.Duration), fmtDur(n.Threshold))
		ctx = append(ctx, "Last report "+slackDate(n.LastReport))
		fallback = fmt.Sprintf("HOST DOWN %s: no report for %s", n.Host, fmtDur(n.Duration))
	case KindHostUp:
		main = fmt.Sprintf(":white_check_mark: *HOST UP* · %s\nMonit is reporting again after *%s*.", host, fmtDur(n.Duration))
		fallback = fmt.Sprintf("HOST UP %s after %s", n.Host, fmtDur(n.Duration))
	case KindEmergency:
		main = fmt.Sprintf(":rotating_light: *EMERGENCY: %d hosts down* within %s.\nSingle host notifications are paused; updates will follow.\n*Down:* %s",
			n.Count, fmtDur(n.Threshold), hostList(n.Hosts))
		fallback = fmt.Sprintf("EMERGENCY: %d hosts down", n.Count)
	case KindEmergencyUpdate:
		trend := "no change"
		switch {
		case n.Count > n.Previous:
			trend = fmt.Sprintf("+%d since the last update", n.Count-n.Previous)
		case n.Count < n.Previous:
			trend = fmt.Sprintf("−%d since the last update", n.Previous-n.Count)
		}
		main = fmt.Sprintf(":rotating_light: *EMERGENCY ongoing* for %s: *%d hosts down* (%s).\n%d of %d back up · %d notifications suppressed\n*Still down:* %s",
			fmtDur(n.Duration), n.Count, trend, n.Recovered, n.Involved, n.Suppressed, hostList(n.Hosts))
		fallback = fmt.Sprintf("EMERGENCY ongoing: %d hosts down", n.Count)
	case KindEmergencyOver:
		main = fmt.Sprintf(":white_check_mark: *EMERGENCY OVER* after %s.\n%d of %d hosts back up · %d notifications suppressed (check Bmon-web for the services)",
			fmtDur(n.Duration), n.Recovered, n.Involved, n.Suppressed)
		if n.Count > 0 {
			main += fmt.Sprintf("\n*Still down (%d):* %s", n.Count, hostList(n.Hosts))
		}
		fallback = fmt.Sprintf("EMERGENCY OVER: %d of %d hosts back up", n.Recovered, n.Involved)
	case KindHostsUp:
		main = fmt.Sprintf(":white_check_mark: *%d HOSTS UP*: Monit is reporting again.\n%s", n.Count, hostList(n.Hosts))
		fallback = fmt.Sprintf("%d hosts back up", n.Count)
	}
	if n.Remote != "" {
		ctx = append(ctx, slackEscape(n.Remote))
	}
	if n.Kind != KindHostDown {
		ctx = append(ctx, slackDate(n.Time))
	}
	if n.Kind == KindEmergencyOver && n.Count > 0 {
		ctx = append(ctx, "hosts still down will be notified one by one when they come back")
	}
	var parts []string
	for _, c := range ctx {
		if c != "" {
			parts = append(parts, c)
		}
	}
	return main, strings.Join(parts, " · "), fallback
}

func (s *SlackNotifier) buildPayload(batch []Notification, withBlocks bool) []byte {
	type text struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type block struct {
		Type     string `json:"type"`
		Text     *text  `json:"text,omitempty"`
		Elements []text `json:"elements,omitempty"`
	}
	type attachment struct {
		Color    string   `json:"color"`
		Fallback string   `json:"fallback"`
		Blocks   []block  `json:"blocks,omitempty"`
		Text     string   `json:"text,omitempty"`
		MrkdwnIn []string `json:"mrkdwn_in,omitempty"`
	}
	payload := struct {
		Text        string       `json:"text,omitempty"`
		Attachments []attachment `json:"attachments"`
	}{}

	var down, up, head []string
	for _, n := range batch {
		main, ctx, fb := render(n)
		a := attachment{Color: slackGreen, Fallback: fb}
		if n.Kind.Problem() {
			a.Color = slackRed
		}
		if withBlocks {
			a.Blocks = []block{{Type: "section", Text: &text{"mrkdwn", main}}}
			if ctx != "" {
				a.Blocks = append(a.Blocks, block{Type: "context", Elements: []text{{"mrkdwn", ctx}}})
			}
		} else {
			a.Text = main
			if ctx != "" {
				a.Text += "\n" + ctx
			}
			a.MrkdwnIn = []string{"text"}
		}
		payload.Attachments = append(payload.Attachments, a)
		switch n.Kind {
		case KindHostDown:
			down = append(down, slackEscape(n.Host))
		case KindHostUp:
			up = append(up, slackEscape(n.Host))
		case KindEmergency:
			h := fmt.Sprintf(":rotating_light::rotating_light: *EMERGENCY: %d HOSTS DOWN* :rotating_light::rotating_light:", n.Count)
			if s.mention != "" {
				h = s.mention + " " + h
			}
			head = append(head, h)
		case KindEmergencyUpdate:
			head = append(head, fmt.Sprintf(":rotating_light: *EMERGENCY ongoing: %d hosts down*", n.Count))
		case KindEmergencyOver:
			head = append(head, fmt.Sprintf(":white_check_mark: *EMERGENCY OVER: %d of %d hosts back up*", n.Recovered, n.Involved))
		case KindHostsUp:
			head = append(head, fmt.Sprintf(":white_check_mark: *%d HOSTS UP*", n.Count))
		}
	}

	// Host down/up and emergencies also get a headline above the attachments:
	// it is always visible (attachments may be collapsed) and carries the mention.
	if len(down) > 0 {
		h := ":rotating_light: *HOST DOWN: " + strings.Join(down, ", ") + "*"
		if s.mention != "" {
			h = s.mention + " " + h
		}
		head = append(head, h)
	}
	if len(up) > 0 {
		head = append(head, ":white_check_mark: *HOST UP: "+strings.Join(up, ", ")+"*")
	}
	payload.Text = strings.Join(head, "\n")

	b, _ := json.Marshal(payload)
	return b
}

// SampleNotifications returns one notification of each kind, for -slack-test.
func SampleNotifications() []Notification {
	now := time.Now()
	return []Notification{
		{Kind: KindServiceFailed, Host: "web01.example.com", Remote: "203.0.113.10", Service: "nginx",
			ServiceType: 3, EventID: 0x200, Message: "process is not running", Time: now},
		{Kind: KindServiceOK, Host: "web01.example.com", Remote: "203.0.113.10", Service: "nginx",
			ServiceType: 3, EventID: 0x200, Message: "process is running with pid 1234", Time: now},
		{Kind: KindMonitStopped, Host: "db01.example.com", Remote: "203.0.113.20", Service: "Monit",
			ServiceType: 5, EventID: EventInstance, Message: "Monit 5.33.0 stopped", Time: now},
		{Kind: KindMonitStarted, Host: "db01.example.com", Remote: "203.0.113.20", Service: "Monit",
			ServiceType: 5, EventID: EventInstance, Message: "Monit 5.33.0 started", Time: now},
		{Kind: KindHostDown, Host: "mail01.example.com", Remote: "203.0.113.30",
			LastReport: now.Add(-200 * time.Second), Duration: 200 * time.Second, Threshold: 3 * time.Minute, Time: now},
		{Kind: KindHostUp, Host: "mail01.example.com", Remote: "203.0.113.30", Duration: 754 * time.Second, Time: now},
	}
}

// SampleEmergency returns the messages of an emergency, for -slack-test.
func SampleEmergency() []Notification {
	now := time.Now()
	var hosts []string
	for i := 1; i <= 120; i++ {
		hosts = append(hosts, fmt.Sprintf("srv%03d.example.com", i))
	}
	return []Notification{
		{Kind: KindEmergency, Time: now, Hosts: hosts, Count: 120, Involved: 120, Threshold: 5 * time.Minute},
		{Kind: KindEmergencyUpdate, Time: now, Hosts: hosts[:35], Count: 35, Previous: 120, Involved: 120,
			Recovered: 85, Suppressed: 412, Duration: 9 * time.Minute},
		{Kind: KindEmergencyOver, Time: now, Hosts: hosts[:3], Count: 3, Involved: 120, Recovered: 117,
			Suppressed: 530, Duration: 17 * time.Minute},
	}
}
