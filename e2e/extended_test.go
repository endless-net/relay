//go:build e2e && extended

package e2e

import (
	"os"
	"strconv"
	"testing"
	"time"
)

func TestProductExtended(t *testing.T) {
	if os.Getenv("E2E_EXTENDED") != "1" {
		t.Fatal("extended run requires E2E_EXTENDED=1")
	}
	deadline := time.Now().Add(30 * time.Minute)
	for cycle := 0; time.Now().Before(deadline); cycle++ {
		a := dialRelayClientEventually(t, "relay-a", "node-a", 15*time.Second)
		b := dialRelayClientEventually(t, "relay-b", "node-b", 15*time.Second)
		waitForTransfer(t, a, b, 20*time.Second)
		waitForTransfer(t, b, a, 20*time.Second)
		assertTransfer(t, b, a, []byte("soak"))
		a.close()
		b.close()
		if err := suite.recreateRelay("relay-b", "soak-"+strconv.Itoa(cycle)); err != nil {
			t.Fatal(err)
		}
		if cycle%10 == 0 {
			if err := suite.stopService("relay-coordinator"); err != nil {
				t.Fatal(err)
			}
			if err := suite.startService("relay-coordinator"); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("completed recovery cycle %d", cycle)
	}
}
