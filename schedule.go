package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the timezone database is embedded: no dependency on the server's tzdata
)

// OfficeHours is a weekly schedule, e.g. "mon-fri 08:00-13:00,14:00-18:00",
// evaluated in a timezone.
type OfficeHours struct {
	days   [7]bool // indexed by time.Weekday
	ranges []minuteRange
	loc    *time.Location
	spec   string
}

type minuteRange struct{ from, to int } // minutes since midnight, [from, to)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseOfficeHours parses "DAYS HH:MM-HH:MM[,HH:MM-HH:MM...]" where DAYS is a
// day ("mon"), a range ("mon-fri") or a comma list ("mon,wed,fri").
func ParseOfficeHours(spec, tz string) (*OfficeHours, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", tz, err)
	}
	oh := &OfficeHours{loc: loc, spec: strings.TrimSpace(spec)}
	daysPart, timesPart, ok := strings.Cut(strings.ToLower(oh.spec), " ")
	if !ok {
		return nil, fmt.Errorf("office hours %q: expected e.g. \"mon-fri 08:00-13:00,14:00-18:00\"", spec)
	}
	for _, d := range strings.Split(daysPart, ",") {
		from, to, isRange := strings.Cut(d, "-")
		a, okA := weekdays[from]
		if !okA {
			return nil, fmt.Errorf("office hours: unknown day %q", from)
		}
		if !isRange {
			oh.days[a] = true
			continue
		}
		b, okB := weekdays[to]
		if !okB {
			return nil, fmt.Errorf("office hours: unknown day %q", to)
		}
		for w := a; ; w = (w + 1) % 7 {
			oh.days[w] = true
			if w == b {
				break
			}
		}
	}
	for _, r := range strings.Split(strings.ReplaceAll(timesPart, " ", ""), ",") {
		f, t, ok := strings.Cut(r, "-")
		if !ok {
			return nil, fmt.Errorf("office hours: bad time range %q", r)
		}
		fm, err1 := parseHHMM(f)
		tm, err2 := parseHHMM(t)
		if err1 != nil || err2 != nil || tm <= fm {
			return nil, fmt.Errorf("office hours: bad time range %q", r)
		}
		oh.ranges = append(oh.ranges, minuteRange{fm, tm})
	}
	return oh, nil
}

func parseHHMM(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return h*60 + m, nil
}

// Contains reports whether t falls within office hours.
func (oh *OfficeHours) Contains(t time.Time) bool {
	lt := t.In(oh.loc)
	if !oh.days[lt.Weekday()] {
		return false
	}
	m := lt.Hour()*60 + lt.Minute()
	for _, r := range oh.ranges {
		if m >= r.from && m < r.to {
			return true
		}
	}
	return false
}

func (oh *OfficeHours) String() string { return oh.spec + " " + oh.loc.String() }

// Policy says when the notifications of a service are sent.
type Policy int

const (
	PolicyAlways Policy = iota
	PolicyOffice
	PolicyNever
)

func ParsePolicy(s string) (Policy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "always":
		return PolicyAlways, nil
	case "office":
		return PolicyOffice, nil
	case "never":
		return PolicyNever, nil
	}
	return 0, fmt.Errorf("unknown policy %q (always, office, never)", s)
}

// GroupSchedule filters service failed/OK notifications by service group:
// services in an "always" group are notified at any time, those in an
// "office" group only during office hours. Everything else (host down/up,
// Monit start/stop, emergencies) passes through.
//
// Outside office hours:
//   - a failure of an office-hours service is held; if it is still failing
//     when the office opens it is notified then, if it recovers meanwhile
//     nothing is sent;
//   - a recovery is sent anyway when its failure had been notified, so that
//     the channel never keeps a red message without its green one.
type GroupSchedule struct {
	inner    Notifier
	always   map[string]bool // lower-case group names
	office   map[string]bool
	ungroup  Policy
	hours    *OfficeHours
	log      *slog.Logger
	now      func() time.Time
	mu       sync.Mutex
	held     map[string]Notification // failures held outside office hours
	notified map[string]int          // failures notified and not yet recovered
	wasOpen  bool
}

func NewGroupSchedule(inner Notifier, always, office []string, ungrouped Policy, hours *OfficeHours, log *slog.Logger) *GroupSchedule {
	g := &GroupSchedule{
		inner: inner, ungroup: ungrouped, hours: hours, log: log, now: time.Now,
		always: map[string]bool{}, office: map[string]bool{},
		held: map[string]Notification{}, notified: map[string]int{},
	}
	for _, n := range always {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			g.always[n] = true
		}
	}
	for _, n := range office {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			g.office[n] = true
		}
	}
	g.wasOpen = hours.Contains(g.now())
	return g
}

func (g *GroupSchedule) policy(groups []string) Policy {
	inOffice := false
	for _, gr := range groups {
		gr = strings.ToLower(gr)
		if g.always[gr] {
			return PolicyAlways // "always" wins over "office"
		}
		if g.office[gr] {
			inOffice = true
		}
	}
	if inOffice {
		return PolicyOffice
	}
	return g.ungroup
}

func failKey(n Notification) string {
	return n.Host + "\x00" + n.Service + "\x00" + fmt.Sprint(n.EventID)
}

func (g *GroupSchedule) Notify(n Notification) {
	if n.Kind != KindServiceFailed && n.Kind != KindServiceOK {
		g.inner.Notify(n)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := failKey(n)
	pol := g.policy(n.Groups)
	open := g.hours.Contains(g.now())

	if n.Kind == KindServiceOK {
		if _, ok := g.held[key]; ok {
			delete(g.held, key) // failed and recovered outside office hours: nothing to say
			g.log.Info("notification held: recovered outside office hours", "host", n.Host, "service", n.Service)
			return
		}
		if g.notified[key] > 0 {
			// its failure was notified: always close it
			if g.notified[key]--; g.notified[key] == 0 {
				delete(g.notified, key)
			}
			g.inner.Notify(n)
			return
		}
		if pol == PolicyAlways || (pol == PolicyOffice && open) {
			g.inner.Notify(n)
		}
		return
	}

	switch {
	case pol == PolicyNever:
		return
	case pol == PolicyAlways || open:
		g.notified[key]++
		g.inner.Notify(n)
	default:
		if _, ok := g.held[key]; !ok {
			g.held[key] = n
			g.log.Info("notification held until office hours", "host", n.Host, "service", n.Service,
				"groups", strings.Join(n.Groups, ","))
		}
	}
}

// Run sends the held failures when office hours begin.
func (g *GroupSchedule) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.tick()
		}
	}
}

func (g *GroupSchedule) tick() {
	g.mu.Lock()
	defer g.mu.Unlock()
	open := g.hours.Contains(g.now())
	if open && !g.wasOpen && len(g.held) > 0 {
		g.log.Info("office hours started: sending held failures", "count", len(g.held))
		for key, n := range g.held {
			n.Note = "failing since " + n.Time.In(g.hours.loc).Format("Mon 15:04") + " (outside office hours)"
			g.notified[key]++
			g.inner.Notify(n)
		}
		g.held = map[string]Notification{}
	}
	g.wasOpen = open
}
