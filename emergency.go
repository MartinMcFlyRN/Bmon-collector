package main

import (
	"context"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"
)

// Emergency sits between the store and the real notifier (Slack). When many
// hosts go down in a short time (a provider or network outage), it replaces
// the flood of single "host down" messages with one emergency message,
// periodic updates and an "emergency over" summary. Service and Monit events
// coming from the hosts involved (for instance the backlog Monit resends from
// its eventqueue when the network comes back) are counted instead of sent.
//
// Host down/up notifications are held for a few seconds before being
// forwarded, so that a whole round of the offline check is evaluated at once.
type Emergency struct {
	inner Notifier
	log   *slog.Logger
	in    chan Notification

	Threshold int           // hosts down within Window that start an emergency; 0 = disabled
	Window    time.Duration // sliding window for the threshold
	Hold      time.Duration // how long host down/up are held before deciding
	Update    time.Duration // minimum interval between progress updates
	Cooldown  time.Duration // service events of a recovered host are suppressed for this long

	active   atomic.Bool
	dropped  atomic.Int64
	state    emergencyState
	tickEach time.Duration
}

type emergencyState struct {
	pending   []Notification
	downTimes []time.Time // host down times within Window
	upTimes   []time.Time // host up times within Window (outside emergencies)

	active     bool
	started    time.Time
	lastUpdate time.Time
	down       map[string]time.Time // hosts currently down that are part of the emergency
	involved   int                  // hosts that went down during the emergency
	recovered  int
	suppressed int
	changed    bool
	lastSent   int // down count in the last message

	recoveredAt map[string]time.Time
	recentDown  map[string]time.Time // hosts notified down outside an emergency
	stragglers  map[string]bool      // hosts still down when the last emergency ended
}

func NewEmergency(inner Notifier, threshold int, window time.Duration, log *slog.Logger) *Emergency {
	if inner == nil {
		inner = nopNotifier{}
	}
	return &Emergency{
		inner: inner, log: log, in: make(chan Notification, 5000),
		Threshold: threshold, Window: window,
		Hold: 10 * time.Second, Update: 3 * time.Minute, Cooldown: 5 * time.Minute,
		tickEach: time.Second,
		state:    emergencyState{down: map[string]time.Time{}, recoveredAt: map[string]time.Time{}},
	}
}

type nopNotifier struct{}

func (nopNotifier) Notify(Notification) {}

func (e *Emergency) Active() bool { return e.active.Load() }

// Notify never blocks.
func (e *Emergency) Notify(n Notification) {
	if e.Threshold <= 0 {
		e.inner.Notify(n)
		return
	}
	select {
	case e.in <- n:
	default:
		if e.dropped.Add(1)%100 == 1 {
			e.log.Error("emergency queue full, dropping notifications", "dropped_total", e.dropped.Load())
		}
	}
}

func (e *Emergency) Run(ctx context.Context) {
	if e.Threshold <= 0 {
		return
	}
	t := time.NewTicker(e.tickEach)
	defer t.Stop()
	for {
		select {
		case n := <-e.in:
			e.process(n, time.Now())
		case now := <-t.C:
			e.tick(now)
		case <-ctx.Done():
			// forward whatever is still held, so nothing is lost on shutdown
			for _, n := range e.state.pending {
				e.inner.Notify(n)
			}
			return
		}
	}
}

func (e *Emergency) process(n Notification, now time.Time) {
	st := &e.state
	switch n.Kind {
	case KindHostDown, KindHostUp:
		st.pending = append(st.pending, n)
		return
	}
	// service / Monit events
	if st.active {
		if _, ok := st.down[n.Host]; ok {
			st.suppressed++
			return
		}
	}
	if st.stragglers[n.Host] {
		// still down after the emergency: this is backlog arriving just before its "host up"
		st.suppressed++
		e.log.Info("notification suppressed: host still down after an emergency",
			"host", n.Host, "kind", n.Kind.String(), "service", n.Service)
		return
	}
	if t, ok := st.recoveredAt[n.Host]; ok && now.Sub(t) < e.Cooldown {
		st.suppressed++
		e.log.Info("notification suppressed: host recovering from an emergency",
			"host", n.Host, "kind", n.Kind.String(), "service", n.Service)
		return
	}
	e.inner.Notify(n)
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}

func (e *Emergency) tick(now time.Time) {
	st := &e.state
	for h, t := range st.recoveredAt {
		if now.Sub(t) >= e.Cooldown {
			delete(st.recoveredAt, h)
		}
	}
	for h, t := range st.recentDown {
		if now.Sub(t) >= e.Window {
			delete(st.recentDown, h)
		}
	}
	if len(st.pending) > 0 && now.Sub(st.pending[0].Time) >= e.Hold {
		e.flush(now)
	}
	if !st.active {
		return
	}
	st.downTimes = prune(st.downTimes, now.Add(-e.Window))
	// Over when fewer than Threshold hosts are still down and nothing new went down recently.
	if len(st.down) < e.Threshold && len(st.downTimes) == 0 {
		e.end(now)
		return
	}
	if st.changed && now.Sub(st.lastUpdate) >= e.Update {
		e.send(now, KindEmergencyUpdate)
	}
}

func (e *Emergency) flush(now time.Time) {
	st := &e.state
	batch := st.pending
	st.pending = nil
	cutoff := now.Add(-e.Window)
	st.downTimes = prune(st.downTimes, cutoff)
	st.upTimes = prune(st.upTimes, cutoff)

	var downs, ups []Notification
	for _, n := range batch {
		if n.Kind == KindHostDown {
			downs = append(downs, n)
			st.downTimes = append(st.downTimes, now)
		} else {
			ups = append(ups, n)
		}
	}

	if !st.active && len(st.downTimes) >= e.Threshold {
		st.active = true
		e.active.Store(true)
		st.started = now
		st.involved, st.recovered, st.suppressed = 0, 0, 0
		// hosts that went down earlier in the window were already notified one
		// by one; they are part of the emergency anyway
		for h, t := range st.recentDown {
			if !t.Before(cutoff) {
				st.down[h] = t
				st.involved++
			}
		}
		for _, n := range downs {
			if _, ok := st.down[n.Host]; !ok {
				st.involved++
			}
			st.down[n.Host] = n.LastReport
		}
		st.recentDown = nil
		downs = nil
		e.log.Warn("EMERGENCY: many hosts down", "down", len(st.down), "window", e.Window)
		e.send(now, KindEmergency)
	}

	if st.active {
		for _, n := range downs {
			if _, ok := st.down[n.Host]; !ok {
				st.involved++
			}
			st.down[n.Host] = n.LastReport
			st.changed = true
		}
		for _, n := range ups {
			if _, ok := st.down[n.Host]; ok {
				delete(st.down, n.Host)
				st.recovered++
				st.recoveredAt[n.Host] = now
				st.changed = true
				continue
			}
			e.inner.Notify(n) // a host that was not part of the emergency
		}
		return
	}

	// No emergency: remember recent downs (to include them if one starts) and forward.
	for _, n := range downs {
		st.rememberDown(n.Host, now)
		e.inner.Notify(n)
	}
	for _, n := range ups {
		delete(st.recentDownMap(), n.Host)
		st.upTimes = append(st.upTimes, now)
		if st.stragglers[n.Host] {
			delete(st.stragglers, n.Host)
			st.recoveredAt[n.Host] = now // its backlog is suppressed during the cooldown
		}
	}
	if len(ups) > 0 && len(st.upTimes) >= e.Threshold {
		// many hosts coming back at once (e.g. after a collector restart): one message
		e.sendHostsUp(now, ups)
		return
	}
	for _, n := range ups {
		e.inner.Notify(n)
	}
}

// recentDown keeps the hosts notified down outside an emergency, with the
// time, so that they can be counted in an emergency that starts shortly after.
func (st *emergencyState) rememberDown(host string, t time.Time) {
	st.recentDownMap()[host] = t
}

func (st *emergencyState) recentDownMap() map[string]time.Time {
	if st.recentDown == nil {
		st.recentDown = map[string]time.Time{}
	}
	return st.recentDown
}

func (e *Emergency) end(now time.Time) {
	st := &e.state
	e.log.Warn("EMERGENCY over", "recovered", st.recovered, "still_down", len(st.down),
		"duration", fmtDur(now.Sub(st.started)), "suppressed", st.suppressed)
	e.send(now, KindEmergencyOver)
	// Hosts still down go back to normal handling: their "host up" will be
	// notified, while the backlog they send when they come back is suppressed.
	st.stragglers = map[string]bool{}
	for h := range st.down {
		st.stragglers[h] = true
	}
	st.active = false
	e.active.Store(false)
	st.down = map[string]time.Time{}
	st.changed = false
}

func sortedHosts(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for h := range m {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func (e *Emergency) send(now time.Time, kind Kind) {
	st := &e.state
	e.inner.Notify(Notification{
		Kind: kind, Time: now,
		Hosts:      sortedHosts(st.down),
		Count:      len(st.down),
		Involved:   st.involved,
		Recovered:  st.recovered,
		Suppressed: st.suppressed,
		Duration:   now.Sub(st.started),
		Threshold:  e.Window,
		Previous:   st.lastSent,
	})
	st.lastUpdate = now
	st.lastSent = len(st.down)
	st.changed = false
}

func (e *Emergency) sendHostsUp(now time.Time, ups []Notification) {
	hosts := make([]string, 0, len(ups))
	for _, n := range ups {
		hosts = append(hosts, n.Host)
	}
	sort.Strings(hosts)
	e.inner.Notify(Notification{Kind: KindHostsUp, Time: now, Hosts: hosts, Count: len(hosts)})
}
