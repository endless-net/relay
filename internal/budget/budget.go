// Package budget provides nonblocking resource accounting for runtime queues.
package budget

import (
	"sync"
	"sync/atomic"
	"time"
)

type Bytes struct {
	Limit int64
	used  atomic.Int64
}

func (b *Bytes) Acquire(n int64) bool {
	if b == nil || n < 0 {
		return false
	}
	for {
		old := b.used.Load()
		if n > b.Limit-old {
			return false
		}
		if b.used.CompareAndSwap(old, old+n) {
			return true
		}
	}
}
func (b *Bytes) Release(n int64) {
	if b != nil {
		b.used.Add(-n)
	}
}
func (b *Bytes) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

type Rate struct {
	mu                  sync.Mutex
	rate, burst, tokens float64
	last                time.Time
}

func NewRate(rate, burst int) *Rate {
	return &Rate{rate: float64(rate), burst: float64(burst), tokens: float64(burst), last: time.Now()}
}
func (r *Rate) Allow(n int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.tokens = min(r.burst, r.tokens+now.Sub(r.last).Seconds()*r.rate)
	r.last = now
	if n < 0 || float64(n) > r.tokens {
		return false
	}
	r.tokens -= float64(n)
	return true
}
