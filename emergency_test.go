package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

type recorder struct{ got []Notification }

func (r *recorder) Notify(n Notification) { r.got = append(r.got, n) }

func (r *recorder) take() []Notification { g := r.got; r.got = nil; return g }

func kinds(ns []Notification) map[Kind]int {
	m := map[Kind]int{}
	for _, n := range ns {
		m[n.Kind]++
	}
	return m
}

func newTestEmergency() (*Emergency, *recorder) {
	rec := &recorder{}
	e := NewEmergency(rec, 10, 5*time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return e, rec
}

func host(i int) string { return fmt.Sprintf("srv%03d", i) }

func TestEmergencyLifecycle(t *testing.T) {
	e, rec := newTestEmergency()
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	down := func(i int, now time.Time) {
		e.process(Notification{Kind: KindHostDown, Host: host(i), Time: now, LastReport: now.Add(-3 * time.Minute)}, now)
	}
	up := func(i int, now time.Time) { e.process(Notification{Kind: KindHostUp, Host: host(i), Time: now}, now) }
	svc := func(i int, now time.Time) {
		e.process(Notification{Kind: KindServiceFailed, Host: host(i), Service: "nginx", Time: now}, now)
	}

	// 1. three hosts down: below the threshold, notified one by one after the hold
	for i := 1; i <= 3; i++ {
		down(i, at(0))
	}
	e.tick(at(5 * time.Second))
	if len(rec.got) != 0 {
		t.Fatal("host down must be held")
	}
	e.tick(at(11 * time.Second))
	if k := kinds(rec.take()); k[KindHostDown] != 3 || e.Active() {
		t.Fatalf("step 1: %v", k)
	}

	// 2. one round of the offline check marks 150 hosts: one emergency message
	for i := 4; i <= 153; i++ {
		down(i, at(time.Minute))
	}
	e.tick(at(time.Minute + 11*time.Second))
	got := rec.take()
	if len(got) != 1 || got[0].Kind != KindEmergency || got[0].Count != 153 || !e.Active() {
		t.Fatalf("step 2: %d %v %+v", len(got), kinds(got), got)
	}

	// 3. service events: suppressed for hosts in the emergency, sent for the others
	svc(50, at(2*time.Minute))
	svc(999, at(2*time.Minute))
	if g := rec.take(); len(g) != 1 || g[0].Host != host(999) {
		t.Fatalf("step 3: %+v", g)
	}

	// 4. 120 hosts come back: no single host up; an update after the interval
	for i := 1; i <= 120; i++ {
		up(i, at(4*time.Minute))
	}
	e.tick(at(4*time.Minute + 11*time.Second))
	if g := rec.take(); len(g) != 1 || g[0].Kind != KindEmergencyUpdate || g[0].Count != 33 || g[0].Recovered != 120 || g[0].Suppressed != 1 {
		t.Fatalf("step 4: %+v", g)
	}
	// no change -> no further update
	e.tick(at(8 * time.Minute))
	if g := rec.take(); len(g) != 0 {
		t.Fatalf("step 4b: unexpected %+v", g)
	}

	// 5. more come back; with < 10 still down and no new downs for 5 min -> over
	for i := 121; i <= 148; i++ {
		up(i, at(9*time.Minute))
	}
	e.tick(at(9*time.Minute + 11*time.Second))
	g := rec.take()
	if len(g) != 1 || g[0].Kind != KindEmergencyOver || g[0].Count != 5 || g[0].Recovered != 148 || g[0].Involved != 153 || e.Active() {
		t.Fatalf("step 5: %+v", g)
	}

	// 6. backlog from a recovered host is suppressed during the cooldown, then sent
	svc(130, at(10*time.Minute))
	if g := rec.take(); len(g) != 0 {
		t.Fatalf("step 6: cooldown not applied: %+v", g)
	}
	e.tick(at(16 * time.Minute))
	svc(130, at(16*time.Minute))
	if g := rec.take(); len(g) != 1 {
		t.Fatalf("step 6b: %+v", g)
	}

	// 7. a host still down after the emergency comes back: its backlog is
	// suppressed (also during the cooldown that follows), its host up is sent
	svc(150, at(20*time.Minute))
	if g := rec.take(); len(g) != 0 {
		t.Fatalf("step 7: straggler backlog sent: %+v", g)
	}
	up(150, at(20*time.Minute))
	e.tick(at(20*time.Minute + 11*time.Second))
	if g := rec.take(); len(g) != 1 || g[0].Kind != KindHostUp {
		t.Fatalf("step 7: %+v", g)
	}
	svc(150, at(21*time.Minute))
	if g := rec.take(); len(g) != 0 {
		t.Fatalf("step 7b: backlog after host up sent: %+v", g)
	}
	svc(150, at(27*time.Minute))
	if g := rec.take(); len(g) != 1 {
		t.Fatalf("step 7c: real event after cooldown not sent: %+v", g)
	}
}

func TestHostsUpStorm(t *testing.T) {
	// e.g. after a collector restart: many hosts reported back at once
	e, rec := newTestEmergency()
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 50; i++ {
		e.process(Notification{Kind: KindHostUp, Host: host(i), Time: t0}, t0)
	}
	e.tick(t0.Add(11 * time.Second))
	g := rec.take()
	if len(g) != 1 || g[0].Kind != KindHostsUp || g[0].Count != 50 {
		t.Fatalf("%d %v", len(g), kinds(g))
	}
}

func TestEmergencyDisabled(t *testing.T) {
	rec := &recorder{}
	e := NewEmergency(rec, 0, 5*time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := 1; i <= 50; i++ {
		e.Notify(Notification{Kind: KindHostDown, Host: host(i), Time: time.Now()})
	}
	if len(rec.got) != 50 {
		t.Fatalf("disabled: %d", len(rec.got))
	}
}

func TestEmergencyPayload(t *testing.T) {
	s := NewSlackNotifier("https://example.invalid", "<!channel>", nil, nil)
	var m slackMsg
	if err := json.Unmarshal(s.buildPayload(SampleEmergency(), true), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Attachments) != 3 || m.Attachments[0].Color != slackRed || m.Attachments[1].Color != slackRed || m.Attachments[2].Color != slackGreen {
		t.Fatalf("%+v", m.Attachments)
	}
	if want := "<!channel> :rotating_light::rotating_light: *EMERGENCY: 120 HOSTS DOWN*"; len(m.Text) < len(want) || m.Text[:len(want)] != want {
		t.Fatalf("headline: %q", m.Text)
	}
	body := m.Attachments[0].Blocks[0].Text.Text
	if len(body) > 3000 {
		t.Fatalf("section too long: %d", len(body))
	}
}
