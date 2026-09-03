package webui

import (
	"sort"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
)

// Health is the compact per-device summary behind the topology hover card
// and the device side panel: everything an operator wants to see in one
// glance before deciding whether to click through. It is computed from the
// store's last samples — no extra polling, no history reads.
type Health struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	IP          string       `json:"ip"`
	Role        model.Role   `json:"role"`
	Vendor      string       `json:"vendor,omitempty"`
	Model       string       `json:"model,omitempty"`
	Status      model.Status `json:"status"`
	StatusSince time.Time    `json:"status_since"`
	Cause       string       `json:"cause,omitempty"` // upstream device name when unreachable
	Monitored   bool         `json:"monitored"`
	SNMPOK      bool         `json:"snmp_ok"`
	LastPoll    time.Time    `json:"last_poll,omitempty"`
	UptimeS     int64        `json:"uptime_s,omitempty"`

	// Device metrics; nil when the last poll had no value for them.
	CPUPct  *float64 `json:"cpu_pct,omitempty"`
	MemPct  *float64 `json:"mem_pct,omitempty"`
	TempC   *float64 `json:"temp_c,omitempty"`
	RTTms   *float64 `json:"rtt_ms,omitempty"`
	LossPct *float64 `json:"loss_pct,omitempty"`

	Interfaces IfSummary `json:"interfaces"`
	Traffic    Traffic   `json:"traffic"`
	// TopUtil lists the busiest interfaces (max of in/out utilisation), worst first.
	TopUtil []IfBrief `json:"top_util"`
	// Down lists interfaces that are administratively up but operationally down,
	// important ones first, then by name. More says how many were cut off.
	Down     []IfBrief `json:"down"`
	DownMore int       `json:"down_more,omitempty"`

	OpenAlerts int            `json:"open_alerts"`
	Worst      model.Severity `json:"worst_severity,omitempty"`
}

// IfSummary counts interfaces by state. Loopbacks, VLAN SVIs and tunnels are
// excluded from Down (an admin-up VLAN with no members is not a fault); the
// totals count every interface the device reports.
type IfSummary struct {
	Total         int `json:"total"`
	Up            int `json:"up"`
	Down          int `json:"down"`       // admin up, oper down (physical + LAG)
	AdminDown     int `json:"admin_down"` // shut by configuration
	Important     int `json:"important"`
	ImportantDown int `json:"important_down"`
}

// Traffic aggregates the last sample of every oper-up physical/LAG interface.
type Traffic struct {
	InBps     float64 `json:"in_bps"`
	OutBps    float64 `json:"out_bps"`
	InPps     float64 `json:"in_pps"`
	OutPps    float64 `json:"out_pps"`
	InErrPs   float64 `json:"in_err_ps"`
	OutErrPs  float64 `json:"out_err_ps"`
	InDropPs  float64 `json:"in_drop_ps"`
	OutDropPs float64 `json:"out_drop_ps"`
	// HaveRates is false until two polls have produced a delta.
	HaveRates bool `json:"have_rates"`
}

// IfBrief is one interface line in the card.
type IfBrief struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Alias     string    `json:"alias,omitempty"`
	Important bool      `json:"important,omitempty"`
	InUtil    float64   `json:"in_util_pct"`
	OutUtil   float64   `json:"out_util_pct"`
	InBps     float64   `json:"in_bps"`
	OutBps    float64   `json:"out_bps"`
	DropPs    float64   `json:"drop_ps,omitempty"`
	ErrPs     float64   `json:"err_ps,omitempty"`
	Since     time.Time `json:"since,omitempty"`
}

const (
	healthTopUtil = 3
	healthDownMax = 6
)

func metric(m map[string]float64, k string) *float64 {
	if m == nil {
		return nil
	}
	v, ok := m[k]
	if !ok {
		return nil
	}
	return &v
}

// countsForTraffic says whether an interface's counters belong in the
// device-level aggregate: physical ports and LAGs only, and only while up —
// a VLAN SVI would double-count the members beneath it.
func countsForTraffic(i model.Interface) bool {
	return (i.Kind == "phys" || i.Kind == "lag") && i.OperUp
}

func deviceHealth(d model.Device, ifs []model.Interface, alerts []model.Alert, causeName string) Health {
	h := Health{ID: d.ID, Name: d.Name, IP: d.IP, Role: d.Role, Vendor: d.Vendor, Model: d.Model, Status: d.Status, StatusSince: d.StatusSince,
		Cause: causeName, Monitored: d.Monitored, SNMPOK: d.SNMPOK, LastPoll: d.LastPoll, UptimeS: d.Uptime,
		CPUPct: metric(d.Metrics, "cpu_pct"), MemPct: metric(d.Metrics, "mem_pct"), TempC: metric(d.Metrics, "temp_c"),
		RTTms: metric(d.Metrics, "rtt_ms"), LossPct: metric(d.Metrics, "loss_pct"), TopUtil: []IfBrief{}, Down: []IfBrief{}}

	var down []model.Interface
	for _, i := range ifs {
		h.Interfaces.Total++
		if i.Important {
			h.Interfaces.Important++
		}
		switch {
		case !i.AdminUp:
			h.Interfaces.AdminDown++
		case i.OperUp:
			h.Interfaces.Up++
		default:
			if i.Kind == "phys" || i.Kind == "lag" {
				h.Interfaces.Down++
				down = append(down, i)
				if i.Important {
					h.Interfaces.ImportantDown++
				}
			}
		}
		if countsForTraffic(i) {
			t := &h.Traffic
			t.InBps += i.InBps
			t.OutBps += i.OutBps
			t.InPps += i.InPps
			t.OutPps += i.OutPps
			t.InErrPs += i.InErrRate
			t.OutErrPs += i.OutErrRate
			t.InDropPs += i.InDropRate
			t.OutDropPs += i.OutDropRate
			if i.InBps > 0 || i.OutBps > 0 || i.InPps > 0 || i.OutPps > 0 {
				t.HaveRates = true
			}
		}
	}

	// busiest interfaces: any oper-up port with a known speed
	var busy []model.Interface
	for _, i := range ifs {
		if countsForTraffic(i) && i.SpeedMbps > 0 && (i.InUtil > 0 || i.OutUtil > 0) {
			busy = append(busy, i)
		}
	}
	// busiest by utilisation; when utilisation ties (an idle estate), ports
	// that drop or error come first, then the ones carrying the most bits.
	sort.SliceStable(busy, func(a, b int) bool {
		ua, ub := maxf(busy[a].InUtil, busy[a].OutUtil), maxf(busy[b].InUtil, busy[b].OutUtil)
		if int(ua) != int(ub) {
			return ua > ub
		}
		pa := busy[a].InDropRate + busy[a].OutDropRate + busy[a].InErrRate + busy[a].OutErrRate
		pb := busy[b].InDropRate + busy[b].OutDropRate + busy[b].InErrRate + busy[b].OutErrRate
		if (pa > 0) != (pb > 0) {
			return pa > 0
		}
		return busy[a].InBps+busy[a].OutBps > busy[b].InBps+busy[b].OutBps
	})
	for k, i := range busy {
		if k >= healthTopUtil {
			break
		}
		h.TopUtil = append(h.TopUtil, brief(i))
	}

	sort.SliceStable(down, func(a, b int) bool {
		if down[a].Important != down[b].Important {
			return down[a].Important
		}
		return down[a].Name < down[b].Name
	})
	for k, i := range down {
		if k >= healthDownMax {
			h.DownMore = len(down) - healthDownMax
			break
		}
		h.Down = append(h.Down, brief(i))
	}

	for _, a := range alerts {
		if a.DeviceID != d.ID || a.State == model.AlertResolved {
			continue
		}
		h.OpenAlerts++
		if a.Severity.Rank() > h.Worst.Rank() {
			h.Worst = a.Severity
		}
	}
	return h
}

func brief(i model.Interface) IfBrief {
	return IfBrief{ID: i.ID, Name: i.Name, Alias: i.Alias, Important: i.Important, InUtil: i.InUtil, OutUtil: i.OutUtil,
		InBps: i.InBps, OutBps: i.OutBps, DropPs: i.InDropRate + i.OutDropRate, ErrPs: i.InErrRate + i.OutErrRate, Since: i.StatusSince}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
