package server

import (
	"sync"
	"time"
)

// limiter is a token bucket per key (usually client IP). It is deliberately
// simple and in-memory: one process serves one deployment.
type limiter struct {
	rate  float64 // tokens per second
	burst float64
	mu    sync.Mutex
	m     map[string]*bucket
	max   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perSecond float64, burst int) *limiter {
	return &limiter{rate: perSecond, burst: float64(burst), m: map[string]*bucket{}, max: 200_000}
}

// allow takes one token; when denied it reports how long until one is free.
func (l *limiter) allow(key string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= l.max { // memory bound under a flood of distinct keys
			l.m = map[string]*bucket{}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.m[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// refund gives back a token. It is used so that only FAILED logins count against
// the throttles: a shop with several devices signing in at opening time must not
// be locked out by its own successful logins.
func (l *limiter) refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.m[key]; b != nil {
		b.tokens = min(l.burst, b.tokens+1)
	}
}

func (l *limiter) gc(idle time.Duration) {
	cut := time.Now().Add(-idle)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.m {
		if b.last.Before(cut) {
			delete(l.m, k)
		}
	}
}
