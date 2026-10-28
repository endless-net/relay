package mesh

import (
	"testing"
	"time"
)

func TestWireGrantConservesLifetimeAcrossOppositeClockOffsets(t *testing.T) {
	common := time.Unix(1800000000, 0)
	for _, senderOffset := range []time.Duration{-time.Second, 0, time.Second} {
		for _, receiverOffset := range []time.Duration{-time.Second, 0, time.Second} {
			raw := common.Add(senderOffset + 10*time.Second).UnixNano()
			for _, delay := range []time.Duration{0, 5 * time.Second} {
				now := common.Add(receiverOffset + delay)
				until, err := receiveGrantDeadline(raw, now)
				if err != nil {
					t.Fatal(err)
				}
				if until.Sub(now) > 10*time.Second-delay {
					t.Fatal("clock skew extended source lifetime", senderOffset, receiverOffset, delay)
				}
			}
			if _, err := receiveGrantDeadline(raw, common.Add(receiverOffset+11*time.Second)); err == nil {
				t.Fatal("delayed expired frame admitted")
			}
		}
	}
}

func TestWireGrantRequiresCurrentFieldAndNeverRestartsAfterReconnect(t *testing.T) {
	now := time.Now()
	for _, raw := range []int64{0, -1, now.Add(13 * time.Second).UnixNano(), now.Add(time.Second).UnixNano()} {
		if _, err := receiveGrantDeadline(raw, now); err == nil {
			t.Fatal("invalid/missing wire deadline admitted")
		}
	}
	raw := now.Add(10 * time.Second).UnixNano()
	first, err := receiveGrantDeadline(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := receiveGrantDeadline(raw, now.Add(5*time.Second))
	if err != nil || !first.Equal(again) {
		t.Fatal("reconnect restarted deadline", err)
	}
}
