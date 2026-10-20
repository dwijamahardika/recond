// Package clock abstracts time so as-of defaults and webhook timestamp
// tolerance are testable without sleeping.
package clock

import (
	"sync"
	"time"
)

// Clock tells the time.
type Clock interface{ Now() time.Time }

// System is the real clock.
type System struct{}

// Now returns the current time.
func (System) Now() time.Time { return time.Now() }

// Fake is a manually advanced, goroutine-safe clock for tests.
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake returns a fake clock stopped at t.
func NewFake(t time.Time) *Fake { return &Fake{t: t} }

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance moves the fake time forward.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}
