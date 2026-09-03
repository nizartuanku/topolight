package syslog

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/nizartuanku/topolight/internal/selfcert"
	"github.com/nizartuanku/topolight/internal/store"
)

func TestParse(t *testing.T) {
	recv := time.Date(2026, 9, 2, 14, 31, 10, 0, time.UTC)
	cases := []struct {
		raw       string
		sev       int
		mnemonic  string
		msgPrefix string
	}{
		{"<187>1234567: Sep  2 14:31:06.412 UTC: %LINK-3-UPDOWN: Interface GigabitEthernet1/0/10, changed state to down", 3, "LINK-3-UPDOWN", "%LINK-3-UPDOWN"},
		{"<189>Sep  2 14:33:12 sw1 1234570: %SYS-5-CONFIG_I: Configured from console by admin on vty0 (10.20.9.15)", 5, "SYS-5-CONFIG_I", "sw1"},
		{`<134>1 2026-09-02T14:31:09.000Z fw-dc-01 fortigate - - - date=2026-09-02 time=14:31:09 devname="FW-DC-01" logid="0100022002" type="event" subtype="ha" level="critical" logdesc="HA state change" msg="HA state changed"`, 2, "FGT-event-ha-HA_STATE_CHANGE", "date="},
		{"garbage without pri", 6, "", "garbage"},
	}
	for _, c := range cases {
		e := Parse("10.0.0.1", c.raw, recv)
		if e.Severity != c.sev || e.Mnemonic != c.mnemonic || len(e.Message) < len(c.msgPrefix) || e.Message[:len(c.msgPrefix)] != c.msgPrefix {
			t.Fatalf("%q -> %+v", c.raw, e)
		}
	}
	e := Parse("10.0.0.1", "<187>1234567: Sep  2 14:31:06.412 UTC: %LINK-3-UPDOWN: x", recv)
	if e.TS.Hour() != 14 || e.TS.Minute() != 31 || e.TS.Second() != 6 || e.TS.Year() != 2026 {
		t.Fatalf("timestamp %v", e.TS)
	}
}

func TestFramingAndTLS(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := OpenLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := New(st, logs)
	cert, _, _, err := selfcert.Load(dir, "test", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	go r.serveTCP(ctx, ln)
	defer ln.Close()
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	m1 := "<134>1 2026-09-03T02:00:00Z fw1 - - - - first line\nwith embedded newline"
	m2 := "<187>Sep  3 02:00:01 sw1 1: %LINK-3-UPDOWN: Interface Gi1/0/1, changed state to down"
	fmt.Fprintf(c, "%d %s", len(m1), m1) // octet counting, newline inside
	fmt.Fprintf(c, "%s\n", m2)           // non-transparent
	fmt.Fprintf(c, "%d %s", len(m2), m2) // octet counting again
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := r.Received
		r.mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Received != 3 {
		t.Fatalf("received %d, want 3", r.Received)
	}
	// plain TCP client to the TLS port fails the handshake and is counted
	p, _ := net.Dial("tcp", ln.Addr().String())
	p.Write([]byte("<13>not tls\n"))
	p.Close()
	r.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	r.mu.Lock()
	if r.TLSFailed != 1 {
		t.Fatalf("tls failures %d", r.TLSFailed)
	}
}
