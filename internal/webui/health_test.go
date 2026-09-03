package webui

import (
	"testing"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
)

func TestDeviceHealthSummary(t *testing.T) {
	now := time.Now()
	d := model.Device{ID: "d1", Name: "dist-sw1", IP: "10.0.0.2", Role: model.RoleDist, Status: model.StatusDegraded, Monitored: true, SNMPOK: true,
		Metrics: map[string]float64{"cpu_pct": 41, "mem_pct": 63, "rtt_ms": 1.2, "loss_pct": 0}}
	ifs := []model.Interface{
		// uplink, busy, with drops
		{ID: "d1:1", Name: "Te1/1", Kind: "phys", Important: true, AdminUp: true, OperUp: true, SpeedMbps: 10000, InBps: 6e9, OutBps: 2e9, InUtil: 60, OutUtil: 20, InPps: 500000, OutPps: 200000, InDropRate: 3, OutDropRate: 0},
		// access port, quiet
		{ID: "d1:2", Name: "Gi1/0/1", Kind: "phys", AdminUp: true, OperUp: true, SpeedMbps: 1000, InBps: 1e6, OutBps: 2e6, InUtil: 0.1, OutUtil: 0.2, InPps: 100, OutPps: 150},
		// down uplink (important) and a plain down port
		{ID: "d1:3", Name: "Te1/2", Kind: "phys", Important: true, AdminUp: true, OperUp: false, StatusSince: now.Add(-time.Hour)},
		{ID: "d1:4", Name: "Gi1/0/2", Kind: "phys", AdminUp: true, OperUp: false},
		// shut by config: not a fault
		{ID: "d1:5", Name: "Gi1/0/3", Kind: "phys", AdminUp: false, OperUp: false},
		// SVI: oper down but not a physical fault; must not double-count traffic either
		{ID: "d1:6", Name: "Vlan10", Kind: "vlan", AdminUp: true, OperUp: true, InBps: 5e9, OutBps: 5e9, InPps: 1e6, OutPps: 1e6},
		{ID: "d1:7", Name: "Vlan20", Kind: "vlan", AdminUp: true, OperUp: false},
	}
	alerts := []model.Alert{
		{ID: "a1", DeviceID: "d1", Severity: model.SevMinor, State: model.AlertOpen},
		{ID: "a2", DeviceID: "d1", Severity: model.SevMajor, State: model.AlertAcked},
		{ID: "a3", DeviceID: "d1", Severity: model.SevCritical, State: model.AlertResolved}, // ignored
		{ID: "a4", DeviceID: "other", Severity: model.SevCritical, State: model.AlertOpen},  // other device
	}
	h := deviceHealth(d, ifs, alerts, "core-sw1")

	if h.Cause != "core-sw1" || h.Name != "dist-sw1" || h.Status != model.StatusDegraded {
		t.Fatalf("identity: %+v", h)
	}
	if h.CPUPct == nil || *h.CPUPct != 41 || h.MemPct == nil || *h.MemPct != 63 || h.TempC != nil {
		t.Fatalf("metrics: cpu=%v mem=%v temp=%v", h.CPUPct, h.MemPct, h.TempC)
	}
	s := h.Interfaces
	if s.Total != 7 || s.Up != 3 || s.Down != 2 || s.AdminDown != 1 || s.Important != 2 || s.ImportantDown != 1 {
		t.Fatalf("interface summary: %+v", s)
	}
	tr := h.Traffic
	// VLAN traffic must not be in the aggregate
	if tr.InBps != 6e9+1e6 || tr.OutBps != 2e9+2e6 || tr.InPps != 500100 || tr.OutPps != 200150 || tr.InDropPs != 3 || !tr.HaveRates {
		t.Fatalf("traffic: %+v", tr)
	}
	if len(h.TopUtil) != 2 || h.TopUtil[0].Name != "Te1/1" || h.TopUtil[0].DropPs != 3 || h.TopUtil[1].Name != "Gi1/0/1" {
		t.Fatalf("top util: %+v", h.TopUtil)
	}
	// important down interface listed first, SVI and admin-down excluded
	if len(h.Down) != 2 || h.Down[0].Name != "Te1/2" || !h.Down[0].Important || h.Down[1].Name != "Gi1/0/2" || h.DownMore != 0 {
		t.Fatalf("down: %+v more=%d", h.Down, h.DownMore)
	}
	if h.OpenAlerts != 2 || h.Worst != model.SevMajor {
		t.Fatalf("alerts: %d %s", h.OpenAlerts, h.Worst)
	}
}

func TestDeviceHealthEmpty(t *testing.T) {
	h := deviceHealth(model.Device{ID: "x", Name: "x", Status: model.StatusUnknown}, nil, nil, "")
	if h.Interfaces.Total != 0 || h.Traffic.HaveRates || h.CPUPct != nil || len(h.TopUtil) != 0 || len(h.Down) != 0 || h.OpenAlerts != 0 {
		t.Fatalf("empty device: %+v", h)
	}
}

func TestDeviceHealthDownCap(t *testing.T) {
	var ifs []model.Interface
	for i := 0; i < 10; i++ {
		ifs = append(ifs, model.Interface{ID: string(rune('a' + i)), Name: string(rune('a' + i)), Kind: "phys", AdminUp: true, OperUp: false})
	}
	h := deviceHealth(model.Device{ID: "x"}, ifs, nil, "")
	if len(h.Down) != healthDownMax || h.DownMore != 10-healthDownMax || h.Interfaces.Down != 10 {
		t.Fatalf("cap: %d shown, %d more, %d down", len(h.Down), h.DownMore, h.Interfaces.Down)
	}
}
