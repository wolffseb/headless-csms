// Package clocktest is a fake clock for tests that depend on time passing:
// reservation expiry, for one. Time stands still until a test calls Advance,
// and timers fire synchronously inside that call, so a test can say "fifteen
// minutes later" without sleeping and without racing a timer goroutine.
package clocktest

import (
	"sort"
	"sync"
	"time"
)

// Clock is a manually advanced clock. The zero value is not usable; call New.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*Timer
}

// New returns a clock standing at start.
func New(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now reports the clock's current time. It has the signature of time.Now, so
// it can be passed wherever a clock function is expected.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Timer is a pending call scheduled with AfterFunc.
type Timer struct {
	c       *Clock
	at      time.Time
	fn      func()
	stopped bool
}

// Stop prevents the timer from firing. It reports whether it did so, like
// time.Timer.Stop: false means the timer had already fired or been stopped.
func (t *Timer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()

	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// AfterFunc schedules fn to run once the clock has been advanced by d. It
// mirrors time.AfterFunc; a d of zero or less fires on the next Advance, even
// Advance(0).
func (c *Clock) AfterFunc(d time.Duration, fn func()) *Timer {
	c.mu.Lock()
	defer c.mu.Unlock()

	t := &Timer{c: c, at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the clock forward and fires every timer that has come due, in
// deadline order. The callbacks run on the caller's goroutine, after the clock
// has been released, so they may call Now or AfterFunc themselves.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)

	var due, pending []*Timer
	for _, t := range c.timers {
		switch {
		case t.stopped:
		case !t.at.After(c.now):
			t.stopped = true
			due = append(due, t)
		default:
			pending = append(pending, t)
		}
	}
	c.timers = pending
	c.mu.Unlock()

	sort.SliceStable(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, t := range due {
		t.fn()
	}
}

// Pending reports how many timers are scheduled and not yet fired or stopped.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := 0
	for _, t := range c.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}
