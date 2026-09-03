package endpoint

import (
	"testing"
	"time"
)

func TestNormMAC(t *testing.T) {
	for in, want := range map[string]string{"00:1C:73:AA:BB:CC": "00:1c:73:aa:bb:cc", "001c.73aa.bbcc": "00:1c:73:aa:bb:cc", "00-1c-73-aa-bb-cc": "00:1c:73:aa:bb:cc", "001c73aabbc": "", "": ""} {
		if got := NormMAC(in); got != want {
			t.Errorf("%q → %q want %q", in, got, want)
		}
	}
}

func TestPlacementPrefersQuietPort(t *testing.T) {
	s, _ := Open("")
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	// core sees everything on its trunk (ifIndex 49), plus access sees the PC on port 7
	var core []FDBEntry
	for i := 0; i < 50; i++ {
		core = append(core, FDBEntry{MAC: "00:1c:73:00:00:" + string(rune('a'+i%6)) + string(rune('a'+i/6)), IfIndex: 49, IfName: "Eth1/49", VLAN: 10})
	}
	core = append(core, FDBEntry{MAC: "3c:5a:37:11:22:33", IfIndex: 49, IfName: "Eth1/49", VLAN: 10})
	s.ObserveFDB("core", core, nil, now)
	acc := []FDBEntry{{MAC: "3c:5a:37:11:22:33", IfIndex: 7, IfName: "1/1/7", VLAN: 10}, {MAC: "3c:5a:37:11:22:33", IfIndex: 52, IfName: "1/1/52", VLAN: 10}}
	mv := s.ObserveFDB("acc", acc, map[int]bool{52: true}, now.Add(time.Minute))
	if len(mv) != 0 {
		t.Fatalf("first placement is not a move: %+v", mv)
	}
	e, ok := s.Get("3C5A37112233")
	if !ok || e.DeviceID != "acc" || e.IfIndex != 7 || e.Vendor == "" || e.Ports != 2 {
		t.Fatalf("placement: %+v", e)
	}
	// the PC moves to port 9 on the same switch
	mv = s.ObserveFDB("acc", []FDBEntry{{MAC: "3c:5a:37:11:22:33", IfIndex: 9, IfName: "1/1/9", VLAN: 10}}, nil, now.Add(2*time.Minute))
	if len(mv) != 1 || mv[0].FromIf != "1/1/7" || mv[0].ToIf != "1/1/9" {
		t.Fatalf("move: %+v", mv)
	}
	// ARP adds the IP
	s.ObserveARP("rtr", []ARPEntry{{MAC: "3C:5A:37:11:22:33", IP: "10.10.20.15", IfIndex: 3}}, now.Add(3*time.Minute))
	e, _ = s.Get("3c:5a:37:11:22:33")
	if len(e.IPs) != 1 || e.IPs[0] != "10.10.20.15" || e.ARPDevice != "rtr" || e.Moves != 1 {
		t.Fatalf("arp merge: %+v", e)
	}
	// queries
	if r := s.Query("10.10.20", "", 0, 10); len(r) != 1 {
		t.Fatalf("ip query: %d", len(r))
	}
	if r := s.Query("samsung", "", 0, 10); len(r) != 1 {
		t.Fatalf("vendor query: %d", len(r))
	}
	if r := s.Query("3c:5a:37", "", 0, 10); len(r) != 1 {
		t.Fatalf("partial mac query: %d", len(r))
	}
	if r := s.Query("", "acc", 9, 10); len(r) != 1 {
		t.Fatalf("port query: %d", len(r))
	}
	// MACs seen only on the core trunk stay there; the PC is not among them
	if c := s.PortCounts("core"); c[49] == 0 || c[49] != len(s.Query("", "core", 49, 0)) {
		t.Fatalf("core port counts: %v", c)
	}
	for _, x := range s.Query("", "core", 49, 0) {
		if x.MAC == "3c:5a:37:11:22:33" {
			t.Fatal("PC placed on the trunk")
		}
	}
	// multicast never stored
	s.ObserveFDB("acc", []FDBEntry{{MAC: "01:00:5e:00:00:fb", IfIndex: 9}}, nil, now.Add(4*time.Minute))
	if _, ok := s.Get("01:00:5e:00:00:fb"); ok {
		t.Fatal("multicast stored")
	}
}

func TestPersist(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	now := time.Now()
	s.ObserveARP("r", []ARPEntry{{MAC: "00:1c:73:00:00:01", IP: "10.0.0.1"}}, now)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := Open(dir)
	if e, ok := b.Get("00:1c:73:00:00:01"); !ok || e.IPs[0] != "10.0.0.1" {
		t.Fatalf("reload: %+v %v", e, ok)
	}
	if n := b.Prune(time.Hour, now.Add(2*time.Hour)); n != 1 {
		t.Fatalf("prune: %d", n)
	}
}
