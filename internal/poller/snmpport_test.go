package poller

import (
	"net"
	"strconv"
	"testing"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/snmp"
)

// A credential with no port must keep dialling the well-known 161, because
// every credential that existed before this field did has Port == 0.
func TestNewClient_NoPortKeepsWellKnown(t *testing.T) {
	c := NewClient("192.0.2.10", model.Credential{Version: "2c", Community: "public"})
	if got, want := c.Addr, "192.0.2.10:"+strconv.Itoa(snmp.DefaultPort); got != want {
		t.Fatalf("Addr = %q, want %q", got, want)
	}
}

// The bug this test exists for: an agent on a high port was unreachable, since
// the port was fixed at 161 with no way to say otherwise.
func TestNewClient_CredentialPortIsUsed(t *testing.T) {
	c := NewClient("192.0.2.10", model.Credential{Version: "2c", Community: "public", Port: 11161})
	if c.Addr != "192.0.2.10:11161" {
		t.Fatalf("Addr = %q, want 192.0.2.10:11161", c.Addr)
	}
	if c.Version != snmp.V2c || c.Community != "public" {
		t.Fatalf("port must not disturb the rest of the credential: %+v", c)
	}
}

// v3 goes down the same path; the context and user must survive it.
func TestNewClient_V3WithPort(t *testing.T) {
	c := NewClient("192.0.2.11", model.Credential{Version: "3", User: "topolight", AuthProto: "sha", AuthPass: "x", Port: 16161})
	if c.Addr != "192.0.2.11:16161" || c.Version != snmp.V3 || c.User != "topolight" {
		t.Fatalf("unexpected client: addr=%q ver=%v user=%q", c.Addr, c.Version, c.User)
	}
}

// An IPv6 literal has to come back bracketed or the dial fails.
func TestNewClient_IPv6IsBracketed(t *testing.T) {
	c := NewClient("2001:db8::1", model.Credential{Version: "2c", Community: "public", Port: 11161})
	if c.Addr != "[2001:db8::1]:11161" {
		t.Fatalf("Addr = %q, want [2001:db8::1]:11161", c.Addr)
	}
	if host, port, err := net.SplitHostPort(c.Addr); err != nil || host != "2001:db8::1" || port != "11161" {
		t.Fatalf("SplitHostPort(%q) = %q, %q, %v", c.Addr, host, port, err)
	}
}

// A device entered as host:port predates this field and must still win, rather
// than getting a second port appended to it.
func TestNewClient_AddressAlreadyCarryingAPortIsLeftAlone(t *testing.T) {
	c := NewClient("192.0.2.12:11161", model.Credential{Version: "2c", Community: "public"})
	if c.Addr != "192.0.2.12:11161" {
		t.Fatalf("Addr = %q, want 192.0.2.12:11161", c.Addr)
	}
}

// Out-of-range values are rejected at the API, but the poller must not dial a
// nonsense port if one ever reaches the store another way.
func TestSNMPPort_OutOfRangeFallsBack(t *testing.T) {
	for _, p := range []int{-1, 0, 65536, 99999} {
		if got := SNMPPort(model.Credential{Port: p}); got != snmp.DefaultPort {
			t.Fatalf("SNMPPort(%d) = %d, want %d", p, got, snmp.DefaultPort)
		}
	}
}
