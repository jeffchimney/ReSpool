package auth

import (
	"sync"
	"time"
)

// Limiter slows down password guessing: each client gets a few wrong
// attempts per window, then has to wait.
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time
}

// NewLimiter allows max failures per client within window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, failures: map[string][]time.Time{}}
}

// Wait returns how long client has to wait before trying again, or 0.
func (l *Limiter) Wait(client string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.recent(client)
	if len(f) < l.max {
		return 0
	}
	return f[len(f)-l.max].Add(l.window).Sub(l.now())
}

// Fail records a wrong attempt by client.
func (l *Limiter) Fail(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) > 10000 {
		for c := range l.failures {
			l.recent(c) // drops clients with no recent failures
		}
	}
	l.failures[client] = append(l.recent(client), l.now())
}

// Reset forgets client's failures after it gets in.
func (l *Limiter) Reset(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, client)
}

// recent returns client's failures within the window, pruning older ones.
func (l *Limiter) recent(client string) []time.Time {
	cutoff := l.now().Add(-l.window)
	f := l.failures[client]
	i := 0
	for i < len(f) && !f[i].After(cutoff) {
		i++
	}
	f = f[i:]
	if len(f) == 0 {
		delete(l.failures, client)
		return nil
	}
	l.failures[client] = f
	return f
}
