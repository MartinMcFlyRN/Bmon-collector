package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func mustHours(t *testing.T, spec string) *OfficeHours {
	t.Helper()
	oh, err := ParseOfficeHours(spec, "Europe/Rome")
	if err != nil {
		t.Fatal(err)
	}
	return oh
}

func rome(t *testing.T, s string) time.Time {
	t.Helper()
	loc, _ := time.LoadLocation("Europe/Rome")
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestOfficeHours(t *testing.T) {
	oh := mustHours(t, "mon-fri 08:00-13:00,14:00-18:00")
	cases := map[string]bool{
		"2026-11-17 07:59": false, // Tuesday
		"2026-11-17 08:00": true,
		"2026-11-17 12:59": true,
		"2026-11-17 13:00": false, // lunch break
		"2026-11-17 13:59": false,
		"2026-11-17 14:00": true,
		"2026-11-17 17:59": true,
		"2026-11-17 18:00": false,
		"2026-11-21 10:00": false, // Saturday
		"2026-11-22 10:00": false, // Sunday
		"2026-11-23 08:30": true,  // Monday
		"2026-07-15 08:30": true,  // summer (CEST)
		"2026-07-15 07:30": false,
	}
	for s, want := range cases {
		if got := oh.Contains(rome(t, s)); got != want {
			t.Errorf("%s: got %v want %v", s, got, want)
		}
	}
	// the same UTC instant is in office hours in winter (UTC+1) but not in summer (UTC+2)
	if !oh.Contains(time.Date(2026, 1, 14, 7, 30, 0, 0, time.UTC)) || oh.Contains(time.Date(2026, 7, 15, 5, 30, 0, 0, time.UTC)) {
		t.Error("CET/CEST not handled")
	}
	for _, bad := range []string{"", "mon-fri", "xyz 08:00-09:00", "mon 09:00-08:00", "mon 25:00-26:00"} {
		if _, err := ParseOfficeHours(bad, "Europe/Rome"); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	if _, err := ParseOfficeHours("mon,wed,fri 09:00-17:30", "Europe/Rome"); err != nil {
		t.Error(err)
	}
}

func newSched(t *testing.T, ungrouped Policy) (*GroupSchedule, *recorder, *time.Time) {
	rec := &recorder{}
	now := rome(t, "2026-11-17 10:00")
	g := NewGroupSchedule(rec, []string{"H24"}, []string{"workday"}, ungrouped,
		mustHours(t, "mon-fri 08:00-13:00,14:00-18:00"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.now = func() time.Time { return now }
	g.wasOpen = true
	return g, rec, &now
}

func svcNote(kind Kind, svc string, groups ...string) Notification {
	return Notification{Kind: kind, Host: "web01", Service: svc, EventID: 0x2, Groups: groups}
}

func TestGroupSchedule(t *testing.T) {
	g, rec, now := newSched(t, PolicyAlways)

	// office hours: everything is sent
	g.Notify(svcNote(KindServiceFailed, "php", "workday"))
	g.Notify(svcNote(KindServiceFailed, "nginx", "H24"))
	if n := len(rec.take()); n != 2 {
		t.Fatalf("office hours: %d", n)
	}

	// evening: H24 and ungrouped are sent, workday is held
	*now = rome(t, "2026-11-17 19:00")
	g.tick()
	g.Notify(svcNote(KindServiceFailed, "nginx2", "H24"))
	g.Notify(svcNote(KindServiceFailed, "cron"))                   // no group -> always
	g.Notify(svcNote(KindServiceFailed, "both", "workday", "H24")) // H24 wins
	g.Notify(svcNote(KindServiceFailed, "mysql", "workday"))       // held
	g.Notify(svcNote(KindServiceFailed, "backup", "WORKDAY"))      // held (case-insensitive)
	if got := rec.take(); len(got) != 3 {
		t.Fatalf("evening: %+v", got)
	}

	// a workday failure notified during the day recovers in the evening: OK is sent
	g.Notify(svcNote(KindServiceOK, "php", "workday"))
	if got := rec.take(); len(got) != 1 || got[0].Service != "php" {
		t.Fatalf("closing OK: %+v", got)
	}
	// a held failure recovers during the night: nothing at all
	g.Notify(svcNote(KindServiceOK, "backup", "workday"))
	// an OK for a workday service whose failure we never saw: not sent at night
	g.Notify(svcNote(KindServiceOK, "other", "workday"))
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("night OKs: %+v", got)
	}

	// next morning: mysql is still failing -> notified with a note
	*now = rome(t, "2026-11-18 07:59")
	g.tick()
	if len(rec.got) != 0 {
		t.Fatal("too early")
	}
	*now = rome(t, "2026-11-18 08:00")
	g.tick()
	got := rec.take()
	if len(got) != 1 || got[0].Service != "mysql" || !strings.Contains(got[0].Note, "failing since") {
		t.Fatalf("morning: %+v", got)
	}
	// and its later recovery is notified
	g.Notify(svcNote(KindServiceOK, "mysql", "workday"))
	if got := rec.take(); len(got) != 1 {
		t.Fatalf("mysql OK: %+v", got)
	}

	// lunch break counts as outside office hours
	*now = rome(t, "2026-11-18 13:30")
	g.tick()
	g.Notify(svcNote(KindServiceFailed, "lunch", "workday"))
	if len(rec.take()) != 0 {
		t.Fatal("lunch break")
	}
	*now = rome(t, "2026-11-18 14:00")
	g.tick()
	if got := rec.take(); len(got) != 1 || got[0].Service != "lunch" {
		t.Fatalf("after lunch: %+v", got)
	}

	// other kinds always pass
	*now = rome(t, "2026-11-21 03:00")
	g.tick()
	g.Notify(Notification{Kind: KindHostDown, Host: "web01"})
	g.Notify(Notification{Kind: KindMonitStopped, Host: "web01"})
	if len(rec.take()) != 2 {
		t.Fatal("host/monit notifications must pass")
	}
}

func TestGroupScheduleUngrouped(t *testing.T) {
	g, rec, now := newSched(t, PolicyNever)
	g.Notify(svcNote(KindServiceFailed, "cron"))
	g.Notify(svcNote(KindServiceFailed, "nginx", "H24"))
	if got := rec.take(); len(got) != 1 || got[0].Service != "nginx" {
		t.Fatalf("never: %+v", got)
	}
	g, rec, now = newSched(t, PolicyOffice)
	*now = rome(t, "2026-11-17 22:00")
	g.tick()
	g.Notify(svcNote(KindServiceFailed, "cron"))
	if len(rec.take()) != 0 {
		t.Fatal("office policy for ungrouped at night")
	}
}
