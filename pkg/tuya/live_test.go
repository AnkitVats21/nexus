package tuya

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLive reads a real device's DPs. It runs only with TUYA_LIVE_ID,
// TUYA_LIVE_IP and TUYA_LIVE_KEY set.
func TestLive(t *testing.T) {
	d := Device{ID: os.Getenv("TUYA_LIVE_ID"), IP: os.Getenv("TUYA_LIVE_IP"), Key: os.Getenv("TUYA_LIVE_KEY")}
	if d.ID == "" || d.IP == "" || d.Key == "" {
		t.Skip("TUYA_LIVE_ID, TUYA_LIVE_IP, TUYA_LIVE_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := d.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	dps, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status: %v", dps)
	if _, ok := dps["20"]; !ok {
		t.Errorf("no DP 20 (switch) in %v", dps)
	}
}
