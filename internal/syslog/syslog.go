// Package syslog receives RFC 3164 / RFC 5424 messages over UDP and TCP,
// stores them, and turns the ones that matter into normalised events.
package syslog

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
)

// Receiver listens and parses.
type Receiver struct {
	st     *store.Store
	logs   *LogStore
	Events chan model.Event
	// PerSourceRate is the sustained lines/second allowed per source IP.
	PerSourceRate int
	// Forward, when set, receives every line instead of the local store (cluster standby).
	Forward func(host, raw string)

	mu                sync.Mutex
	buckets           map[string]*bucket
	Received, Dropped int64
	TLSFailed         int64 // TLS handshakes that failed
	TLSLastErr        string
	unknownHosts      map[string]int64
}

type bucket struct {
	tokens float64
	last   time.Time
	flood  bool
}

// New builds a receiver.
func New(st *store.Store, logs *LogStore) *Receiver {
	return &Receiver{st: st, logs: logs, Events: make(chan model.Event, 4096), PerSourceRate: 2000, buckets: map[string]*bucket{}, unknownHosts: map[string]int64{}}
}

// ListenAndServe runs UDP and TCP listeners on addr (e.g. ":514") until ctx ends.
func (r *Receiver) ListenAndServe(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		pc.Close()
		ln.Close()
	}()
	go r.serveTCP(ctx, ln)
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		host, _, _ := net.SplitHostPort(from.String())
		r.Handle(host, string(buf[:n]))
	}
}

func (r *Receiver) serveTCP(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go r.serveConn(conn)
	}
}

// serveConn reads syslog frames from one stream: RFC 6587/5425 octet
// counting ("123 <34>…", the length may cover embedded newlines) or
// newline-delimited (non-transparent framing), decided per message.
func (r *Receiver) serveConn(c net.Conn) {
	defer c.Close()
	host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	if tc, ok := c.(*tls.Conn); ok {
		c.SetDeadline(time.Now().Add(15 * time.Second))
		if err := tc.Handshake(); err != nil {
			r.mu.Lock()
			r.TLSFailed++
			r.TLSLastErr = host + ": " + err.Error()
			r.mu.Unlock()
			return
		}
		c.SetDeadline(time.Time{})
	}
	br := bufio.NewReaderSize(c, 64<<10)
	for {
		b, err := br.Peek(1)
		if err != nil {
			return
		}
		var line string
		if b[0] >= '1' && b[0] <= '9' {
			// octet counting: decimal length, space, message
			head, err := br.ReadString(' ')
			if err != nil {
				return
			}
			n, err := strconv.Atoi(strings.TrimSpace(head))
			if err != nil || n <= 0 || n > 1<<20 {
				return
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(br, buf); err != nil {
				return
			}
			line = string(buf)
		} else {
			l, err := br.ReadString('\n')
			if err != nil && l == "" {
				return
			}
			line = strings.TrimRight(l, "\r\n")
			if err != nil {
				r.Handle(host, line)
				return
			}
		}
		r.Handle(host, line)
	}
}

// ListenAndServeTLS accepts syslog over TLS (RFC 5425) on addr (":6514").
// clientCA, when set, requires and verifies client certificates.
func (r *Receiver) ListenAndServeTLS(ctx context.Context, addr string, cfg *tls.Config) error {
	ln, err := tls.Listen("tcp", addr, cfg)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	r.serveTCP(ctx, ln)
	return nil
}

func (r *Receiver) allow(host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.buckets[host]
	now := time.Now()
	if b == nil {
		b = &bucket{tokens: float64(r.PerSourceRate), last: now}
		r.buckets[host] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * float64(r.PerSourceRate)
	if b.tokens > float64(r.PerSourceRate)*2 {
		b.tokens = float64(r.PerSourceRate) * 2
	}
	b.last = now
	if b.tokens < 1 {
		if !b.flood {
			b.flood = true
			r.mu.Unlock()
			r.emit(model.Event{Kind: "log_flood", Source: "syslog", Severity: model.SevMinor, Message: fmt.Sprintf("%s exceeds %d log lines/s — excess dropped", host, r.PerSourceRate), DedupKey: "log_flood:" + host, Attrs: map[string]string{"host": host}})
			r.mu.Lock()
		}
		return false
	}
	b.tokens--
	if b.flood && b.tokens > float64(r.PerSourceRate) {
		b.flood = false
	}
	return true
}

func (r *Receiver) emit(e model.Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	if e.Domain == "" {
		e.Domain = model.DomainNetwork
	}
	select {
	case r.Events <- e:
	default:
	}
}

// Handle processes one raw line from host.
func (r *Receiver) Handle(host, raw string) {
	r.mu.Lock()
	r.Received++
	r.mu.Unlock()
	if !r.allow(host) {
		r.mu.Lock()
		r.Dropped++
		r.mu.Unlock()
		return
	}
	if r.Forward != nil {
		r.Forward(host, raw)
		return
	}
	e := Parse(host, raw, time.Now())
	dev, ok := r.st.DeviceByIP(host)
	if ok {
		e.DeviceID = dev.ID
	} else {
		r.mu.Lock()
		r.unknownHosts[host]++
		r.mu.Unlock()
	}
	r.logs.Append(e)
	if ok {
		r.classify(dev, e)
	}
}

// UnknownHosts lists sources that sent logs but are not in the inventory.
func (r *Receiver) UnknownHosts() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.unknownHosts))
	for k, v := range r.unknownHosts {
		out[k] = v
	}
	return out
}

var (
	priRe     = regexp.MustCompile(`^<(\d{1,3})>`)
	rfc5424Re = regexp.MustCompile(`^1 (\S+) (\S+) (\S+) (\S+) (\S+) (?:-|\[.*?\]) ?(.*)$`)
	// Cisco: "123: *Sep  2 14:31:06.412 WIB: %LINK-3-UPDOWN: ..." or "Sep  2 14:31:06 host 123: %..."
	ciscoMnemonicRe = regexp.MustCompile(`%([A-Z0-9_]+-\d-[A-Z0-9_]+):`)
	bsdTimeRe       = regexp.MustCompile(`^(?:\d+: )?[*.]?([A-Z][a-z]{2} {1,2}\d{1,2} \d{2}:\d{2}:\d{2})(?:\.\d+)?(?: [A-Z]{2,5})?:? ?`)
	kvRe            = regexp.MustCompile(`(\w+)=("[^"]*"|\S+)`)
)

// Parse normalises one line. It never fails: unparseable input becomes an
// info-level entry with the raw text as message.
func Parse(host, raw string, recv time.Time) model.LogEntry {
	raw = strings.TrimRight(raw, "\r\n\x00")
	e := model.LogEntry{TS: recv, Recv: recv, Host: host, Facility: 1, Severity: 6, Source: "syslog", Message: raw}
	m := priRe.FindStringSubmatch(raw)
	rest := raw
	if m != nil {
		pri, _ := strconv.Atoi(m[1])
		e.Facility, e.Severity = pri/8, pri%8
		rest = raw[len(m[0]):]
	}
	if mm := rfc5424Re.FindStringSubmatch(rest); mm != nil {
		if ts, err := time.Parse(time.RFC3339Nano, mm[1]); err == nil {
			e.TS = ts
		}
		msg := mm[6]
		if mm[3] != "-" && mm[3] != "" {
			e.Mnemonic = mm[3] // APP-NAME
		}
		e.Message = strings.TrimSpace(msg)
	} else {
		// BSD-ish: optional sequence, timestamp, optional hostname, message
		if mm := bsdTimeRe.FindStringSubmatch(rest); mm != nil {
			if ts, err := time.Parse("Jan _2 15:04:05", mm[1]); err == nil {
				ts = ts.AddDate(recv.Year(), 0, 0)
				ts = time.Date(ts.Year(), ts.Month(), ts.Day(), ts.Hour(), ts.Minute(), ts.Second(), 0, recv.Location())
				if ts.After(recv.Add(48 * time.Hour)) {
					ts = ts.AddDate(-1, 0, 0)
				}
				e.TS = ts
			}
			rest = rest[len(mm[0]):]
		}
		e.Message = strings.TrimSpace(rest)
	}
	if mm := ciscoMnemonicRe.FindStringSubmatch(e.Message); mm != nil {
		e.Mnemonic = mm[1]
		parts := strings.Split(mm[1], "-")
		if len(parts) == 3 {
			if s, err := strconv.Atoi(parts[1]); err == nil {
				e.Severity = s
			}
		}
	} else if strings.Contains(e.Message, "logid=") {
		// Fortinet key=value
		kv := map[string]string{}
		for _, m := range kvRe.FindAllStringSubmatch(e.Message, -1) {
			kv[m[1]] = strings.Trim(m[2], `"`)
		}
		if id := kv["logid"]; id != "" {
			e.Mnemonic = "FGT-" + kv["type"] + "-" + kv["subtype"]
			if kv["logdesc"] != "" {
				e.Mnemonic += "-" + strings.ReplaceAll(strings.ToUpper(kv["logdesc"]), " ", "_")
			}
		}
		switch kv["level"] {
		case "emergency":
			e.Severity = 0
		case "alert":
			e.Severity = 1
		case "critical":
			e.Severity = 2
		case "error":
			e.Severity = 3
		case "warning":
			e.Severity = 4
		case "notice":
			e.Severity = 5
		case "information":
			e.Severity = 6
		}
	}
	// Devices with a wrong clock: trust arrival time when far off.
	if d := e.TS.Sub(recv); d > 5*time.Minute || d < -24*time.Hour {
		e.TS = recv
	}
	return e
}

var (
	ifNameRe  = regexp.MustCompile(`(?i)interface ([A-Za-z\-]+[\d/\.:]+)`)
	bgpPeerRe = regexp.MustCompile(`neighbor ([\d\.:a-fA-F]+)`)
	loginUser = regexp.MustCompile(`(?i)user[:= ]+'?([A-Za-z0-9_\-.@]+)'?`)
	fromIPRe  = regexp.MustCompile(`(?i)(?:from|by) ([\d\.]+)`)
	changedBy = regexp.MustCompile(`(?i)configured from \S+ by (\S+)`)
)

// classify maps a stored entry to zero or one normalised event.
func (r *Receiver) classify(dev model.Device, e model.LogEntry) {
	msg := e.Message
	up := strings.ToUpper(e.Mnemonic)
	ev := model.Event{TS: e.TS, DeviceID: dev.ID, Source: "syslog", Domain: dev.Domain, Severity: model.SevInfo, Attrs: map[string]string{}}
	switch {
	case strings.HasPrefix(up, "LINK-") && strings.Contains(up, "UPDOWN"), strings.HasPrefix(up, "LINEPROTO-") && strings.Contains(up, "UPDOWN"):
		m := ifNameRe.FindStringSubmatch(msg)
		if m == nil {
			return
		}
		ev.Attrs["ifName"] = m[1]
		if i, ok := r.st.InterfaceByName(dev.ID, m[1]); ok {
			ev.Object = i.ID
		} else if i, ok := r.st.InterfaceByName(dev.ID, shortIf(m[1])); ok {
			ev.Object = i.ID
		}
		if strings.Contains(strings.ToLower(msg), "to down") {
			ev.Kind, ev.Severity = "link_down", model.SevMajor
			ev.Message = fmt.Sprintf("%s: %s down (syslog)", dev.Name, m[1])
		} else {
			ev.Kind, ev.Severity = "link_up", model.SevInfo
			ev.Message = fmt.Sprintf("%s: %s up (syslog)", dev.Name, m[1])
		}
		ev.DedupKey = ev.Kind + ":" + ev.Object
	case strings.Contains(up, "CONFIG_I"), strings.Contains(up, "CFGLOG_LOGGEDCMD"), strings.Contains(up, "CONFIG-CHANGE"), strings.Contains(strings.ToLower(msg), "configuration changed"), strings.Contains(strings.ToLower(msg), "config changed"):
		ev.Kind, ev.Severity = "config_changed", model.SevMinor
		who := ""
		if m := changedBy.FindStringSubmatch(msg); m != nil {
			who = m[1]
		} else if m := loginUser.FindStringSubmatch(msg); m != nil {
			who = m[1]
		}
		from := ""
		if m := fromIPRe.FindStringSubmatch(msg); m != nil {
			from = m[1]
		}
		ev.Attrs["user"], ev.Attrs["from"] = who, from
		ev.Message = fmt.Sprintf("%s: configuration changed by %s %s", dev.Name, orUnknown(who), from)
		ev.DedupKey = "config_changed:" + dev.ID + ":" + time.Now().Format("200601021504")
	case strings.HasPrefix(up, "BGP-") && strings.Contains(up, "ADJCHANGE"):
		peer := ""
		if m := bgpPeerRe.FindStringSubmatch(msg); m != nil {
			peer = m[1]
		}
		ev.Attrs["peer"] = peer
		if strings.Contains(msg, "Down") || strings.Contains(strings.ToLower(msg), " down") {
			ev.Kind, ev.Severity = "bgp_neighbor_down", model.SevMajor
			ev.Message = fmt.Sprintf("%s: BGP neighbor %s down", dev.Name, peer)
		} else {
			ev.Kind = "bgp_neighbor_up"
			ev.Message = fmt.Sprintf("%s: BGP neighbor %s up", dev.Name, peer)
		}
		ev.DedupKey = "bgp:" + dev.ID + ":" + peer
	case strings.HasPrefix(up, "OSPF-") && strings.Contains(up, "ADJCHG"):
		if strings.Contains(msg, "to DOWN") {
			ev.Kind, ev.Severity = "ospf_adjacency_down", model.SevMajor
		} else {
			ev.Kind = "ospf_adjacency_up"
		}
		ev.Message = dev.Name + ": " + msg
		ev.DedupKey = "ospf:" + dev.ID
	case strings.Contains(up, "LOGIN_FAILED"), strings.Contains(up, "AUTHFAIL"), strings.Contains(strings.ToLower(msg), "authentication failed"), strings.Contains(strings.ToLower(msg), "login failed"):
		ev.Kind, ev.Severity = "auth_failure", model.SevInfo
		if m := fromIPRe.FindStringSubmatch(msg); m != nil {
			ev.Attrs["from"] = m[1]
		}
		ev.Message = dev.Name + ": " + firstN(msg, 160)
		ev.DedupKey = "auth_failure:" + dev.ID
	case strings.Contains(up, "-HA-") || strings.Contains(up, "HA_STATE") || strings.Contains(strings.ToLower(msg), "ha state changed") || strings.Contains(strings.ToLower(msg), "failover"):
		ev.Kind, ev.Severity = "ha_state_change", model.SevMajor
		ev.Message = dev.Name + ": " + firstN(msg, 160)
		ev.DedupKey = "ha:" + dev.ID
	case strings.Contains(up, "ENVMON") || strings.Contains(up, "ENVIRONMENT") || strings.Contains(up, "PLATFORM_ENV") || strings.Contains(up, "-FAN-") || strings.Contains(up, "-PSU-") || strings.Contains(up, "POWER_SUPPLY"):
		ev.Kind, ev.Severity = "environment_fault", model.SevMajor
		if e.Severity >= 5 {
			ev.Severity = model.SevInfo
		}
		ev.Message = dev.Name + ": " + firstN(msg, 160)
		ev.DedupKey = "env:" + dev.ID
	case strings.Contains(up, "VPN") && (strings.Contains(strings.ToLower(msg), "down") || strings.Contains(strings.ToLower(msg), "tunnel")):
		ev.Kind, ev.Severity = "vpn_tunnel_change", model.SevMinor
		ev.Message = dev.Name + ": " + firstN(msg, 160)
		ev.DedupKey = "vpn:" + dev.ID
	case e.Severity <= 2: // emerg/alert/crit from anything
		ev.Kind, ev.Severity = "critical_log", model.SevMajor
		ev.Message = fmt.Sprintf("%s: %s %s", dev.Name, e.Mnemonic, firstN(msg, 160))
		ev.DedupKey = "critical_log:" + dev.ID + ":" + e.Mnemonic
	default:
		return
	}
	r.emit(ev)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown user"
	}
	return s
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// shortIf abbreviates a long Cisco interface name.
func shortIf(s string) string {
	for _, r := range []struct{ l, s string }{{"TenGigabitEthernet", "Te"}, {"GigabitEthernet", "Gi"}, {"FastEthernet", "Fa"}, {"Port-channel", "Po"}, {"HundredGigE", "Hu"}, {"FortyGigabitEthernet", "Fo"}, {"TwentyFiveGigE", "Twe"}, {"Ethernet", "Eth"}} {
		if strings.HasPrefix(s, r.l) {
			return r.s + s[len(r.l):]
		}
	}
	return s
}
