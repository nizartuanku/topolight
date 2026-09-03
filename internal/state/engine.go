// Package state turns samples and events into object status, alerts and
// live change notifications. Alerts are raised on transitions, never on
// values; hysteresis, confirmation cycles, flap detection and topology-based
// suppression live here.
package state

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
)

// Change is broadcast to UI subscribers (SSE).
type Change struct {
	Type string `json:"type"` // device|interface|link|alert|event|topology|discovery
	Data any    `json:"data"`
}

// Engine is the state machine host.
type Engine struct {
	st *store.Store

	Devices    <-chan model.DeviceSample
	Interfaces <-chan model.InterfaceSample
	Events     []<-chan model.Event
	// Notify receives alerts to deliver (open and resolved).
	Notify chan model.Alert
	// RootDevice, when set, is the device closest to the collector; the
	// topology path from it decides what "upstream" means.
	RootDevice string

	mu      sync.Mutex
	dev     map[string]*objState
	ifs     map[string]*objState
	authWin map[string][]time.Time
	subs    map[chan Change]struct{}
	// lastSiteEval throttles site-down evaluation
	lastSiteEval time.Time
	parents      map[string]string // device -> upstream device (cached from topology)
	parentsAt    time.Time
}

type objState struct {
	status      model.Status
	since       time.Time
	failStreak  int
	okStreak    int
	metricRuns  map[string]int // rule id -> consecutive cycles over threshold
	metricOn    map[string]bool
	transitions []time.Time
	flapping    bool
	lastSample  time.Time
	lastSeen    time.Time
	candidate   model.Status // set by hard events (trap/syslog) pending confirmation
	candidateAt time.Time
}

func newObj() *objState {
	return &objState{status: model.StatusUnknown, metricRuns: map[string]int{}, metricOn: map[string]bool{}}
}

// New builds the engine and seeds default rules.
func New(st *store.Store) *Engine {
	e := &Engine{st: st, Notify: make(chan model.Alert, 1024), dev: map[string]*objState{}, ifs: map[string]*objState{}, authWin: map[string][]time.Time{}, subs: map[chan Change]struct{}{}, parents: map[string]string{}}
	existing := map[string]bool{}
	for _, r := range st.Rules() {
		existing[r.ID] = true
	}
	for _, r := range DefaultRules {
		if !existing[r.ID] {
			st.PutRule(r)
		}
	}
	// restore statuses from the store so restarts do not re-alert
	for _, d := range st.Devices() {
		o := newObj()
		o.status, o.since = d.Status, d.StatusSince
		e.dev[d.ID] = o
	}
	for _, i := range st.Interfaces("") {
		o := newObj()
		o.status, o.since = i.Status, i.StatusSince
		e.ifs[i.ID] = o
	}
	return e
}

// Subscribe returns a channel of changes for the UI. Slow subscribers drop.
func (e *Engine) Subscribe() chan Change {
	ch := make(chan Change, 256)
	e.mu.Lock()
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber.
func (e *Engine) Unsubscribe(ch chan Change) {
	e.mu.Lock()
	delete(e.subs, ch)
	e.mu.Unlock()
}

// Broadcast sends a change to every subscriber.
func (e *Engine) Broadcast(c Change) {
	e.mu.Lock()
	for ch := range e.subs {
		select {
		case ch <- c:
		default:
		}
	}
	e.mu.Unlock()
}

// Run consumes inputs until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	events := make(chan model.Event, 4096)
	for _, src := range e.Events {
		go func(src <-chan model.Event) {
			for {
				select {
				case <-ctx.Done():
					return
				case ev := <-src:
					select {
					case events <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}(src)
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-e.Devices:
			e.onDevice(s)
		case s := <-e.Interfaces:
			e.onInterface(s)
		case ev := <-events:
			e.onEvent(ev)
		case <-tick.C:
			e.housekeeping()
		}
	}
}

func (e *Engine) rule(id string) (model.Rule, bool) {
	r, ok := e.st.Rule(id)
	if !ok || !r.Enabled {
		return r, false
	}
	return r, true
}

// ---- transitions ----

func (e *Engine) transition(o *objState, to model.Status, now time.Time) (from model.Status, changed bool) {
	from = o.status
	if from == to {
		return from, false
	}
	o.status, o.since = to, now
	if to != model.StatusUnknown && from != model.StatusUnknown {
		o.transitions = append(o.transitions, now)
		cut := now.Add(-10 * time.Minute)
		var keep []time.Time
		for _, t := range o.transitions {
			if t.After(cut) {
				keep = append(keep, t)
			}
		}
		o.transitions = keep
	}
	return from, true
}

func (e *Engine) isFlapping(o *objState, now time.Time) bool {
	r, ok := e.rule("flapping")
	if !ok {
		return false
	}
	limit := r.ForCycles
	if limit <= 0 {
		limit = 5
	}
	cut := now.Add(-10 * time.Minute)
	n := 0
	for _, t := range o.transitions {
		if t.After(cut) {
			n++
		}
	}
	return n > limit
}

// ---- devices ----

func (e *Engine) onDevice(s model.DeviceSample) {
	d, err := e.st.Device(s.DeviceID)
	if err != nil {
		return
	}
	now := s.TS
	if now.IsZero() {
		now = time.Now()
	}
	e.mu.Lock()
	o := e.dev[d.ID]
	if o == nil {
		o = newObj()
		e.dev[d.ID] = o
	}
	o.lastSample = now
	inMaint := e.st.InMaintenance(now, d.SiteID, d.ID)
	reachable := s.Reachable || s.SNMPOK
	if reachable {
		o.okStreak++
		o.failStreak = 0
		o.lastSeen = now
	} else {
		o.failStreak++
		o.okStreak = 0
	}
	// metric rules → degraded reasons. Alert calls are deferred until the
	// engine lock is released (Broadcast takes the same lock).
	var post []func()
	degraded := []string{}
	check := func(ruleID string, value float64, have bool) {
		r, ok := e.rule(ruleID)
		if !ok || !have {
			return
		}
		if value >= r.Enter && !o.metricOn[ruleID] {
			o.metricRuns[ruleID]++
			if o.metricRuns[ruleID] >= max1(r.ForCycles) {
				o.metricOn[ruleID] = true
			}
		} else if value <= r.Exit {
			o.metricRuns[ruleID] = 0
			if o.metricOn[ruleID] {
				o.metricOn[ruleID] = false
				post = append(post, func() { e.resolveAlert(ruleID+":"+d.ID, now, fmt.Sprintf("%s back to %.0f", ruleID, value)) })
			}
		}
		if o.metricOn[ruleID] {
			degraded = append(degraded, ruleID)
			post = append(post, func() { e.raiseMetric(r, d, value) })
		}
	}
	if reachable {
		check("icmp_loss", s.LossPct, s.Reachable)
		check("icmp_latency", s.RTTms, s.Reachable && s.LossPct < 100)
		check("device_cpu_high", s.CPU, s.CPU >= 0)
		check("device_mem_high", s.MemPct, s.MemPct >= 0)
		check("device_temp_high", s.TempC, s.TempC > -1000)
	}
	// SNMP unreachable while ICMP fine
	if s.Reachable && !s.SNMPOK && !d.PingOnly {
		o.metricRuns["snmp_unreachable"]++
		if r, ok := e.rule("snmp_unreachable"); ok && o.metricRuns["snmp_unreachable"] >= max1(r.ForCycles) && !o.metricOn["snmp_unreachable"] {
			o.metricOn["snmp_unreachable"] = true
			errText := strings.TrimPrefix(s.Err, "snmp: ")
			post = append(post, func() {
				e.openAlert(model.Alert{Rule: "snmp_unreachable", Severity: r.Severity, DeviceID: d.ID, SiteID: d.SiteID, Domain: d.Domain,
					Title: fmt.Sprintf("SNMP not answering: %s", d.Name), Detail: errText + " — device answers ping; check community/user, ACL or the agent.",
					DedupKey: "snmp_unreachable:" + d.ID}, now, "snmp")
			})
		}
	} else if s.SNMPOK && o.metricOn["snmp_unreachable"] {
		o.metricOn["snmp_unreachable"] = false
		o.metricRuns["snmp_unreachable"] = 0
		post = append(post, func() { e.resolveAlert("snmp_unreachable:"+d.ID, now, "SNMP answering again") })
	}

	// decide status
	downRule, _ := e.rule("device_down")
	need := max1(downRule.ForCycles)
	next := o.status
	switch {
	case inMaint:
		next = model.StatusMaintenance
	case !reachable && o.failStreak >= need:
		next = model.StatusDown
	case !reachable && o.status == model.StatusUnknown:
		next = model.StatusUnknown
	case !reachable:
		// not yet confirmed: keep previous status
		next = o.status
		if next == model.StatusMaintenance {
			next = model.StatusUp
		}
	case reachable && (o.status == model.StatusDown || o.status == model.StatusUnreachable) && o.okStreak < 2:
		next = o.status // wait for 2 good cycles before recovery
	case len(degraded) > 0:
		next = model.StatusDegraded
	default:
		next = model.StatusUp
	}
	if next == model.StatusDown {
		// topology suppression: is an upstream device already down?
		if parent := e.upstreamLocked(d.ID); parent != "" {
			if po := e.dev[parent]; po != nil && (po.status == model.StatusDown || po.status == model.StatusUnreachable) {
				next = model.StatusUnreachable
			}
		}
	}
	from, changed := e.transition(o, next, now)
	flapping := e.isFlapping(o, now)
	e.mu.Unlock()
	for _, f := range post {
		f()
	}

	cause := ""
	if next == model.StatusUnreachable {
		cause = e.upstream(d.ID)
	}
	e.st.UpdateDevice(d.ID, func(x *model.Device) {
		x.Status = next
		if changed {
			x.StatusSince = now
		}
		x.LastPoll = now
		if reachable {
			x.LastSeen = now
		}
		x.SNMPOK = s.SNMPOK
		if s.Uptime >= 0 {
			x.Uptime = s.Uptime
		}
		x.Cause = cause
		if x.Metrics == nil {
			x.Metrics = map[string]float64{}
		}
		x.Metrics["rtt_ms"], x.Metrics["loss_pct"] = s.RTTms, s.LossPct
		if s.CPU >= 0 {
			x.Metrics["cpu_pct"] = s.CPU
		}
		if s.MemPct >= 0 {
			x.Metrics["mem_pct"] = s.MemPct
		}
		if s.TempC > -1000 {
			x.Metrics["temp_c"] = s.TempC
		}
		if s.Sessions >= 0 {
			x.Metrics["sessions"] = s.Sessions
		}
	})
	if changed {
		e.onDeviceTransition(d, from, next, now, s, flapping)
	}
	dd, _ := e.st.Device(d.ID)
	e.Broadcast(Change{Type: "device", Data: dd})
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func (e *Engine) onDeviceTransition(d model.Device, from, to model.Status, now time.Time, s model.DeviceSample, flapping bool) {
	e.st.AddEvent(model.Event{TS: now, Kind: "device_status", DeviceID: d.ID, Source: "state", Domain: d.Domain,
		Severity: severityForStatus(to), Message: fmt.Sprintf("%s: %s → %s", d.Name, from, to), DedupKey: "device_status:" + d.ID})
	rule, _ := e.rule("device_down")
	switch to {
	case model.StatusDown:
		sev := rule.Severity
		if sev == "" {
			sev = model.SevMajor
		}
		if d.Role == model.RoleCore || d.Role == model.RoleRouter || d.Role == model.RoleFirewall {
			sev = model.SevCritical
		}
		impact := e.impactOf(d.ID)
		a := model.Alert{Rule: "device_down", Severity: sev, DeviceID: d.ID, SiteID: d.SiteID, Domain: d.Domain,
			Title: fmt.Sprintf("Device down: %s", d.Name), Detail: fmt.Sprintf("%s (%s) stopped answering. Last seen %s.", d.Name, d.IP, humanAgo(d.LastSeen, now)),
			DedupKey: "device_down:" + d.ID, Impact: impact}
		if flapping {
			e.openFlapping(d.ID, d.SiteID, d.Domain, "Device flapping: "+d.Name, now)
			return
		}
		e.openAlert(a, now, "icmp/snmp")
		e.foldChildren(d.ID, now)
		e.evaluateSite(d.SiteID, now)
	case model.StatusUnreachable:
		parent := e.upstream(d.ID)
		pn := parent
		if pd, err := e.st.Device(parent); err == nil {
			pn = pd.Name
		}
		// fold under the parent's alert when it exists
		if pa, ok := e.st.AlertByDedup("device_down:" + parent); ok {
			a := model.Alert{Rule: "device_down", Severity: model.SevMinor, DeviceID: d.ID, SiteID: d.SiteID, Domain: d.Domain,
				Title: fmt.Sprintf("Unreachable: %s (caused by %s)", d.Name, pn), Detail: "Suppressed — upstream " + pn + " is down.", DedupKey: "device_down:" + d.ID, RootCause: pa.ID}
			e.openAlert(a, now, "topology")
			kids := 0
			for _, k := range e.st.Alerts() {
				if k.RootCause == pa.ID && k.State != model.AlertResolved {
					kids++
				}
			}
			e.st.UpdateAlert(pa.ID, func(x *model.Alert) { x.Children = kids; x.UpdatedAt = now; x.Impact = e.impactOf(parent) })
		}
	case model.StatusUp, model.StatusDegraded:
		if from == model.StatusDown || from == model.StatusUnreachable || from == model.StatusFlapping || from == model.StatusUnknown {
			e.resolveAlert("device_down:"+d.ID, now, fmt.Sprintf("%s answering again", d.Name))
			e.resolveAlert("flapping:"+d.ID, now, "stable again")
			// children may recover on their next poll; nudge them by re-evaluating site
			e.evaluateSite(d.SiteID, now)
		}
	case model.StatusMaintenance:
		e.resolveAlert("device_down:"+d.ID, now, "maintenance window")
	}
}

func humanAgo(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "moments ago"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func severityForStatus(s model.Status) model.Severity {
	switch s {
	case model.StatusDown, model.StatusFlapping:
		return model.SevMajor
	case model.StatusDegraded, model.StatusUnreachable:
		return model.SevMinor
	}
	return model.SevInfo
}

func (e *Engine) raiseMetric(r model.Rule, d model.Device, value float64) {
	titles := map[string]string{
		"icmp_loss": "Packet loss %.0f%% to %s", "icmp_latency": "High latency %.0f ms to %s", "device_cpu_high": "CPU %.0f%% on %s",
		"device_mem_high": "Memory %.0f%% on %s", "device_temp_high": "Temperature %.0f °C on %s",
	}
	t := titles[r.ID]
	if t == "" {
		t = r.ID + " %.0f on %s"
	}
	sev := r.Severity
	if r.Escalate > 0 && value >= r.Escalate {
		sev = escalate(sev)
	}
	e.openAlert(model.Alert{Rule: r.ID, Severity: sev, DeviceID: d.ID, SiteID: d.SiteID, Domain: d.Domain,
		Title: fmt.Sprintf(t, value, d.Name), Detail: r.Description, DedupKey: r.ID + ":" + d.ID}, time.Now(), "snmp")
}

func escalate(s model.Severity) model.Severity {
	switch s {
	case model.SevInfo:
		return model.SevMinor
	case model.SevMinor:
		return model.SevMajor
	default:
		return model.SevCritical
	}
}

// ---- interfaces ----

func (e *Engine) onInterface(s model.InterfaceSample) {
	i, err := e.st.Interface(s.IfID)
	if err != nil {
		return
	}
	d, err := e.st.Device(s.DeviceID)
	if err != nil {
		return
	}
	now := s.TS
	if now.IsZero() {
		now = time.Now()
	}
	e.mu.Lock()
	o := e.ifs[i.ID]
	if o == nil {
		o = newObj()
		e.ifs[i.ID] = o
	}
	o.lastSample = now
	next := o.status
	devState := e.dev[d.ID]
	switch {
	case devState != nil && (devState.status == model.StatusDown || devState.status == model.StatusUnreachable):
		next = model.StatusUnknown
	case devState != nil && devState.status == model.StatusMaintenance:
		next = model.StatusMaintenance
	case !s.AdminUp:
		next = model.StatusUnknown // administratively down: not a fault
	case !s.OperUp:
		next = model.StatusDown
	default:
		next = model.StatusUp
	}
	// utilisation / errors with hysteresis (important interfaces only)
	var post []func()
	degraded := false
	if next == model.StatusUp && s.HaveRates {
		for _, rid := range []string{"interface_util_high", "interface_errors"} {
			r, ok := e.rule(rid)
			if !ok || (r.OnlyImport && !i.Important) {
				continue
			}
			var v float64
			if rid == "interface_util_high" {
				v = s.InUtil
				if s.OutUtil > v {
					v = s.OutUtil
				}
			} else {
				v = s.InErrRate + s.OutErrRate
			}
			if v >= r.Enter {
				o.metricRuns[rid]++
				if o.metricRuns[rid] >= max1(r.ForCycles) && !o.metricOn[rid] {
					o.metricOn[rid] = true
					sev := r.Severity
					if r.Escalate > 0 && v >= r.Escalate {
						sev = escalate(sev)
					}
					title := fmt.Sprintf("Utilisation %.0f%% on %s %s", v, d.Name, i.Name)
					if rid == "interface_errors" {
						title = fmt.Sprintf("Errors %.1f/s on %s %s", v, d.Name, i.Name)
					}
					e.mu.Unlock()
					e.openAlert(model.Alert{Rule: rid, Severity: sev, DeviceID: d.ID, SiteID: d.SiteID, Domain: d.Domain, Object: i.ID, Title: title,
						Detail: fmt.Sprintf("%s · %s · %d Mbps", i.Alias, r.Description, i.SpeedMbps), DedupKey: rid + ":" + i.ID}, now, "snmp")
					e.mu.Lock()
				}
			} else if v <= r.Exit {
				o.metricRuns[rid] = 0
				if o.metricOn[rid] {
					o.metricOn[rid] = false
					rid, v := rid, v
					post = append(post, func() { e.resolveAlert(rid+":"+i.ID, now, fmt.Sprintf("%s %s back to %.1f", d.Name, i.Name, v)) })
				}
			}
			if o.metricOn[rid] {
				degraded = true
			}
		}
	}
	if degraded {
		next = model.StatusDegraded
	}
	from, changed := e.transition(o, next, now)
	flapping := e.isFlapping(o, now)
	e.mu.Unlock()
	for _, f := range post {
		f()
	}

	e.st.UpdateInterface(i.ID, func(x *model.Interface) {
		x.Status = next
		if changed {
			x.StatusSince = now
			x.LastChange = now
		}
		x.OperUp, x.AdminUp = s.OperUp, s.AdminUp
		if s.HaveRates {
			x.InBps, x.OutBps, x.InUtil, x.OutUtil, x.InErrRate, x.OutErrRate = s.InBps, s.OutBps, s.InUtil, s.OutUtil, s.InErrRate, s.OutErrRate
			x.InPps, x.OutPps, x.InDropRate, x.OutDropRate = s.InPps, s.OutPps, s.InDropRate, s.OutDropRate
		}
	})
	if changed {
		e.onInterfaceTransition(d, i, from, next, now, flapping)
	}
	e.updateLinkStatus(i, next, s)
	if changed || s.HaveRates {
		ii, _ := e.st.Interface(i.ID)
		e.Broadcast(Change{Type: "interface", Data: ii})
	}
}

func (e *Engine) onInterfaceTransition(d model.Device, i model.Interface, from, to model.Status, now time.Time, flapping bool) {
	label := i.Name
	if i.Alias != "" {
		label += " (" + i.Alias + ")"
	}
	kind := "interface_status"
	sev := model.SevInfo
	if to == model.StatusDown && i.Important {
		sev = model.SevMajor
	}
	// transitions to/from unknown (first observation, device offline) are not
	// news on their own — the device-level event covers them
	if from != model.StatusUnknown && to != model.StatusUnknown {
		e.st.AddEvent(model.Event{TS: now, Kind: kind, DeviceID: d.ID, Object: i.ID, Source: "state", Domain: d.Domain, Severity: sev,
			Message: fmt.Sprintf("%s %s: %s → %s", d.Name, label, from, to), DedupKey: "interface_status:" + i.ID})
	}
	r, ok := e.rule("interface_down")
	if !ok {
		return
	}
	switch to {
	case model.StatusDown:
		if r.OnlyImport && !i.Important {
			return
		}
		if flapping {
			e.openFlapping(i.ID, d.SiteID, d.Domain, fmt.Sprintf("Interface flapping: %s %s", d.Name, i.Name), now)
			return
		}
		peer := e.peerOf(d.ID, i.Name)
		title := fmt.Sprintf("Link down: %s %s", d.Name, i.Name)
		if peer != "" {
			title += " → " + peer
		}
		e.openAlert(model.Alert{Rule: "interface_down", Severity: r.Severity, DeviceID: d.ID, SiteID: d.SiteID, Domain: d.Domain, Object: i.ID,
			Title: title, Detail: strings.TrimSpace(i.Alias + " · " + fmt.Sprintf("%d Mbps", i.SpeedMbps)), DedupKey: "interface_down:" + i.ID, Impact: e.impactOf(d.ID)}, now, "state")
	case model.StatusUp, model.StatusDegraded:
		if from == model.StatusDown || from == model.StatusFlapping {
			e.resolveAlert("interface_down:"+i.ID, now, fmt.Sprintf("%s %s up again", d.Name, i.Name))
			e.resolveAlert("flapping:"+i.ID, now, "stable again")
		}
	}
}

// peerOf names the device at the other end of an interface's link.
func (e *Engine) peerOf(deviceID, ifName string) string {
	for _, l := range e.st.Links() {
		if l.Stale {
			continue
		}
		var other, otherIf string
		switch {
		case l.ADevice == deviceID && strings.EqualFold(l.AIf, ifName):
			other, otherIf = l.BDevice, l.BIf
		case l.BDevice == deviceID && strings.EqualFold(l.BIf, ifName):
			other, otherIf = l.ADevice, l.AIf
		default:
			continue
		}
		if l.External {
			return l.ExternalName + " " + otherIf
		}
		if od, err := e.st.Device(other); err == nil {
			return od.Name + " " + otherIf
		}
	}
	return ""
}

func (e *Engine) updateLinkStatus(i model.Interface, st model.Status, s model.InterfaceSample) {
	for _, l := range e.st.Links() {
		if (l.ADevice == i.DeviceID && strings.EqualFold(l.AIf, i.Name)) || (l.BDevice == i.DeviceID && strings.EqualFold(l.BIf, i.Name)) {
			lid := l.ID
			e.st.UpdateLink(lid, func(x *model.Link) {
				switch st {
				case model.StatusDown:
					x.Status = model.StatusDown
				case model.StatusUp, model.StatusDegraded:
					x.Status = model.StatusUp
				}
				if s.HaveRates {
					u := s.InUtil
					if s.OutUtil > u {
						u = s.OutUtil
					}
					x.Util = u
				}
			})
			if ll, err := e.st.Link(lid); err == nil {
				e.Broadcast(Change{Type: "link", Data: ll})
			}
		}
	}
}

// ---- events (trap / syslog / topology / discovery / poller) ----

func (e *Engine) onEvent(ev model.Event) {
	if ev.TS.IsZero() {
		ev.TS = time.Now()
	}
	if ev.ID == "" {
		ev.ID = model.NewID("evt")
	}
	e.st.AddEvent(ev)
	e.Broadcast(Change{Type: "event", Data: ev})
	d, _ := e.st.Device(ev.DeviceID)
	now := ev.TS
	switch ev.Kind {
	case "link_down", "link_up":
		// Hard evidence: move the interface immediately; polling confirms.
		if ev.Object == "" {
			return
		}
		i, err := e.st.Interface(ev.Object)
		if err != nil {
			return
		}
		e.mu.Lock()
		o := e.ifs[i.ID]
		if o == nil {
			o = newObj()
			e.ifs[i.ID] = o
		}
		target := model.StatusUp
		if ev.Kind == "link_down" {
			target = model.StatusDown
		}
		from, changed := e.transition(o, target, now)
		flapping := e.isFlapping(o, now)
		e.mu.Unlock()
		if changed {
			e.st.UpdateInterface(i.ID, func(x *model.Interface) {
				x.Status = target
				x.StatusSince = now
				x.LastChange = now
				x.OperUp = target == model.StatusUp
			})
			e.addEvidence("interface_down:"+i.ID, ev.Source, now)
			e.onInterfaceTransition(d, i, from, target, now, flapping)
			e.updateLinkStatus(i, target, model.InterfaceSample{})
			ii, _ := e.st.Interface(i.ID)
			e.Broadcast(Change{Type: "interface", Data: ii})
		} else {
			e.addEvidence("interface_down:"+i.ID, ev.Source, now)
		}
	case "probe_ok", "tls_ok", "config_backup_ok", "lag_member_up", "sdwan_link_up", "integration_ok":
		e.resolveAlert(ev.DedupKey, now, ev.Message)
	case "bgp_neighbor_up", "ospf_adjacency_up":
		if ev.Kind == "bgp_neighbor_up" {
			e.resolveAlert(ev.DedupKey, now, ev.Message)
		} else {
			e.resolveAlert("ospf:"+ev.DeviceID, now, ev.Message)
		}
	case "auth_failure":
		e.mu.Lock()
		w := append(e.authWin[ev.DeviceID], now)
		cut := now.Add(-5 * time.Minute)
		var keep []time.Time
		for _, t := range w {
			if t.After(cut) {
				keep = append(keep, t)
			}
		}
		e.authWin[ev.DeviceID] = keep
		n := len(keep)
		e.mu.Unlock()
		r, ok := e.rule("auth_failure")
		if ok && n >= int(max1(int(r.Enter))) {
			e.openAlert(model.Alert{Rule: "auth_failure", Severity: r.Severity, DeviceID: d.ID, SiteID: d.SiteID, Domain: d.Domain,
				Title: fmt.Sprintf("%d authentication failures on %s in 5 minutes", n, d.Name), Detail: ev.Message, DedupKey: "auth_failure:" + d.ID}, now, ev.Source)
		}
	case "device_status", "interface_status", "link_new", "link_lost", "device_discovered":
		// informational only
	default:
		// generic event rule: any rule with Object == "event" and id == kind
		r, ok := e.rule(ev.Kind)
		if !ok || r.Object != "event" {
			return
		}
		if r.Severity == model.SevInfo {
			return
		}
		key := ev.DedupKey
		if key == "" {
			key = ev.Kind + ":" + ev.DeviceID
		}
		siteID, domain := d.SiteID, d.Domain
		if domain == "" {
			domain = ev.Domain
		}
		title := ev.Message
		if len(title) > 120 {
			title = title[:120] + "…"
		}
		e.openAlert(model.Alert{Rule: ev.Kind, Severity: r.Severity, DeviceID: ev.DeviceID, SiteID: siteID, Domain: domain, Object: ev.Object,
			Title: title, Detail: ev.Message, DedupKey: key}, now, ev.Source)
	}
}

func (e *Engine) addEvidence(dedup, source string, now time.Time) {
	if a, ok := e.st.AlertByDedup(dedup); ok {
		e.st.UpdateAlert(a.ID, func(x *model.Alert) {
			if len(x.Evidence) < 20 {
				x.Evidence = append(x.Evidence, source+" "+now.Format("15:04:05"))
			}
		})
	}
}

// ---- alerts ----

func (e *Engine) openAlert(a model.Alert, now time.Time, evidence string) {
	if a.DedupKey == "" {
		a.DedupKey = a.Rule + ":" + a.DeviceID + ":" + a.Object
	}
	if existing, ok := e.st.AlertByDedup(a.DedupKey); ok {
		e.st.UpdateAlert(existing.ID, func(x *model.Alert) {
			x.Occurrences++
			x.UpdatedAt = now
			if a.Severity.Rank() > x.Severity.Rank() {
				x.Severity = a.Severity
			}
			if len(x.Evidence) < 20 {
				x.Evidence = append(x.Evidence, evidence+" "+now.Format("15:04:05"))
			}
			if a.Impact != "" {
				x.Impact = a.Impact
			}
			// the same condition re-evaluated as a symptom of an upstream
			// failure: fold it under the root cause and downgrade
			if a.RootCause != "" && x.RootCause != a.RootCause {
				x.RootCause = a.RootCause
				x.Title, x.Detail = a.Title, a.Detail
				x.Severity = a.Severity
			}
		})
		ea, _ := e.st.Alert(existing.ID)
		e.Broadcast(Change{Type: "alert", Data: ea})
		return
	}
	// Re-open a recently resolved alert (within 30 min) instead of a new one.
	for _, old := range e.st.Alerts() {
		if old.DedupKey == a.DedupKey && old.State == model.AlertResolved && now.Sub(old.ResolvedAt) < 30*time.Minute {
			e.st.UpdateAlert(old.ID, func(x *model.Alert) {
				x.State = model.AlertOpen
				x.Occurrences++
				x.UpdatedAt = now
				x.ResolvedAt = time.Time{}
				x.Notified = false
				x.Evidence = append(x.Evidence, evidence+" "+now.Format("15:04:05")+" (re-opened)")
			})
			ra, _ := e.st.Alert(old.ID)
			e.Broadcast(Change{Type: "alert", Data: ra})
			e.queueNotify(ra)
			return
		}
	}
	a.ID = model.NewID("alr")
	a.State = model.AlertOpen
	a.OpenedAt, a.UpdatedAt = now, now
	a.Occurrences = 1
	a.Evidence = []string{evidence + " " + now.Format("15:04:05")}
	if a.Domain == "" {
		a.Domain = model.DomainNetwork
	}
	e.st.PutAlert(a)
	e.Broadcast(Change{Type: "alert", Data: a})
	if a.RootCause == "" {
		e.queueNotify(a)
	}
}

func (e *Engine) resolveAlert(dedup string, now time.Time, note string) {
	a, ok := e.st.AlertByDedup(dedup)
	if !ok {
		return
	}
	e.st.UpdateAlert(a.ID, func(x *model.Alert) {
		x.State = model.AlertResolved
		x.ResolvedAt = now
		x.UpdatedAt = now
		if note != "" && len(x.Evidence) < 20 {
			x.Evidence = append(x.Evidence, "resolved "+now.Format("15:04:05")+": "+note)
		}
	})
	ra, _ := e.st.Alert(a.ID)
	e.Broadcast(Change{Type: "alert", Data: ra})
	if ra.RootCause == "" {
		e.queueNotify(ra)
	}
	// children of a root cause are re-evaluated by their own polls
}

func (e *Engine) openFlapping(objectID, siteID string, domain model.Domain, title string, now time.Time) {
	r, ok := e.rule("flapping")
	if !ok {
		return
	}
	deviceID := objectID
	if i := strings.IndexByte(objectID, ':'); i > 0 {
		deviceID = objectID[:i]
	}
	e.openAlert(model.Alert{Rule: "flapping", Severity: r.Severity, DeviceID: deviceID, SiteID: siteID, Domain: domain, Object: objectID,
		Title: title, Detail: "More than 5 state changes in 10 minutes; individual transitions are not alerted while flapping.", DedupKey: "flapping:" + objectID}, now, "state")
}

func (e *Engine) queueNotify(a model.Alert) {
	if a.State == model.AlertAcked {
		return
	}
	select {
	case e.Notify <- a:
	default:
	}
}

// Ack marks an alert acknowledged.
func (e *Engine) Ack(id, user, note string) bool {
	ok := e.st.UpdateAlert(id, func(x *model.Alert) {
		if x.State == model.AlertOpen {
			x.State = model.AlertAcked
		}
		x.AckedBy, x.AckNote, x.UpdatedAt = user, note, time.Now()
	})
	if ok {
		a, _ := e.st.Alert(id)
		e.Broadcast(Change{Type: "alert", Data: a})
	}
	return ok
}

// Resolve closes an alert manually.
func (e *Engine) Resolve(id, user string) bool {
	now := time.Now()
	ok := e.st.UpdateAlert(id, func(x *model.Alert) {
		x.State = model.AlertResolved
		x.ResolvedAt, x.UpdatedAt = now, now
		x.Evidence = append(x.Evidence, "resolved manually by "+user+" "+now.Format("15:04:05"))
	})
	if ok {
		a, _ := e.st.Alert(id)
		e.Broadcast(Change{Type: "alert", Data: a})
	}
	return ok
}

// ---- topology helpers ----

// upstream returns the neighbour of dev on the shortest path to the root.
// Must be called WITHOUT e.mu held.
func (e *Engine) upstream(dev string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.upstreamLocked(dev)
}

// upstreamLocked is upstream for callers that already hold e.mu.
func (e *Engine) upstreamLocked(dev string) string {
	if time.Since(e.parentsAt) > time.Minute {
		e.parents, e.parentsAt = e.computeParents(), time.Now()
	}
	return e.parents[dev]
}

// InvalidateTopology forces the parent map to be recomputed.
func (e *Engine) InvalidateTopology() {
	e.mu.Lock()
	e.parentsAt = time.Time{}
	e.mu.Unlock()
}

func (e *Engine) rebuildParents() {
	parents := e.computeParents()
	e.mu.Lock()
	e.parents, e.parentsAt = parents, time.Now()
	e.mu.Unlock()
}

// computeParents walks the link graph from each site's root and returns the
// parent (upstream neighbour) of every reachable device. It touches only the
// store, never e.mu, so it is safe under the engine lock.
func (e *Engine) computeParents() map[string]string {
	links := e.st.Links()
	devs := e.st.Devices()
	adj := map[string][]string{}
	for _, l := range links {
		if l.External || l.Stale {
			continue
		}
		adj[l.ADevice] = append(adj[l.ADevice], l.BDevice)
		adj[l.BDevice] = append(adj[l.BDevice], l.ADevice)
	}
	// roots: configured root device, else the core (highest tier) per site
	roots := map[string]bool{}
	if e.RootDevice != "" {
		roots[e.RootDevice] = true
	}
	bySite := map[string][]model.Device{}
	for _, d := range devs {
		bySite[d.SiteID] = append(bySite[d.SiteID], d)
	}
	rank := func(r model.Role) int {
		switch r {
		case model.RoleCore:
			return 4
		case model.RoleRouter, model.RoleFirewall:
			return 3
		case model.RoleDist:
			return 2
		case model.RoleAccess:
			return 1
		}
		return 0
	}
	for _, ds := range bySite {
		best := ""
		bestRank, bestDeg := -1, -1
		for _, d := range ds {
			r, deg := rank(d.Role), len(adj[d.ID])
			if r > bestRank || (r == bestRank && deg > bestDeg) {
				best, bestRank, bestDeg = d.ID, r, deg
			}
		}
		if best != "" && bestDeg > 0 {
			roots[best] = true
		}
	}
	parents := map[string]string{}
	visited := map[string]bool{}
	var queue []string
	for r := range roots {
		visited[r] = true
		queue = append(queue, r)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		ns := append([]string(nil), adj[cur]...)
		sort.Strings(ns)
		for _, n := range ns {
			if !visited[n] {
				visited[n] = true
				parents[n] = cur
				queue = append(queue, n)
			}
		}
	}
	return parents
}

// impactOf counts devices downstream of dev (children in the parent tree).
func (e *Engine) impactOf(dev string) string {
	e.upstream(dev) // ensure fresh
	e.mu.Lock()
	defer e.mu.Unlock()
	children := map[string][]string{}
	for c, p := range e.parents {
		children[p] = append(children[p], c)
	}
	n := 0
	var stack = []string{dev}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, c := range children[cur] {
			n++
			stack = append(stack, c)
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d downstream device(s)", n)
}

// foldChildren marks downstream devices unreachable right away when an
// upstream device is confirmed down, without waiting for their own cycles.
func (e *Engine) foldChildren(dev string, now time.Time) {
	e.upstream(dev)
	e.mu.Lock()
	var kids []string
	for c, p := range e.parents {
		if p == dev {
			kids = append(kids, c)
		}
	}
	e.mu.Unlock()
	for _, k := range kids {
		e.mu.Lock()
		o := e.dev[k]
		if o == nil || o.status != model.StatusDown {
			e.mu.Unlock()
			continue
		}
		from, changed := e.transition(o, model.StatusUnreachable, now)
		e.mu.Unlock()
		if changed {
			kd, err := e.st.Device(k)
			if err != nil {
				continue
			}
			e.st.UpdateDevice(k, func(x *model.Device) { x.Status = model.StatusUnreachable; x.StatusSince = now; x.Cause = dev })
			e.onDeviceTransition(kd, from, model.StatusUnreachable, now, model.DeviceSample{}, false)
			kd2, _ := e.st.Device(k)
			e.Broadcast(Change{Type: "device", Data: kd2})
		}
	}
}

// evaluateSite raises/clears the site-down alert.
func (e *Engine) evaluateSite(siteID string, now time.Time) {
	r, ok := e.rule("site_down")
	if !ok || siteID == "" {
		return
	}
	total, down := 0, 0
	for _, d := range e.st.Devices() {
		if d.SiteID != siteID || !d.Monitored {
			continue
		}
		total++
		if d.Status == model.StatusDown || d.Status == model.StatusUnreachable {
			down++
		}
	}
	site, _ := e.st.Site(siteID)
	pct := r.Enter
	if pct <= 0 {
		pct = 80
	}
	if total >= 3 && float64(down)*100/float64(total) >= pct {
		e.openAlert(model.Alert{Rule: "site_down", Severity: r.Severity, SiteID: siteID, Domain: model.DomainNetwork,
			Title: fmt.Sprintf("Site down: %s (%d of %d devices)", site.Name, down, total), Detail: "Most of the site is unreachable — check WAN, power or the site's uplink. Device alerts are folded under this one.",
			DedupKey: "site_down:" + siteID, Impact: fmt.Sprintf("%d devices", down)}, now, "state")
	} else {
		e.resolveAlert("site_down:"+siteID, now, fmt.Sprintf("%d of %d devices down", down, total))
	}
}

// housekeeping runs auto-resolution of timed alerts and archival.
func (e *Engine) housekeeping() {
	now := time.Now()
	for _, a := range e.st.Alerts() {
		if a.State == model.AlertResolved {
			continue
		}
		r, ok := e.st.Rule(a.Rule)
		if ok && r.Object == "event" && r.ForCycles > 0 && now.Sub(a.UpdatedAt) > time.Duration(r.ForCycles)*time.Minute {
			e.resolveAlert(a.DedupKey, now, fmt.Sprintf("auto-resolved after %d minutes", r.ForCycles))
		}
		// child alerts whose root cause resolved: resolve too when device is up
		if a.RootCause != "" {
			if root, err := e.st.Alert(a.RootCause); err != nil || root.State == model.AlertResolved {
				if d, err := e.st.Device(a.DeviceID); err == nil && d.Status == model.StatusUp {
					e.resolveAlert(a.DedupKey, now, "root cause resolved")
				}
			}
		}
	}
	e.st.ArchiveResolved(24 * time.Hour)
	// stale samples: devices not polled for 5 intervals become unknown
	for _, d := range e.st.Devices() {
		if !d.Monitored || d.LastPoll.IsZero() {
			continue
		}
		every := time.Duration(max1(d.PollEvery)) * time.Second
		if now.Sub(d.LastPoll) > 5*every && d.Status != model.StatusUnknown {
			e.mu.Lock()
			if o := e.dev[d.ID]; o != nil {
				e.transition(o, model.StatusUnknown, now)
			}
			e.mu.Unlock()
			e.st.UpdateDevice(d.ID, func(x *model.Device) { x.Status = model.StatusUnknown; x.StatusSince = now })
		}
	}
}
