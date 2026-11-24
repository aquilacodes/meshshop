package main

import (
	"sync"
	"time"
)

// Business-flow limits enforced at the edge. The business layer enforces the
// same cart limits again; these give clients early, specific errors.
const (
	maxCartLines    = 20
	maxLineQty      = 10
	checkoutsPerMin = 5
)

// limiter is a per-key token bucket: up to burst events, refilled at burst per
// window. State is per BFF replica, which is fine as a second line behind the
// gateway's rate limit; a shared store (Redis) would make it exact.
type limiter struct {
	mu     sync.Mutex
	burst  float64
	rate   float64 // tokens per second
	bucket map[string]*bucket
	now    func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(burst int, window time.Duration) *limiter {
	return &limiter{burst: float64(burst), rate: float64(burst) / window.Seconds(),
		bucket: map[string]*bucket{}, now: time.Now}
}

// allow takes a token for key. When none is left it returns how long to wait.
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.bucket) > 10000 { // drop idle buckets so the map cannot grow without bound
		for k, b := range l.bucket {
			if now.Sub(b.last).Seconds()*l.rate >= l.burst {
				delete(l.bucket, k)
			}
		}
	}
	b, ok := l.bucket[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.bucket[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}
