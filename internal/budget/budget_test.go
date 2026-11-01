package budget

import (
	"sync"
	"testing"
)

func TestConcurrentByteCapAndRelease(t *testing.T) {
	b := &Bytes{Limit: 100}
	var wg sync.WaitGroup
	for range 1000 {
		wg.Go(func() {
			if b.Acquire(10) {
				if b.Used() > 100 {
					t.Error("cap exceeded")
				}
				b.Release(10)
			}
		})
	}
	wg.Wait()
	if b.Used() != 0 || b.Acquire(101) {
		t.Fatal("accounting")
	}
}
func TestRateDoesNotWaitOrExceedBurst(t *testing.T) {
	r := NewRate(1, 2)
	if !r.Allow(2) || r.Allow(1) || r.Allow(-1) {
		t.Fatal("rate burst")
	}
}
