package icmp

import "time"

// Result of one probe cycle.
type Result struct {
	Sent, Received                 int
	MinRTT, AvgRTT, MaxRTT, Jitter time.Duration
	LossPct                        float64
}

// Reachable reports whether any reply arrived.
func (r Result) Reachable() bool { return r.Received > 0 }
