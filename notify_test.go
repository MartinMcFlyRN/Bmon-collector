package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyEvent(t *testing.T) {
	cases := []struct {
		name   string
		e      Event
		prev   int64
		known  bool
		init   bool
		want   Kind
		notify bool
	}{
		{"monit started", Event{ID: EventInstance, Action: ActionStart, State: 2, Message: "Monit 5.33.0 started"}, 0, false, false, KindMonitStarted, true},
		{"monit stopped", Event{ID: EventInstance, Action: ActionStop, State: 2, Message: "Monit 5.33.0 stopped"}, 0, false, false, KindMonitStopped, true},
		{"monit reloaded", Event{ID: EventInstance, Action: ActionStart, State: 2, Message: "Monit reloaded"}, 0, false, false, 0, false},
		{"action done", Event{ID: EventAction, State: 2, Message: "restart action done"}, 0, true, false, 0, false},
		{"new failure", Event{ID: 0x200, State: StateFailed}, 0, true, false, KindServiceFailed, true},
		{"failure on new service", Event{ID: 0x200, State: StateFailed}, 0, false, true, KindServiceFailed, true},
		{"re-sent after start/reload", Event{ID: 0x200, State: StateFailed}, 0x200, true, true, 0, false},
		// loadavg (1min) failing while loadavg (5min) is still failing: both are Resource (0x2)
		{"same type, other rule failing", Event{ID: 0x2, State: StateFailed}, 0x2, true, false, KindServiceFailed, true},
		{"different failure while initializing", Event{ID: 0x20, State: StateFailed}, 0x200, true, true, KindServiceFailed, true},
		{"changed", Event{ID: 0x1, State: StateChanged}, 0, true, false, KindServiceFailed, true},
		{"recovered", Event{ID: 0x200, State: StateSucceeded}, 0x200, true, false, KindServiceOK, true},
		{"changed not", Event{ID: 0x1, State: StateChangedNot}, 0x1, true, false, KindServiceOK, true},
		{"init", Event{ID: 0x200, State: 4}, 0, true, false, 0, false},
	}
	for _, c := range cases {
		k, ok := classifyEvent(c.e, c.prev, c.known, c.init)
		if ok != c.notify || (ok && k != c.want) {
			t.Errorf("%s: got %v/%v, want %v/%v", c.name, k, ok, c.want, c.notify)
		}
	}
}

func TestParseKinds(t *testing.T) {
	k, err := ParseKinds("host_down, host_up")
	if err != nil || !k[KindHostDown] || !k[KindHostUp] || !k[KindHostsUp] || k[KindServiceFailed] || k[KindEmergency] {
		t.Fatalf("%v %v", k, err)
	}
	if k, _ := ParseKinds("emergency"); !k[KindEmergency] || !k[KindEmergencyUpdate] || !k[KindEmergencyOver] || len(k) != 3 {
		t.Fatalf("emergency: %v", k)
	}
	if k, _ := ParseKinds("all"); len(k) != len(kindNames) {
		t.Fatal("all")
	}
	if _, err := ParseKinds("nope"); err == nil {
		t.Fatal("expected error")
	}
}

type slackMsg struct {
	Text        string `json:"text"`
	Attachments []struct {
		Color    string `json:"color"`
		Fallback string `json:"fallback"`
		Text     string `json:"text"`
		Blocks   []struct {
			Type string `json:"type"`
			Text *struct {
				Text string `json:"text"`
			} `json:"text"`
			Elements []struct {
				Text string `json:"text"`
			} `json:"elements"`
		} `json:"blocks"`
	} `json:"attachments"`
}

func TestSlackPayload(t *testing.T) {
	s := NewSlackNotifier("https://example.invalid", "<!channel>", nil, nil)
	var m slackMsg
	if err := json.Unmarshal(s.buildPayload(SampleNotifications(), true), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Attachments) != 6 {
		t.Fatalf("attachments: %d", len(m.Attachments))
	}
	wantColor := []string{slackRed, slackGreen, slackRed, slackGreen, slackRed, slackGreen}
	for i, a := range m.Attachments {
		if a.Color != wantColor[i] {
			t.Errorf("attachment %d color %s, want %s", i, a.Color, wantColor[i])
		}
		if a.Fallback == "" || len(a.Blocks) == 0 || a.Blocks[0].Text == nil {
			t.Errorf("attachment %d incomplete: %+v", i, a)
		}
	}
	if !strings.HasPrefix(m.Text, "<!channel> :rotating_light: *HOST DOWN: mail01.example.com*") ||
		!strings.Contains(m.Text, "HOST UP: mail01.example.com") {
		t.Fatalf("headline: %q", m.Text)
	}
	if !strings.Contains(m.Attachments[0].Blocks[0].Text.Text, "process is not running") {
		t.Fatalf("service failed: %+v", m.Attachments[0])
	}
	for i, a := range m.Attachments {
		if len(a.Blocks) != 1 {
			t.Errorf("attachment %d: want only the main section, got %d blocks", i, len(a.Blocks))
		}
	}

	// plain fallback layout, no mention, no headline for service-only batches
	s = NewSlackNotifier("https://example.invalid", "", nil, nil)
	m = slackMsg{}
	json.Unmarshal(s.buildPayload(SampleNotifications()[:2], false), &m)
	if m.Text != "" || m.Attachments[0].Text == "" || len(m.Attachments[0].Blocks) != 0 {
		t.Fatalf("plain: %+v", m)
	}
}

func TestSlackEscape(t *testing.T) {
	n := Notification{Kind: KindServiceFailed, Host: "a<b>", Service: "x&y", Message: "<script>"}
	main, _ := render(n)
	if strings.Contains(main, "<script>") || !strings.Contains(main, "&lt;script&gt;") || !strings.Contains(main, "x&amp;y") {
		t.Fatalf("not escaped: %s", main)
	}
}

func TestRenderSymbolsAndSystemName(t *testing.T) {
	n := Notification{Kind: KindServiceFailed, Host: "readydigital.customer.xtserver.it",
		Service: "readydigital.customer.xtserver.it", ServiceType: 5, Message: "loadavg (1min) of 44.4 matches resource limit"}
	main, _ := render(n)
	want := ":x: *Service failed* · `system` on *readydigital.customer.xtserver.it*"
	if !strings.HasPrefix(main, want) {
		t.Fatalf("got %q", main)
	}
	n = Notification{Kind: KindServiceOK, Host: "web01", Service: "php8.3-fpm-process", ServiceType: 3}
	if main, _ := render(n); !strings.HasPrefix(main, ":white_check_mark: *Service OK* · `php8.3-fpm-process` on *web01*") {
		t.Fatalf("got %q", main)
	}
	for _, n := range SampleNotifications() {
		if main, _ := render(n); strings.Contains(main, ":") && strings.Contains(main, "_circle:") {
			t.Fatalf("emoji left in %q", main)
		}
	}
}
