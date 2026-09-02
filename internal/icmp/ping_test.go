//go:build linux

package icmp

import (
	"testing"
	"time"
)

func TestLoopback(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Skip(err)
	}
	defer p.Close()
	r, err := p.Probe("127.0.0.1", 3, 50*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if r.Received != 3 || r.LossPct != 0 || r.AvgRTT <= 0 {
		t.Fatalf("unexpected %+v", r)
	}
	// An address that never answers (TEST-NET-1) must report loss, not hang.
	start := time.Now()
	r, err = p.Probe("192.0.2.1", 2, 10*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// Some sandboxes answer for every address; only the time bound is asserted.
	if time.Since(start) > 2*time.Second {
		t.Fatalf("probe did not respect timeout: %+v in %s", r, time.Since(start))
	}
	if r.Sent != 2 {
		t.Fatalf("sent %d", r.Sent)
	}
}
