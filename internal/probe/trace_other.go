//go:build !linux

package probe

import "errors"

// NewTracer is unavailable outside Linux.
func NewTracer() (Tracer, error) { return nil, errors.New("traceroute is Linux-only") }
