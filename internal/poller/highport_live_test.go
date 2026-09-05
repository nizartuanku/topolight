package poller

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/profile"
	"github.com/nizartuanku/topolight/internal/snmp"
)

// Live check against a real net-snmp agent on a high port. Set
// TOPOLIGHT_HIGHPORT=127.0.0.1:11161 to run it; without it the test is skipped.
func TestLive_HighPortAgent(t *testing.T) {
	addr := os.Getenv("TOPOLIGHT_HIGHPORT")
	if addr == "" {
		t.Skip("set TOPOLIGHT_HIGHPORT=host:port to run")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("TOPOLIGHT_HIGHPORT must be host:port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port %q: %v", portStr, err)
	}

	// 1. with the credential port: the agent answers.
	c := NewClient(host, model.Credential{Version: "2c", Community: "labpublic", Port: port})
	c.Timeout = 2 * time.Second
	defer c.Close()
	vbs, err := c.Get(profile.OIDSysName, profile.OIDSysDescr, profile.OIDSysObjectID)
	if err != nil {
		t.Fatalf("high-port poll failed: %v", err)
	}
	name := snmp.PrintableOrHex(vbs[0].Value.Bytes)
	t.Logf("sysName over udp/%d = %q", port, name)
	if name == "" {
		t.Fatal("empty sysName")
	}

	// 2. without it: the old behaviour, dialling 161, gets nothing. This is
	//    the bug the option exists to fix.
	old := NewClient(host, model.Credential{Version: "2c", Community: "labpublic"})
	old.Timeout = 900 * time.Millisecond
	old.Retries = 0
	defer old.Close()
	if _, err := old.Get(profile.OIDSysName); err == nil {
		t.Fatal("port 161 answered; the lab agent is not where this test thinks it is")
	} else {
		t.Logf("without the port option: %v (expected)", err)
	}

	// 3. v3 on the same high port.
	v3 := NewClient(host, model.Credential{Version: "3", User: "topolight", AuthProto: "sha", AuthPass: "authpass123", PrivProto: "aes", PrivPass: "privpass123", Port: port})
	v3.Timeout = 3 * time.Second
	defer v3.Close()
	vbs3, err := v3.Get(profile.OIDSysName)
	if err != nil {
		t.Fatalf("v3 high-port poll failed: %v", err)
	}
	t.Logf("v3 sysName over udp/%d = %q", port, snmp.PrintableOrHex(vbs3[0].Value.Bytes))
}
