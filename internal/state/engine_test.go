package state

import (
	"context"
	"testing"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
)

// A core → dist → access chain: when dist and access both stop answering,
// dist must become "down" with a major alert and access must be folded into
// "unreachable" with dist as the cause. This also guards against the engine
// deadlocking while it consults the topology under its own lock.
func TestDownstreamSuppression(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	mk := func(id, name string, role model.Role) model.Device {
		d := model.Device{ID: id, Name: name, IP: "10.0.0." + id[len(id)-1:], SiteID: "site1", Role: role, Status: model.StatusUp, Monitored: true, PollEvery: 60, Created: now}
		st.PutDevice(d)
		return d
	}
	mk("dev1", "core", model.RoleCore)
	mk("dev2", "dist", model.RoleDist)
	mk("dev3", "access", model.RoleAccess)
	st.PutLink(model.Link{ID: "l1", ADevice: "dev1", AIf: "e1", BDevice: "dev2", BIf: "e1", Confidence: 1, FirstSeen: now, LastSeen: now})
	st.PutLink(model.Link{ID: "l2", ADevice: "dev2", AIf: "e2", BDevice: "dev3", BIf: "e1", Confidence: 1, FirstSeen: now, LastSeen: now})

	eng := New(st)
	devs := make(chan model.DeviceSample, 64)
	eng.Devices = devs
	eng.Interfaces = make(chan model.InterfaceSample)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Run(ctx)

	ok := func(id string) model.DeviceSample {
		return model.DeviceSample{DeviceID: id, TS: time.Now(), Reachable: true, SNMPOK: true, Uptime: 100, CPU: 10, MemPct: 10, TempC: -1000, Sessions: -1}
	}
	bad := func(id string) model.DeviceSample {
		return model.DeviceSample{DeviceID: id, TS: time.Now(), Uptime: -1, CPU: -1, MemPct: -1, TempC: -1000, Sessions: -1, Err: "snmp: timeout"}
	}
	for _, id := range []string{"dev1", "dev2", "dev3"} {
		devs <- ok(id)
	}
	// dist and access go silent for 3 cycles each
	for i := 0; i < 3; i++ {
		devs <- bad("dev2")
		devs <- bad("dev3")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d2, _ := st.Device("dev2")
		d3, _ := st.Device("dev3")
		if d2.Status == model.StatusDown && d3.Status == model.StatusUnreachable {
			if d3.Cause != "dev2" {
				t.Fatalf("access cause = %q, want dev2", d3.Cause)
			}
			var root, child *model.Alert
			for _, a := range st.Alerts() {
				a := a
				switch a.DeviceID {
				case "dev2":
					root = &a
				case "dev3":
					child = &a
				}
			}
			if root == nil || root.Severity != model.SevMajor || root.State != "open" {
				t.Fatalf("root alert = %+v", root)
			}
			if child == nil || child.RootCause != root.ID {
				t.Fatalf("child alert not folded: %+v", child)
			}
			// recovery: both answer again for 2 cycles
			for i := 0; i < 2; i++ {
				devs <- ok("dev2")
				devs <- ok("dev3")
			}
			deadline2 := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline2) {
				d2, _ := st.Device("dev2")
				d3, _ := st.Device("dev3")
				if d2.Status == model.StatusUp && d3.Status == model.StatusUp {
					for _, a := range st.Alerts() {
						if a.State == "open" && a.Rule == "device_down" {
							t.Fatalf("alert still open after recovery: %+v", a)
						}
					}
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("devices did not recover")
		}
		time.Sleep(20 * time.Millisecond)
	}
	d2, _ := st.Device("dev2")
	d3, _ := st.Device("dev3")
	t.Fatalf("timeout (engine deadlock?): dist=%s access=%s", d2.Status, d3.Status)
}
