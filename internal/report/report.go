// Package report builds availability, alert, utilisation, inventory, change,
// flow, endpoint and probe reports as self-contained HTML (printable to PDF
// from any browser) with CSV for the tables, on demand or on a schedule.
package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"html"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/nizartuanku/topolight/internal/backup"
	"github.com/nizartuanku/topolight/internal/endpoint"
	"github.com/nizartuanku/topolight/internal/flow"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/probe"
	"github.com/nizartuanku/topolight/internal/store"
	"github.com/nizartuanku/topolight/internal/tsdb"
)

// Sections lists the available report sections.
var Sections = []string{"availability", "alerts", "utilisation", "inventory", "changes", "flow", "endpoints", "probes"}

// Deps are the data sources a generator reads.
type Deps struct {
	Store     *store.Store
	DB        *tsdb.DB
	Backup    *backup.Store
	Flow      *flow.Aggregator
	Endpoints *endpoint.Store
	Probes    *probe.Runner
	Instance  string
}

// Table is one tabular section (also exported as CSV).
type Table struct {
	Key   string     `json:"key"`
	Title string     `json:"title"`
	Note  string     `json:"note,omitempty"`
	Cols  []string   `json:"cols"`
	Rows  [][]string `json:"rows"`
	// Bar, when ≥ 0, is the column index rendered with an inline bar (values 0–100).
	Bar int `json:"bar"`
}

// Result is a generated report.
type Result struct {
	Title    string    `json:"title"`
	Period   string    `json:"period"`
	From, To time.Time `json:"-"`
	Site     string    `json:"site"`
	KPIs     [][2]string
	Tables   []Table
	Warnings []string
}

// Period parses 24h|7d|30d.
func Period(p string) time.Duration {
	switch p {
	case "24h":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

// Generate builds the report.
func Generate(d Deps, r model.Report, now time.Time) *Result {
	dur := Period(r.Period)
	from := now.Add(-dur)
	res := &Result{Title: r.Name, Period: r.Period, From: from, To: now}
	if res.Title == "" {
		res.Title = "Network report"
	}
	devices := d.Store.Devices()
	if r.SiteID != "" {
		var f []model.Device
		for _, x := range devices {
			if x.SiteID == r.SiteID {
				f = append(f, x)
			}
		}
		devices = f
		if s, err := d.Store.Site(r.SiteID); err == nil {
			res.Site = s.Name
		}
	}
	names := map[string]string{}
	inScope := map[string]bool{}
	for _, x := range devices {
		names[x.ID] = x.Name
		inScope[x.ID] = true
	}
	sections := r.Sections
	if len(sections) == 0 {
		sections = Sections
	}
	for _, s := range sections {
		switch s {
		case "availability":
			availability(d, res, devices, inScope, from, now)
		case "alerts":
			alerts(d, res, names, inScope, from, now)
		case "utilisation":
			utilisation(d, res, devices, from, now)
		case "inventory":
			inventory(d, res, devices)
		case "changes":
			changes(d, res, devices, from, now)
		case "flow":
			flowSection(d, res, devices, names, dur, now)
		case "endpoints":
			endpoints(d, res, devices, inScope, from, now)
		case "probes":
			probes(d, res)
		}
	}
	return res
}

func pct(v float64) string { return fmt.Sprintf("%.2f", v) }

func availability(d Deps, res *Result, devices []model.Device, inScope map[string]bool, from, to time.Time) {
	span := to.Sub(from)
	down := map[string]time.Duration{}
	incidents := map[string]int{}
	worst := map[string]time.Duration{}
	for _, a := range d.Store.AlertsSince(from) {
		if a.Rule != "device_down" || !inScope[a.DeviceID] {
			continue
		}
		s, e := a.OpenedAt, a.ResolvedAt
		if e.IsZero() || a.State != model.AlertResolved {
			e = to
		}
		if s.Before(from) {
			s = from
		}
		if e.After(to) {
			e = to
		}
		if e.After(s) {
			down[a.DeviceID] += e.Sub(s)
			incidents[a.DeviceID]++
			if e.Sub(s) > worst[a.DeviceID] {
				worst[a.DeviceID] = e.Sub(s)
			}
		}
	}
	t := Table{Key: "availability", Title: "Availability", Cols: []string{"Device", "Site", "Role", "Availability %", "Downtime", "Incidents", "Longest outage", "Status now"}, Bar: 3}
	var sum float64
	n := 0
	below := 0
	for _, dev := range devices {
		if !dev.Monitored {
			continue
		}
		mon := span
		if dev.Created.After(from) {
			mon = to.Sub(dev.Created)
		}
		avail := 100.0
		if mon > 0 {
			avail = 100 * (1 - float64(down[dev.ID])/float64(mon))
		}
		if avail < 0 {
			avail = 0
		}
		sum += avail
		n++
		if avail < 99.9 {
			below++
		}
		site := ""
		if s, err := d.Store.Site(dev.SiteID); err == nil {
			site = s.Name
		}
		t.Rows = append(t.Rows, []string{dev.Name, site, string(dev.Role), pct(avail), fmtDur(down[dev.ID]), fmt.Sprint(incidents[dev.ID]), fmtDur(worst[dev.ID]), string(dev.Status)})
	}
	sort.SliceStable(t.Rows, func(i, j int) bool { return atof(t.Rows[i][3]) < atof(t.Rows[j][3]) })
	if n > 0 {
		res.KPIs = append(res.KPIs, [2]string{"Average availability", pct(sum/float64(n)) + " %"}, [2]string{"Devices below 99.9 %", fmt.Sprint(below)}, [2]string{"Monitored devices", fmt.Sprint(n)})
	}
	t.Note = "Downtime = time a device_down alert was open in the period (maintenance windows suppress the alert, so planned work does not count). Devices added during the period are measured from their creation."
	res.Tables = append(res.Tables, t)
}

func alerts(d Deps, res *Result, names map[string]string, inScope map[string]bool, from, to time.Time) {
	byRule := map[string]int{}
	bySev := map[model.Severity]int{}
	var mttr []time.Duration
	byDev := map[string]int{}
	total := 0
	for _, a := range d.Store.AlertsSince(from) {
		if a.OpenedAt.Before(from) || (a.DeviceID != "" && !inScope[a.DeviceID]) {
			continue
		}
		total++
		byRule[a.Rule]++
		bySev[a.Severity]++
		byDev[a.DeviceID]++
		if a.State == model.AlertResolved && !a.ResolvedAt.IsZero() {
			mttr = append(mttr, a.ResolvedAt.Sub(a.OpenedAt))
		}
	}
	res.KPIs = append(res.KPIs, [2]string{"Alerts opened", fmt.Sprint(total)}, [2]string{"Critical / Major", fmt.Sprintf("%d / %d", bySev[model.SevCritical], bySev[model.SevMajor])})
	if len(mttr) > 0 {
		sort.Slice(mttr, func(i, j int) bool { return mttr[i] < mttr[j] })
		var sum time.Duration
		for _, x := range mttr {
			sum += x
		}
		res.KPIs = append(res.KPIs, [2]string{"Mean / median time to resolve", fmtDur(sum/time.Duration(len(mttr))) + " / " + fmtDur(mttr[len(mttr)/2])})
	}
	t := Table{Key: "alerts_by_rule", Title: "Alerts by rule", Cols: []string{"Rule", "Count", "Share %"}, Bar: 2}
	for k, v := range byRule {
		t.Rows = append(t.Rows, []string{k, fmt.Sprint(v), fmt.Sprintf("%.1f", float64(v)*100/float64(max(1, total)))})
	}
	sort.Slice(t.Rows, func(i, j int) bool { return atoi(t.Rows[i][1]) > atoi(t.Rows[j][1]) })
	res.Tables = append(res.Tables, t)
	t2 := Table{Key: "alerts_by_device", Title: "Noisiest devices", Cols: []string{"Device", "Alerts"}, Bar: -1}
	for k, v := range byDev {
		n := names[k]
		if n == "" {
			n = "(site / no device)"
		}
		t2.Rows = append(t2.Rows, []string{n, fmt.Sprint(v)})
	}
	sort.Slice(t2.Rows, func(i, j int) bool { return atoi(t2.Rows[i][1]) > atoi(t2.Rows[j][1]) })
	if len(t2.Rows) > 15 {
		t2.Rows = t2.Rows[:15]
	}
	res.Tables = append(res.Tables, t2)
}

func series(db *tsdb.DB, key string, from, to time.Time) (avg, peak float64, ok bool) {
	pts := db.Query(key, from.Unix(), to.Unix())
	if len(pts) == 0 {
		return 0, 0, false
	}
	var sum float64
	for _, p := range pts {
		sum += p.V
		m := p.Max
		if m < p.V {
			m = p.V
		}
		if m > peak {
			peak = m
		}
	}
	return sum / float64(len(pts)), peak, true
}

func utilisation(d Deps, res *Result, devices []model.Device, from, to time.Time) {
	type row struct {
		dev, ifn, alias string
		speed           int64
		avg, peak       float64
		dir             string
	}
	var rows []row
	for _, dev := range devices {
		for _, i := range d.Store.Interfaces(dev.ID) {
			if !i.Important && i.SpeedMbps == 0 {
				continue
			}
			for _, dir := range []string{"in", "out"} {
				avg, peak, ok := series(d.DB, "if_"+dir+"_bps|"+i.ID, from, to)
				if !ok || i.SpeedMbps == 0 {
					continue
				}
				cap := float64(i.SpeedMbps) * 1e6
				rows = append(rows, row{dev.Name, i.Name, i.Alias, i.SpeedMbps, avg * 100 / cap, peak * 100 / cap, dir})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].peak > rows[j].peak })
	t := Table{Key: "interfaces", Title: "Busiest interfaces (utilisation, important ports)", Cols: []string{"Device", "Interface", "Description", "Speed", "Dir", "Average %", "Peak %"}, Bar: 6}
	for i, r := range rows {
		if i >= 25 {
			break
		}
		t.Rows = append(t.Rows, []string{r.dev, r.ifn, r.alias, fmtSpeed(r.speed), r.dir, fmt.Sprintf("%.1f", r.avg), fmt.Sprintf("%.1f", math.Min(100, r.peak))})
	}
	t.Note = "Averages over the period from the 5-minute history; peak is the highest 5-minute average (or 60-second sample where raw history is still kept)."
	res.Tables = append(res.Tables, t)
	// device CPU / memory
	t2 := Table{Key: "devices", Title: "Device load", Cols: []string{"Device", "CPU avg %", "CPU peak %", "Memory avg %", "Memory peak %"}, Bar: 2}
	type drow struct {
		n              string
		ca, cp, ma, mp float64
		hasCPU, hasMem bool
	}
	var drows []drow
	for _, dev := range devices {
		if dev.PingOnly {
			continue
		}
		x := drow{n: dev.Name}
		x.ca, x.cp, x.hasCPU = series(d.DB, "cpu_pct|"+dev.ID, from, to)
		x.ma, x.mp, x.hasMem = series(d.DB, "mem_pct|"+dev.ID, from, to)
		if x.hasCPU || x.hasMem {
			drows = append(drows, x)
		}
	}
	sort.Slice(drows, func(i, j int) bool { return drows[i].cp > drows[j].cp })
	for i, x := range drows {
		if i >= 25 {
			break
		}
		f := func(v float64, ok bool) string {
			if !ok {
				return "—"
			}
			return fmt.Sprintf("%.0f", v)
		}
		t2.Rows = append(t2.Rows, []string{x.n, f(x.ca, x.hasCPU), f(x.cp, x.hasCPU), f(x.ma, x.hasMem), f(x.mp, x.hasMem)})
	}
	res.Tables = append(res.Tables, t2)
}

func inventory(d Deps, res *Result, devices []model.Device) {
	t := Table{Key: "inventory", Title: "Inventory", Cols: []string{"Device", "IP", "Site", "Role", "Vendor", "Model", "OS version", "Serial", "Interfaces", "Status", "Uptime", "Monitored"}, Bar: -1}
	byVendor := map[string]int{}
	for _, dev := range devices {
		site := ""
		if s, err := d.Store.Site(dev.SiteID); err == nil {
			site = s.Name
		}
		v := dev.Vendor
		if v == "" {
			v = "unknown"
		}
		byVendor[v]++
		up := ""
		if dev.Uptime > 0 {
			up = fmtDur(time.Duration(dev.Uptime) * time.Second)
		}
		mon := "yes"
		if !dev.Monitored {
			mon = "no"
		} else if dev.PingOnly {
			mon = "ping only"
		}
		t.Rows = append(t.Rows, []string{dev.Name, dev.IP, site, string(dev.Role), dev.Vendor, dev.Model, dev.OSVersion, dev.Serial, fmt.Sprint(len(d.Store.Interfaces(dev.ID))), string(dev.Status), up, mon})
	}
	res.KPIs = append(res.KPIs, [2]string{"Devices in inventory", fmt.Sprint(len(devices))})
	res.Tables = append(res.Tables, t)
	t2 := Table{Key: "vendors", Title: "By vendor", Cols: []string{"Vendor", "Devices"}, Bar: -1}
	for k, v := range byVendor {
		t2.Rows = append(t2.Rows, []string{k, fmt.Sprint(v)})
	}
	sort.Slice(t2.Rows, func(i, j int) bool { return atoi(t2.Rows[i][1]) > atoi(t2.Rows[j][1]) })
	res.Tables = append(res.Tables, t2)
}

func changes(d Deps, res *Result, devices []model.Device, from, to time.Time) {
	t := Table{Key: "changes", Title: "Configuration changes", Cols: []string{"Device", "Stored", "Trigger", "Lines +", "Lines −", "Version"}, Bar: -1}
	n := 0
	failing := 0
	if d.Backup != nil {
		for _, dev := range devices {
			vs, st := d.Backup.Versions(dev.ID)
			if st.Error != "" {
				failing++
			}
			for _, v := range vs {
				if v.TS.Before(from) || v.Added+v.Removed == 0 {
					continue
				}
				n++
				t.Rows = append(t.Rows, []string{dev.Name, v.TS.Format("2006-01-02 15:04"), v.Source, fmt.Sprint(v.Added), fmt.Sprint(v.Removed), v.ID})
			}
		}
	}
	sort.Slice(t.Rows, func(i, j int) bool { return t.Rows[i][1] > t.Rows[j][1] })
	res.KPIs = append(res.KPIs, [2]string{"Configuration changes", fmt.Sprint(n)}, [2]string{"Backups failing", fmt.Sprint(failing)})
	res.Tables = append(res.Tables, t)
	// syslog-reported changes and reboots
	ev := d.Store.EventsSince(from, map[string]bool{"config_changed": true, "device_rebooted": true, "neighbor_changed": true})
	t2 := Table{Key: "events", Title: "Change and reboot events", Cols: []string{"Time", "Device", "Kind", "Message"}, Bar: -1}
	names := map[string]string{}
	for _, dev := range devices {
		names[dev.ID] = dev.Name
	}
	for i := len(ev) - 1; i >= 0 && len(t2.Rows) < 200; i-- {
		e := ev[i]
		if names[e.DeviceID] == "" {
			continue
		}
		t2.Rows = append(t2.Rows, []string{e.TS.Format("2006-01-02 15:04"), names[e.DeviceID], e.Kind, e.Message})
	}
	res.Tables = append(res.Tables, t2)
}

func flowSection(d Deps, res *Result, devices []model.Device, names map[string]string, dur time.Duration, now time.Time) {
	if d.Flow == nil {
		return
	}
	if dur > 24*time.Hour {
		dur = 24 * time.Hour
		res.Warnings = append(res.Warnings, "Flow tables cover the last 24 hours (the flow store keeps 24 h of summaries).")
	}
	sum := d.Flow.Window("", dur, now)
	byIP := map[string]bool{}
	for _, dev := range devices {
		byIP[dev.IP] = true
	}
	t := Table{Key: "talkers", Title: "Top talkers", Cols: []string{"Host", "Bytes", "Share %"}, Bar: 2}
	for i, e := range sum.Talkers {
		if i >= 20 {
			break
		}
		t.Rows = append(t.Rows, []string{e.Key, fmtBytes(e.Bytes), fmt.Sprintf("%.1f", float64(e.Bytes)*100/float64(max64(1, sum.Bytes)))})
	}
	res.Tables = append(res.Tables, t)
	t2 := Table{Key: "apps", Title: "Applications", Cols: []string{"Application", "Bytes", "Share %"}, Bar: 2}
	for i, a := range sum.Apps {
		if i >= 20 {
			break
		}
		n := a.Name
		if n == "" {
			n = fmt.Sprintf("proto %d/%d", a.Proto, a.Port)
		}
		t2.Rows = append(t2.Rows, []string{n, fmtBytes(a.Bytes), fmt.Sprintf("%.1f", float64(a.Bytes)*100/float64(max64(1, sum.Bytes)))})
	}
	res.Tables = append(res.Tables, t2)
	res.KPIs = append(res.KPIs, [2]string{"Flow traffic (" + fmtDurShort(dur) + ")", fmtBytes(sum.Bytes)})
}

func endpoints(d Deps, res *Result, devices []model.Device, inScope map[string]bool, from, to time.Time) {
	if d.Endpoints == nil {
		return
	}
	all := d.Endpoints.Query("", "", 0, 0)
	byVendor := map[string]int{}
	byDev := map[string]int{}
	newN, total := 0, 0
	for _, e := range all {
		if e.DeviceID != "" && !inScope[e.DeviceID] {
			continue
		}
		total++
		v := e.Vendor
		if v == "" {
			v = "unknown"
		}
		byVendor[v]++
		byDev[e.DeviceID]++
		if e.FirstSeen.After(from) {
			newN++
		}
	}
	res.KPIs = append(res.KPIs, [2]string{"Endpoints (MACs)", fmt.Sprint(total)}, [2]string{"New in period", fmt.Sprint(newN)})
	t := Table{Key: "endpoint_vendors", Title: "Endpoints by vendor", Cols: []string{"Vendor", "MACs"}, Bar: -1}
	for k, v := range byVendor {
		t.Rows = append(t.Rows, []string{k, fmt.Sprint(v)})
	}
	sort.Slice(t.Rows, func(i, j int) bool { return atoi(t.Rows[i][1]) > atoi(t.Rows[j][1]) })
	if len(t.Rows) > 20 {
		t.Rows = t.Rows[:20]
	}
	res.Tables = append(res.Tables, t)
	names := map[string]string{}
	for _, dev := range devices {
		names[dev.ID] = dev.Name
	}
	t2 := Table{Key: "endpoint_switches", Title: "Endpoints per switch", Cols: []string{"Switch", "MACs on access ports"}, Bar: -1}
	for k, v := range byDev {
		if k == "" {
			continue
		}
		t2.Rows = append(t2.Rows, []string{names[k], fmt.Sprint(v)})
	}
	sort.Slice(t2.Rows, func(i, j int) bool { return atoi(t2.Rows[i][1]) > atoi(t2.Rows[j][1]) })
	res.Tables = append(res.Tables, t2)
}

func probes(d Deps, res *Result) {
	if d.Probes == nil {
		return
	}
	sums := d.Probes.Summaries()
	t := Table{Key: "probes", Title: "Synthetic probes", Cols: []string{"Probe", "Type", "Target", "OK rate %", "Avg ms", "Last result"}, Bar: 3}
	for _, p := range d.Store.Probes() {
		s := sums[p.ID]
		last := "—"
		if !s.Last.TS.IsZero() {
			if s.Last.OK {
				last = "ok"
			} else {
				last = "FAIL: " + s.Last.Detail
			}
		}
		t.Rows = append(t.Rows, []string{p.Name, p.Type, p.Target, fmt.Sprintf("%.1f", s.Uptime), fmt.Sprintf("%.1f", s.AvgMs), last})
	}
	t.Note = "OK rate is over the probe's retained runs (up to 120), not the full period."
	res.Tables = append(res.Tables, t)
}

// ---- rendering ------------------------------------------------------------------

// HTML renders a self-contained page.
func HTML(r *Result, instance string) string {
	var b strings.Builder
	esc := html.EscapeString
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>` + esc(r.Title) + `</title><style>
body{font:14px/1.45 -apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#111827;margin:32px;max-width:1100px}
h1{font-size:22px;margin:0 0 4px}h2{font-size:16px;margin:28px 0 8px;border-bottom:1px solid #DEE3EA;padding-bottom:4px}
.sub{color:#5B6472;margin-bottom:18px}.kpis{display:flex;flex-wrap:wrap;gap:12px;margin:14px 0}
.kpi{border:1px solid #DEE3EA;border-radius:8px;padding:10px 14px;min-width:150px}.kpi b{display:block;font-size:20px}.kpi span{color:#5B6472;font-size:12px}
table{border-collapse:collapse;width:100%;font-size:13px}th,td{text-align:left;padding:5px 8px;border-bottom:1px solid #EEF1F5;vertical-align:top}th{color:#5B6472;font-weight:600;font-size:12px;text-transform:uppercase;letter-spacing:.03em}
td.num{text-align:right;font-variant-numeric:tabular-nums}.bar{display:inline-block;height:8px;background:#1668B8;border-radius:4px;vertical-align:middle;margin-left:6px}
.note{color:#5B6472;font-size:12px;margin:6px 0 0}.warn{background:#FFF4E5;border:1px solid #FFD8A8;padding:8px 12px;border-radius:6px;margin:10px 0}
@media print{body{margin:12mm}h2{page-break-after:avoid}tr{page-break-inside:avoid}}
</style></head><body>`)
	b.WriteString("<h1>" + esc(r.Title) + "</h1><div class=sub>" + esc(instance) + " · " + esc(r.From.Format("2 Jan 2006 15:04")) + " → " + esc(r.To.Format("2 Jan 2006 15:04")))
	if r.Site != "" {
		b.WriteString(" · site " + esc(r.Site))
	}
	b.WriteString(" · generated by TopoLight</div>")
	for _, w := range r.Warnings {
		b.WriteString("<div class=warn>" + esc(w) + "</div>")
	}
	if len(r.KPIs) > 0 {
		b.WriteString("<div class=kpis>")
		for _, k := range r.KPIs {
			b.WriteString("<div class=kpi><b>" + esc(k[1]) + "</b><span>" + esc(k[0]) + "</span></div>")
		}
		b.WriteString("</div>")
	}
	for _, t := range r.Tables {
		b.WriteString("<h2>" + esc(t.Title) + "</h2>")
		if len(t.Rows) == 0 {
			b.WriteString("<p class=note>Nothing in this period.</p>")
			continue
		}
		b.WriteString("<table><thead><tr>")
		for _, c := range t.Cols {
			b.WriteString("<th>" + esc(c) + "</th>")
		}
		b.WriteString("</tr></thead><tbody>")
		for _, row := range t.Rows {
			b.WriteString("<tr>")
			for i, c := range row {
				cls := ""
				if isNum(c) {
					cls = " class=num"
				}
				b.WriteString("<td" + cls + ">" + esc(c))
				if i == t.Bar {
					if v := atof(c); v >= 0 {
						b.WriteString(fmt.Sprintf(`<span class=bar style="width:%dpx"></span>`, int(math.Min(100, v))))
					}
				}
				b.WriteString("</td>")
			}
			b.WriteString("</tr>")
		}
		b.WriteString("</tbody></table>")
		if t.Note != "" {
			b.WriteString("<p class=note>" + esc(t.Note) + "</p>")
		}
	}
	b.WriteString("</body></html>")
	return b.String()
}

// CSV renders all tables as one CSV (blank line between tables).
func CSV(r *Result) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	for _, t := range r.Tables {
		w.Write([]string{"# " + t.Title})
		w.Write(t.Cols)
		for _, row := range t.Rows {
			w.Write(row)
		}
		w.Write([]string{})
	}
	w.Flush()
	return buf.Bytes()
}

// ---- helpers -----------------------------------------------------------------------

func fmtDur(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h >= 48:
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

func fmtDurShort(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func fmtSpeed(m int64) string {
	if m >= 1000 {
		return fmt.Sprintf("%dG", m/1000)
	}
	return fmt.Sprintf("%dM", m)
}

func fmtBytes(v uint64) string {
	f := float64(v)
	u := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}

func atoi(s string) int {
	n := 0
	fmt.Sscan(s, &n)
	return n
}

func atof(s string) float64 {
	var f float64
	if _, err := fmt.Sscan(s, &f); err != nil {
		return -1
	}
	return f
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
