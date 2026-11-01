package mesh

import (
	"errors"
	"time"
)

var errExpiredGrant = errors.New("mesh_grant_expired")

// Each clock is within one second of the common authority. Two seconds
// conservatively cover the sender/receiver difference. Anchor the remaining
// lifetime to the receiver's monotonic clock; reconnect never restarts it.
func receiveGrantDeadline(raw int64, now time.Time) (time.Time, error) {
	if raw <= 0 {
		return time.Time{}, errors.New("mesh_grant_required")
	}
	remaining := time.Unix(0, raw).Add(-2 * time.Second).Sub(now)
	if remaining <= 0 {
		return time.Time{}, errExpiredGrant
	}
	if remaining > 10*time.Second {
		return time.Time{}, errors.New("mesh_grant_clock_window")
	}
	return now.Add(remaining), nil
}
