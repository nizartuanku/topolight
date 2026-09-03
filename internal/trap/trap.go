// Package trap receives SNMP v2c notifications on UDP/162, stores them as log
// entries and maps the well-known ones to events.
package trap

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/snmp"
	"github.com/nizartuanku/topolight/internal/store"
	"github.com/nizartuanku/topolight/internal/syslog"
)

// Receiver decodes traps.
type Receiver struct {
	st     *store.Store
	logs   *syslog.LogStore
	Events chan model.Event
	// Community, when set, is required on incoming traps (v2c).
	Community string
	// PollNow lets the receiver ask for an immediate re-poll after a trap.
	PollNow func(deviceID string)
	// V3 authenticates SNMPv3 notifications with the saved v3 credentials.
	V3 *snmp.V3Receiver
	// Forward, when set, ships raw datagrams to the leader instead of decoding them here
	// (cluster standby). Informs are still acknowledged locally.
	Forward func(from string, raw []byte)

	mu         sync.Mutex
	Received   int64
	Rejected   int64
	V3Received int64
	V3Rejected int64
	V3LastErr  string
	last       map[string]time.Time // per (device, oid) rate limit
}

// New builds a receiver.
func New(st *store.Store, logs *syslog.LogStore) *Receiver {
	r := &Receiver{st: st, logs: logs, Events: make(chan model.Event, 4096), last: map[string]time.Time{}}
	r.V3 = snmp.NewV3Receiver(func() []snmp.V3User {
		var out []snmp.V3User
		for _, c := range st.Creds() {
			if c.Version == "3" && c.User != "" {
				out = append(out, snmp.V3User{User: c.User, AuthProto: c.AuthProto, AuthPass: c.AuthPass, PrivProto: c.PrivProto, PrivPass: c.PrivPass})
			}
		}
		return out
	})
	return r
}

// ListenAndServe binds addr (":162") until ctx ends.
func (r *Receiver) ListenAndServe(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		pc.Close()
	}()
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		raw := append([]byte(nil), buf[:n]...)
		if r.Forward != nil {
			host, _, _ := net.SplitHostPort(from.String())
			// acknowledge informs here so the sender stops retrying; the leader decodes the copy
			if !snmp.IsV3(raw) {
				if t, err := snmp.DecodeTrap(from, raw); err == nil && t.Inform {
					if resp := snmp.InformResponse(t.Community, snmp.PDU{RequestID: t.RequestID, VarBinds: t.VarBinds}); resp != nil {
						pc.WriteTo(resp, from)
					}
				}
			}
			r.Forward(host, raw)
			continue
		}
		if snmp.IsV3(raw) {
			t, resp, err := r.V3.Decode(from, raw)
			if err == snmp.ErrV3Discovery {
				pc.WriteTo(resp, from)
				continue
			}
			r.mu.Lock()
			if err != nil {
				r.V3Rejected++
				r.V3LastErr = from.String() + ": " + err.Error()
			} else {
				r.V3Received++
			}
			r.mu.Unlock()
			if err != nil {
				continue
			}
			if resp != nil {
				pc.WriteTo(resp, from)
			}
			r.Handle(t)
			continue
		}
		t, err := snmp.DecodeTrap(from, raw)
		if err != nil {
			r.mu.Lock()
			r.Rejected++
			r.mu.Unlock()
			continue
		}
		if t.Inform {
			// acknowledge before processing
			if resp := snmp.InformResponse(t.Community, snmp.PDU{RequestID: t.RequestID, VarBinds: t.VarBinds}); resp != nil {
				pc.WriteTo(resp, from)
			}
		}
		r.Handle(t)
	}
}

// HandleRaw decodes a forwarded datagram (leader side of a cluster).
func (r *Receiver) HandleRaw(from string, raw []byte) {
	addr := &net.UDPAddr{IP: net.ParseIP(from), Port: 0}
	if snmp.IsV3(raw) {
		t, _, err := r.V3.Decode(addr, raw)
		r.mu.Lock()
		if err != nil {
			r.V3Rejected++
			r.V3LastErr = from + ": " + err.Error()
		} else {
			r.V3Received++
		}
		r.mu.Unlock()
		if err == nil {
			r.Handle(t)
		}
		return
	}
	t, err := snmp.DecodeTrap(addr, raw)
	if err != nil {
		r.mu.Lock()
		r.Rejected++
		r.mu.Unlock()
		return
	}
	r.Handle(t)
}

// Handle processes one decoded trap.
func (r *Receiver) Handle(t snmp.Trap) {
	r.mu.Lock()
	r.Received++
	r.mu.Unlock()
	if r.Community != "" && t.V3User == "" && t.Community != r.Community {
		r.mu.Lock()
		r.Rejected++
		r.mu.Unlock()
		return
	}
	now := time.Now()
	dev, known := r.st.DeviceByIP(t.From)
	name := t.From
	if known {
		name = dev.Name
	}
	kind, sev, msg, object := r.describe(dev, known, t)
	entry := model.LogEntry{TS: now, Recv: now, Host: t.From, Facility: 23, Severity: 5, Source: "trap", Mnemonic: trapName(t.TrapOID), Message: msg}
	if known {
		entry.DeviceID = dev.ID
	}
	if sev == model.SevMajor || sev == model.SevCritical {
		entry.Severity = 3
	}
	r.logs.Append(entry)
	if !known || kind == "" {
		return
	}
	// Storms: one event per (device, kind, object) per 2 s.
	key := dev.ID + "|" + kind + "|" + object
	r.mu.Lock()
	if last, ok := r.last[key]; ok && now.Sub(last) < 2*time.Second {
		r.mu.Unlock()
		return
	}
	r.last[key] = now
	if len(r.last) > 50000 {
		r.last = map[string]time.Time{}
	}
	r.mu.Unlock()
	ev := model.Event{TS: now, Kind: kind, DeviceID: dev.ID, Object: object, Source: "trap", Severity: sev, Domain: dev.Domain, Message: msg,
		DedupKey: kind + ":" + firstNonEmpty(object, dev.ID), Attrs: map[string]string{"trap_oid": t.TrapOID, "device": name}}
	select {
	case r.Events <- ev:
	default:
	}
	if r.PollNow != nil {
		switch kind {
		case "link_down", "link_up", "device_rebooted":
			r.PollNow(dev.ID)
		}
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

const (
	oidIfIndex = "1.3.6.1.2.1.2.2.1.1"
	oidIfDescr = "1.3.6.1.2.1.2.2.1.2"
	oidIfName  = "1.3.6.1.2.1.31.1.1.1.1"
)

func (r *Receiver) describe(dev model.Device, known bool, t snmp.Trap) (kind string, sev model.Severity, msg string, object string) {
	name := t.From
	if known {
		name = dev.Name
	}
	ifRef := func() (string, string) {
		idx := -1
		if vb, ok := t.Get(oidIfIndex); ok {
			idx = int(vb.Value.Int)
		}
		label := ""
		if vb, ok := t.Get(oidIfName); ok {
			label = snmp.PrintableOrHex(vb.Value.Bytes)
		} else if vb, ok := t.Get(oidIfDescr); ok {
			label = snmp.PrintableOrHex(vb.Value.Bytes)
		}
		obj := ""
		if known && idx >= 0 {
			obj = model.IfID(dev.ID, idx)
			if label == "" {
				if i, err := r.st.Interface(obj); err == nil {
					label = i.Name
				}
			}
		}
		if label == "" && idx >= 0 {
			label = "ifIndex " + strconv.Itoa(idx)
		}
		return obj, label
	}
	switch t.TrapOID {
	case snmp.TrapLinkDown:
		obj, label := ifRef()
		return "link_down", model.SevMajor, fmt.Sprintf("%s: %s down (trap)", name, label), obj
	case snmp.TrapLinkUp:
		obj, label := ifRef()
		return "link_up", model.SevInfo, fmt.Sprintf("%s: %s up (trap)", name, label), obj
	case snmp.TrapColdStart, snmp.TrapWarmStart:
		return "device_rebooted", model.SevMajor, fmt.Sprintf("%s: %s (trap)", name, trapName(t.TrapOID)), ""
	case snmp.TrapAuthFailure:
		return "auth_failure", model.SevInfo, fmt.Sprintf("%s: SNMP authentication failure (trap)", name), ""
	}
	// Vendor traps: keep them as log entries, classify a few by OID prefix.
	switch {
	case strings.HasPrefix(t.TrapOID, "1.3.6.1.2.1.15.7"): // BGP4-MIB
		if strings.HasSuffix(t.TrapOID, ".2") {
			return "bgp_neighbor_down", model.SevMajor, fmt.Sprintf("%s: BGP backward transition (trap)", name), ""
		}
		return "bgp_neighbor_up", model.SevInfo, fmt.Sprintf("%s: BGP established (trap)", name), ""
	case strings.HasPrefix(t.TrapOID, "1.3.6.1.4.1.9.9.13.3"): // CISCO-ENVMON traps
		return "environment_fault", model.SevMajor, fmt.Sprintf("%s: environment notification %s (trap)", name, trapName(t.TrapOID)), ""
	case strings.HasPrefix(t.TrapOID, "1.3.6.1.4.1.12356.101.2.0"): // FortiGate
		return "vendor_trap", model.SevMinor, fmt.Sprintf("%s: FortiGate trap %s", name, t.TrapOID), ""
	}
	return "vendor_trap", model.SevInfo, fmt.Sprintf("%s: trap %s", name, t.TrapOID), ""
}

func trapName(oid string) string {
	switch oid {
	case snmp.TrapColdStart:
		return "coldStart"
	case snmp.TrapWarmStart:
		return "warmStart"
	case snmp.TrapLinkDown:
		return "linkDown"
	case snmp.TrapLinkUp:
		return "linkUp"
	case snmp.TrapAuthFailure:
		return "authenticationFailure"
	}
	return oid
}
