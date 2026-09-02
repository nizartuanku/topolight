//go:build !linux

// On non-Linux hosts the syscall pinger is not available; TopoLight runs in
// SNMP-only reachability mode (the console explains this on the Overview).
package icmp

import (
	"errors"
	"time"
)

// Pinger is a placeholder on this platform.
type Pinger struct{}

// New always fails here so the caller can fall back to SNMP-only mode.
func New() (*Pinger, error) {
	return nil, errors.New("ICMP probing is only available on Linux in this release")
}

// Probe is never reached because New fails.
func (p *Pinger) Probe(ip string, count int, interval, timeout time.Duration) (Result, error) {
	return Result{}, errors.New("icmp unavailable on this platform")
}
