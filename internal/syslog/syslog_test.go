package syslog

import (
	"testing"
	"time"
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
