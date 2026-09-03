// Package poller schedules ICMP and SNMP polling of monitored devices, turns
// counters into rates, stores metrics and hands samples to the state engine.
package poller

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/endpoint"
	"github.com/nizartuanku/topolight/internal/gnmi"
	"github.com/nizartuanku/topolight/internal/icmp"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/profile"
	"github.com/nizartuanku/topolight/internal/snmp"
	"github.com/nizartuanku/topolight/internal/store"
)

// Poller owns the polling schedule.
// MetricSink receives samples; the local TSDB, or a cluster forwarder.
type MetricSink interface {
	Append(series string, t int64, v float64)
}

type Poller struct {
	st   *store.Store
	db   MetricSink
	lib  *profile.Library
	ping *icmp.Pinger
	// assigned, when non-nil, restricts polling to these device ids (cluster sharding)
	assigned map[string]bool
	// NoRouting disables the routing/L2 walk (cluster standbys: the leader owns it).
	NoRouting bool
	// Caps reports (licence device limit, monitored devices) so controller-discovered
	// access points stop at the cap; nil = unlimited.
	Caps func() (int, int)
	// InventoryAll makes this node run the 15-minute inventory walk for devices
	// polled elsewhere too (the cluster leader owns the inventory).
	InventoryAll bool

	Workers int
	// Outputs. The state engine must drain these.
	DeviceSamples    chan model.DeviceSample
	InterfaceSamples chan model.InterfaceSample
	Events           chan model.Event

	mu       sync.Mutex
	next     map[string]time.Time // device id -> next poll
	invAt    map[string]time.Time // last inventory walk
	counters map[string]*ifCounters
	uptime   map[string]int64
	inflight map[string]bool
	epAt     map[string]time.Time // last endpoint (FDB/ARP) walk
	clients  map[string]*snmp.Client
	gnmis    map[string]*gnmi.Client
	creds    map[string]string // device id -> cred fingerprint for client reuse
	// Endpoints (MAC/ARP tables); nil disables. EndpointEvery defaults to 5 minutes.
	Endpoints     *endpoint.Store
	EndpointEvery time.Duration
	// Stats
	Cycles   int64
	Failures int64
	EPWalks  int64
}

type ifCounters struct {
	ts            time.Time
	inOct, outOct uint64
	inErr, outErr uint64
	inPkt, outPkt uint64 // unicast packets (HC when the agent has ifXTable)
	inDrp, outDrp uint64 // ifInDiscards / ifOutDiscards
	hc, hcPkt     bool
	uptime        int64
	lastStore     time.Time // last time a point was written to the tsdb
}

// New creates a poller. ping may be nil (ICMP disabled).
func New(st *store.Store, db MetricSink, lib *profile.Library, ping *icmp.Pinger) *Poller {
	return &Poller{st: st, db: db, lib: lib, ping: ping, Workers: 48,
		DeviceSamples: make(chan model.DeviceSample, 4096), InterfaceSamples: make(chan model.InterfaceSample, 65536), Events: make(chan model.Event, 4096),
		next: map[string]time.Time{}, invAt: map[string]time.Time{}, counters: map[string]*ifCounters{}, uptime: map[string]int64{},
		inflight: map[string]bool{}, epAt: map[string]time.Time{}, clients: map[string]*snmp.Client{}, creds: map[string]string{}}
}

// Run schedules until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	sem := make(chan struct{}, p.Workers)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		for _, d := range p.st.Devices() {
			if !d.Monitored || d.Managed != "" {
				// managed devices are reported by their controller integration
				continue
			}
			p.mu.Lock()
			if p.assigned != nil && !p.assigned[d.ID] {
				// not ours to poll; the leader still keeps the inventory fresh
				invDue := p.InventoryAll && !d.PingOnly && (p.invAt[d.ID].IsZero() || now.Sub(p.invAt[d.ID]) > 15*time.Minute) && !p.inflight[d.ID]
				if invDue {
					p.inflight[d.ID] = true
					p.invAt[d.ID] = now
				}
				p.mu.Unlock()
				if invDue {
					select {
					case sem <- struct{}{}:
					case <-ctx.Done():
						return
					}
					go func(d model.Device) {
						defer func() { <-sem }()
						p.inventoryOnly(ctx, d)
						p.mu.Lock()
						p.inflight[d.ID] = false
						p.mu.Unlock()
					}(d)
				}
				continue
			}
			nx, ok := p.next[d.ID]
			if !ok {
				// spread first polls over the interval
				nx = now.Add(time.Duration(rand.Intn(max(1, d.PollEvery))) * time.Second)
				p.next[d.ID] = nx
			}
			due := !now.Before(nx) && !p.inflight[d.ID]
			if due {
				p.inflight[d.ID] = true
			}
			p.mu.Unlock()
			if !due {
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func(d model.Device) {
				defer func() { <-sem }()
				p.pollDevice(ctx, d)
				p.mu.Lock()
				p.inflight[d.ID] = false
				every := d.PollEvery
				if every <= 0 {
					every = 60
				}
				p.next[d.ID] = time.Now().Add(time.Duration(every) * time.Second)
				p.mu.Unlock()
			}(d)
		}
	}
}

// SetSink replaces the metric sink (cluster forwarder on standby nodes).
func (p *Poller) SetSink(s MetricSink) {
	p.mu.Lock()
	p.db = s
	p.mu.Unlock()
}

// SetAssigned restricts polling to the given device ids (nil = every device).
func (p *Poller) SetAssigned(ids []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ids == nil {
		p.assigned = nil
		return
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	p.assigned = m
}

// Assigned returns how many devices this node polls (-1 = all).
func (p *Poller) Assigned() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.assigned == nil {
		return -1
	}
	return len(p.assigned)
}

// inventoryOnly refreshes identity, interfaces and neighbours without
// touching counters (cluster leader, for devices polled by other nodes).
func (p *Poller) inventoryOnly(ctx context.Context, d model.Device) {
	c, err := p.ClientFor(d)
	if err != nil {
		return
	}
	vbs, err := c.Get(profile.OIDSysName)
	if err != nil {
		return
	}
	sysName := ""
	if len(vbs) > 0 {
		sysName = vbs[0].Value.String()
	}
	p.inventory(ctx, c, &d, sysName)
}

// PollNow asks for an immediate poll of a device (e.g. after a trap).
func (p *Poller) PollNow(deviceID string) {
	p.mu.Lock()
	p.next[deviceID] = time.Now()
	p.mu.Unlock()
}

// Forget drops per-device state (after deletion).
func (p *Poller) Forget(deviceID string) {
	if p.Endpoints != nil {
		p.Endpoints.Forget(deviceID)
	}
	p.mu.Lock()
	delete(p.epAt, deviceID)
	defer p.mu.Unlock()
	delete(p.next, deviceID)
	delete(p.invAt, deviceID)
	delete(p.uptime, deviceID)
	if c := p.clients[deviceID]; c != nil {
		c.Close()
	}
	delete(p.clients, deviceID)
	delete(p.creds, deviceID)
	for k := range p.counters {
		if strings.HasPrefix(k, deviceID+":") {
			delete(p.counters, k)
		}
	}
}

// ClientFor builds (or reuses) an SNMP client for a device.
func (p *Poller) ClientFor(d model.Device) (*snmp.Client, error) {
	cred, err := p.credFor(d)
	if err != nil {
		return nil, err
	}
	if cred.IsGNMI() {
		return nil, fmt.Errorf("credential %s is a gNMI credential, not SNMP", cred.Name)
	}
	fp := cred.ID + "|" + cred.Version + "|" + cred.Community + "|" + cred.User + "|" + cred.AuthProto + "|" + cred.AuthPass + "|" + cred.PrivProto + "|" + cred.PrivPass + "|" + d.IP
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.clients[d.ID]; c != nil && p.creds[d.ID] == fp {
		return c, nil
	}
	if c := p.clients[d.ID]; c != nil {
		c.Close()
	}
	c := NewClient(d.IP, cred)
	p.clients[d.ID] = c
	p.creds[d.ID] = fp
	return c, nil
}

// NewClient builds an SNMP client from a credential.
func NewClient(ip string, cred model.Credential) *snmp.Client {
	c := &snmp.Client{Addr: ip, Timeout: 2 * time.Second, Retries: 1, MaxRep: 30}
	if cred.Version == "3" {
		c.Version = snmp.V3
		c.User, c.AuthProto, c.AuthPass, c.PrivProto, c.PrivPass = cred.User, cred.AuthProto, cred.AuthPass, cred.PrivProto, cred.PrivPass
	} else {
		c.Version = snmp.V2c
		c.Community = cred.Community
	}
	return c
}

func (p *Poller) credFor(d model.Device) (model.Credential, error) {
	id := d.CredID
	if id == "" {
		if s, err := p.st.Site(d.SiteID); err == nil {
			id = s.CredID
		}
	}
	if id == "" {
		for _, c := range p.st.Creds() {
			if c.IsSNMP() {
				return c, nil
			}
		}
		return model.Credential{}, fmt.Errorf("no SNMP credential configured")
	}
	c, err := p.st.Cred(id)
	if err == nil && c.IsSSH() {
		return model.Credential{}, fmt.Errorf("credential %s is an SSH credential, not SNMP", c.Name)
	}
	return c, err
}

func (p *Poller) emitEvent(e model.Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	select {
	case p.Events <- e:
	default:
	}
}

func (p *Poller) pollDevice(ctx context.Context, d model.Device) {
	now := time.Now()
	p.mu.Lock()
	p.Cycles++
	p.mu.Unlock()
	ds := model.DeviceSample{DeviceID: d.ID, TS: now, Uptime: -1, CPU: -1, MemPct: -1, TempC: -1000, Sessions: -1}

	// ICMP
	if p.ping != nil {
		r, err := p.ping.Probe(d.IP, 4, 150*time.Millisecond, time.Second)
		if err == nil {
			ds.Reachable = r.Reachable()
			ds.LossPct = r.LossPct
			ds.RTTms = float64(r.AvgRTT.Microseconds()) / 1000
			ds.Jitterms = float64(r.Jitter.Microseconds()) / 1000
			p.db.Append("icmp_rtt_ms|"+d.ID, now.Unix(), ds.RTTms)
			p.db.Append("icmp_loss_pct|"+d.ID, now.Unix(), ds.LossPct)
		}
	}

	if d.PingOnly {
		// nothing more to ask: the sample carries reachability and RTT only
		if p.ping == nil {
			ds.Err = "ping-only device but ICMP is unavailable on this host"
		}
		p.publishDevice(ds)
		return
	}

	if cred, err := p.credFor(d); err == nil && cred.IsGNMI() {
		p.pollGNMI(ctx, d, cred, &ds)
		return
	}

	// SNMP
	c, err := p.ClientFor(d)
	if err != nil {
		ds.Err = err.Error()
		p.publishDevice(ds)
		return
	}
	vbs, err := c.Get(profile.OIDSysUpTime, profile.OIDSysName)
	if err != nil {
		ds.Err = "snmp: " + err.Error()
		p.mu.Lock()
		p.Failures++
		p.mu.Unlock()
		if p.ping == nil {
			ds.Reachable = false
		}
		p.publishDevice(ds)
		return
	}
	ds.SNMPOK = true
	if p.ping == nil {
		ds.Reachable = true
	}
	var uptimeTicks int64
	sysName := ""
	for _, vb := range vbs {
		switch vb.OID {
		case profile.OIDSysUpTime:
			uptimeTicks = vb.Value.Int
		case profile.OIDSysName:
			sysName = vb.Value.String()
		}
	}
	ds.Uptime = uptimeTicks / 100
	p.mu.Lock()
	prevUp, had := p.uptime[d.ID]
	p.uptime[d.ID] = ds.Uptime
	lastInv := p.invAt[d.ID]
	p.mu.Unlock()
	// uptime went backwards and is now small: a real reboot (a sysUpTime
	// wrap at 497 days also lands here, once every 16 months)
	if had && ds.Uptime < prevUp-5 && prevUp > 0 && ds.Uptime < 24*3600 {
		ds.Rebooted = true
		p.emitEvent(model.Event{Kind: "device_rebooted", DeviceID: d.ID, Source: "snmp", Severity: model.SevMajor, Domain: d.Domain,
			Message: fmt.Sprintf("%s rebooted (uptime %s → %s)", d.Name, fmtDur(prevUp), fmtDur(ds.Uptime)), DedupKey: "device_rebooted:" + d.ID})
	}

	// Inventory (identity, profile, interface table) every 15 minutes
	inventoryDue := lastInv.IsZero() || now.Sub(lastInv) > 15*time.Minute || ds.Rebooted
	prof := p.lib.Match(d.SysObjectID, d.SysDescr)
	if inventoryDue {
		if pr, ok := p.inventory(ctx, c, &d, sysName); ok {
			prof = pr
		}
		p.mu.Lock()
		p.invAt[d.ID] = now
		p.mu.Unlock()
	}

	// Interface counters
	p.pollInterfaces(ctx, c, d, now, &ds)

	// Vendor metrics
	if prof.CPU != nil {
		if v, ok := fetchMetric(c, prof.CPU); ok {
			ds.CPU = v
			p.db.Append("cpu_pct|"+d.ID, now.Unix(), v)
		}
	}
	if prof.MemUsed != nil {
		used, ok1 := fetchMetric(c, prof.MemUsed)
		if prof.MemIsPct {
			if ok1 {
				ds.MemPct = used
			}
		} else if prof.MemFree != nil {
			free, ok2 := fetchMetric(c, prof.MemFree)
			if ok1 && ok2 && used+free > 0 {
				ds.MemPct = used * 100 / (used + free)
			}
		}
	}
	if ds.MemPct < 0 {
		// vendor OID missing or unsupported on this model: HOST-RESOURCES fallback
		if v, ok := hrMemoryPct(c); ok {
			ds.MemPct = v
		}
	}
	if ds.MemPct >= 0 {
		p.db.Append("mem_pct|"+d.ID, now.Unix(), ds.MemPct)
	}
	if ds.CPU < 0 && prof.CPU == nil {
		if v, ok := hrCPUPct(c); ok {
			ds.CPU = v
			p.db.Append("cpu_pct|"+d.ID, now.Unix(), v)
		}
	}
	if prof.Temp != nil {
		if v, ok := fetchMetric(c, prof.Temp); ok && v > -100 && v < 200 {
			ds.TempC = v
			p.db.Append("temp_c|"+d.ID, now.Unix(), v)
		}
	}
	if prof.Sessions != nil {
		if v, ok := fetchMetric(c, prof.Sessions); ok {
			ds.Sessions = v
			p.db.Append("sessions|"+d.ID, now.Unix(), v)
		}
	}
	p.publishDevice(ds)

	// Slow cycle every 5 minutes: endpoint tables (MAC forwarding + ARP/ND)
	// and routing / layer-2 protocol state
	every := p.EndpointEvery
	if every <= 0 {
		every = 5 * time.Minute
	}
	p.mu.Lock()
	last := p.epAt[d.ID]
	slowDue := last.IsZero() || now.Sub(last) >= every
	if slowDue {
		p.epAt[d.ID] = now
	}
	p.mu.Unlock()
	if slowDue {
		if p.Endpoints != nil {
			p.pollEndpoints(ctx, c, d, now)
		}
		if !p.NoRouting {
			p.pollRouting(ctx, c, d, now)
			p.pollWireless(ctx, c, d, now)
		}
	}
}

func (p *Poller) publishDevice(ds model.DeviceSample) {
	select {
	case p.DeviceSamples <- ds:
	default:
	}
}

func fmtDur(s int64) string {
	d := time.Duration(s) * time.Second
	if d > 48*time.Hour {
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	return d.Round(time.Second).String()
}

// fetchMetric evaluates one profile metric.
func fetchMetric(c *snmp.Client, m *profile.Metric) (float64, bool) {
	scale := m.Scale
	if scale == 0 {
		scale = 1
	}
	if !m.Walk {
		vbs, err := c.Get(m.OID)
		if err != nil || len(vbs) == 0 || !vbs[0].Value.IsNumber() {
			return 0, false
		}
		return vbs[0].Value.Float() * scale, true
	}
	vbs, err := c.Walk(m.OID)
	if err != nil || len(vbs) == 0 {
		return 0, false
	}
	var vals []float64
	for _, vb := range vbs {
		if vb.Value.IsNumber() {
			vals = append(vals, vb.Value.Float()*scale)
		}
	}
	if len(vals) == 0 {
		return 0, false
	}
	switch m.Agg {
	case "max":
		mx := vals[0]
		for _, v := range vals {
			if v > mx {
				mx = v
			}
		}
		return mx, true
	case "sum":
		var s float64
		for _, v := range vals {
			s += v
		}
		return s, true
	case "first":
		return vals[0], true
	default:
		var s float64
		for _, v := range vals {
			s += v
		}
		return s / float64(len(vals)), true
	}
}

// hrMemoryPct derives RAM usage from HOST-RESOURCES when no vendor OID exists.
func hrMemoryPct(c *snmp.Client) (float64, bool) {
	types, err := c.Walk(profile.OIDHrStorageType)
	if err != nil {
		return 0, false
	}
	idx := ""
	for _, vb := range types {
		if vb.Value.Kind == snmp.KindOID && vb.Value.OID == profile.OIDHrStorageRAM {
			idx = snmp.OIDSuffix(vb.OID, profile.OIDHrStorageType)
			break
		}
	}
	if idx == "" {
		return 0, false
	}
	vbs, err := c.Get(profile.OIDHrStorageSize+"."+idx, profile.OIDHrStorageUsed+"."+idx)
	if err != nil || len(vbs) != 2 || vbs[0].Value.Float() <= 0 {
		return 0, false
	}
	return vbs[1].Value.Float() * 100 / vbs[0].Value.Float(), true
}

// inventory refreshes identity and the interface table. Returns the profile.
func (p *Poller) inventory(ctx context.Context, c *snmp.Client, d *model.Device, sysName string) (profile.Profile, bool) {
	vbs, err := c.Get(profile.OIDSysDescr, profile.OIDSysObjectID, profile.OIDSysLocation)
	if err != nil {
		return profile.Profile{}, false
	}
	var descr, oid, loc string
	for _, vb := range vbs {
		switch vb.OID {
		case profile.OIDSysDescr:
			descr = snmp.PrintableOrHex(vb.Value.Bytes)
		case profile.OIDSysObjectID:
			oid = vb.Value.OID
		case profile.OIDSysLocation:
			loc = snmp.PrintableOrHex(vb.Value.Bytes)
		}
	}
	prof := p.lib.Match(oid, descr)
	serial, modelName := entity(c)
	var bridge string
	if vb, err := c.Get(profile.OIDDot1dBaseBridge); err == nil && len(vb) == 1 && len(vb[0].Value.Bytes) == 6 {
		bridge = snmp.MACString(vb[0].Value.Bytes)
	}
	p.st.UpdateDevice(d.ID, func(dev *model.Device) {
		dev.SysDescr = descr
		dev.SysObjectID = oid
		dev.Location = loc
		if sysName != "" && (dev.Name == "" || dev.Name == dev.IP) {
			dev.Name = sysName
		}
		if serial != "" {
			dev.Serial = serial
		}
		if modelName != "" {
			dev.Model = modelName
		}
		if bridge != "" {
			dev.ChassisMAC = bridge
		}
		dev.ProfileID = prof.ID
		if prof.Vendor != "" {
			dev.Vendor = prof.Vendor
		}
		if !dev.RoleLocked && prof.Role != "" && (dev.Role == "" || dev.Role == model.RoleOther) {
			dev.Role = prof.Role
		}
		if prof.Domain != "" && dev.Domain == "" {
			dev.Domain = prof.Domain
		}
		if dev.Domain == "" {
			dev.Domain = model.DomainNetwork
		}
		dev.OSVersion = osVersion(descr)
		*d = *dev
	})
	p.interfaceTable(ctx, c, *d)
	return prof, true
}

func osVersion(descr string) string {
	for _, key := range []string{"Version ", "version ", "v"} {
		if i := strings.Index(descr, key); i >= 0 {
			rest := descr[i+len(key):]
			end := strings.IndexAny(rest, ", \n")
			if end < 0 {
				end = len(rest)
			}
			v := rest[:end]
			if len(v) >= 3 && len(v) <= 30 && (v[0] >= '0' && v[0] <= '9') {
				return v
			}
		}
	}
	return ""
}

func entity(c *snmp.Client) (serial, model string) {
	classes, err := c.Walk(profile.OIDEntPhysicalClass)
	if err != nil {
		return "", ""
	}
	for _, vb := range classes {
		if vb.Value.Int == 3 { // chassis
			idx := snmp.OIDSuffix(vb.OID, profile.OIDEntPhysicalClass)
			vbs, err := c.Get(profile.OIDEntPhysicalSerial+"."+idx, profile.OIDEntPhysicalModel+"."+idx)
			if err == nil && len(vbs) == 2 {
				return strings.TrimSpace(vbs[0].Value.String()), strings.TrimSpace(vbs[1].Value.String())
			}
			return "", ""
		}
	}
	return "", ""
}

// interfaceTable walks the inventory columns and stores interfaces.
func (p *Poller) interfaceTable(ctx context.Context, c *snmp.Client, d model.Device) {
	names, err := c.WalkContext(ctx, profile.OIDIfName)
	if err != nil || len(names) == 0 {
		names, err = c.WalkContext(ctx, profile.OIDIfDescr)
		if err != nil {
			return
		}
	}
	col := func(oid string) map[string]snmp.Value {
		m := map[string]snmp.Value{}
		vbs, err := c.WalkContext(ctx, oid)
		if err != nil {
			return m
		}
		for _, vb := range vbs {
			m[snmp.OIDSuffix(vb.OID, oid)] = vb.Value
		}
		return m
	}
	aliases := col(profile.OIDIfAlias)
	speeds := col(profile.OIDIfHighSpeed)
	types := col(profile.OIDIfType)
	macs := col(profile.OIDIfPhysAddress)
	admin := col(profile.OIDIfAdminStatus)
	oper := col(profile.OIDIfOperStatus)
	var list []model.Interface
	for _, vb := range names {
		idxS := snmp.OIDSuffix(vb.OID, profile.OIDIfName)
		if !strings.HasPrefix(vb.OID, profile.OIDIfName) {
			idxS = snmp.OIDSuffix(vb.OID, profile.OIDIfDescr)
		}
		idx, err := strconv.Atoi(idxS)
		if err != nil {
			continue
		}
		name := snmp.PrintableOrHex(vb.Value.Bytes)
		t := int(types[idxS].Int)
		kind := kindOf(t, name)
		if kind == "skip" {
			continue
		}
		i := model.Interface{ID: model.IfID(d.ID, idx), DeviceID: d.ID, Index: idx, Name: name, Kind: kind,
			Alias: snmp.PrintableOrHex(aliases[idxS].Bytes), SpeedMbps: speeds[idxS].Int, MAC: snmp.MACString(macs[idxS].Bytes),
			AdminUp: admin[idxS].Int == 1, OperUp: oper[idxS].Int == 1, Status: model.StatusUnknown}
		i.Important = kind == "lag" || importantAlias(i.Alias) || importantName(name)
		list = append(list, i)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Index < list[b].Index })
	p.st.PutInterfaces(d.ID, list)
}

func kindOf(ifType int, name string) string {
	switch ifType {
	case 6, 117: // ethernetCsmacd, gigabitEthernet
		return "phys"
	case 161: // ieee8023adLag
		return "lag"
	case 53, 135, 136: // propVirtual, l2vlan, l3ipvlan
		return "vlan"
	case 131, 150: // tunnel, mplsTunnel
		return "tunnel"
	case 24: // softwareLoopback
		return "loopback"
	case 1: // other
		if strings.HasPrefix(strings.ToLower(name), "null") || strings.HasPrefix(strings.ToLower(name), "stack") {
			return "skip"
		}
		return "other"
	case 0:
		return "phys"
	}
	return "other"
}

func importantAlias(alias string) bool {
	a := strings.ToLower(alias)
	for _, k := range []string{"uplink", "trunk", "core", "wan", "dist", "isp", "backbone", "to-", "link to", "-> "} {
		if strings.Contains(a, k) {
			return true
		}
	}
	return false
}

func importantName(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"port-channel", "po", "ae", "bundle-ether", "lag", "te", "ten", "twe", "hu", "fo", "et-", "xe-", "sfp"} {
		if strings.HasPrefix(n, k) && len(n) > len(k) && (n[len(k)] >= '0' && n[len(k)] <= '9') {
			return true
		}
	}
	return false
}

// pollInterfaces reads counters and publishes interface samples.
func (p *Poller) pollInterfaces(ctx context.Context, c *snmp.Client, d model.Device, now time.Time, ds *model.DeviceSample) {
	ifs := p.st.Interfaces(d.ID)
	if len(ifs) == 0 {
		return
	}
	col := func(oid string) (map[string]snmp.Value, bool) {
		m := map[string]snmp.Value{}
		vbs, err := c.WalkContext(ctx, oid)
		if err != nil {
			return m, false
		}
		for _, vb := range vbs {
			m[snmp.OIDSuffix(vb.OID, oid)] = vb.Value
		}
		return m, len(m) > 0
	}
	inOct, hc := col(profile.OIDIfHCInOctets)
	var outOct map[string]snmp.Value
	if hc {
		outOct, _ = col(profile.OIDIfHCOutOctets)
	} else {
		inOct, _ = col(profile.OIDIfInOctets)
		outOct, _ = col(profile.OIDIfOutOctets)
	}
	inErr, _ := col(profile.OIDIfInErrors)
	outErr, _ := col(profile.OIDIfOutErrors)
	// packets and discards feed the hover/health summary: unicast packet rate
	// in each direction and the drop rate (ifInDiscards/ifOutDiscards).
	inPkt, hcPkt := col(profile.OIDIfHCInUcastPkts)
	var outPkt map[string]snmp.Value
	if hcPkt {
		outPkt, _ = col(profile.OIDIfHCOutUcastPkts)
	} else {
		inPkt, _ = col(profile.OIDIfInUcastPkts)
		outPkt, _ = col(profile.OIDIfOutUcastPkts)
	}
	inDrp, _ := col(profile.OIDIfInDiscards)
	outDrp, _ := col(profile.OIDIfOutDiscards)
	oper, _ := col(profile.OIDIfOperStatus)
	admin, _ := col(profile.OIDIfAdminStatus)

	for _, i := range ifs {
		idx := strconv.Itoa(i.Index)
		s := model.InterfaceSample{IfID: i.ID, DeviceID: d.ID, Name: i.Name, TS: now, Important: i.Important, SpeedMbps: i.SpeedMbps,
			OperUp: oper[idx].Int == 1, AdminUp: admin[idx].Int == 1}
		if _, ok := oper[idx]; !ok {
			s.OperUp, s.AdminUp = i.OperUp, i.AdminUp
		}
		cur := &ifCounters{ts: now, inOct: uint64(inOct[idx].Uint), outOct: uint64(outOct[idx].Uint), inErr: uint64(inErr[idx].Int), outErr: uint64(outErr[idx].Int), hc: hc, uptime: ds.Uptime,
			inDrp: uint64(inDrp[idx].Int), outDrp: uint64(outDrp[idx].Int), hcPkt: hcPkt}
		if !hc {
			cur.inOct, cur.outOct = uint64(inOct[idx].Int), uint64(outOct[idx].Int)
		}
		if hcPkt {
			cur.inPkt, cur.outPkt = uint64(inPkt[idx].Uint), uint64(outPkt[idx].Uint)
		} else {
			cur.inPkt, cur.outPkt = uint64(inPkt[idx].Int), uint64(outPkt[idx].Int)
		}
		p.ifRates(i, &s, cur, now, ds.Rebooted)
		select {
		case p.InterfaceSamples <- s:
		default:
		}
	}
}

// ifRates turns two counter snapshots into rates on the sample and stores the
// history points (every cycle for important interfaces, every 5 minutes for the
// rest). Shared by the SNMP and gNMI paths.
func (p *Poller) ifRates(i model.Interface, s *model.InterfaceSample, cur *ifCounters, now time.Time, rebooted bool) {
	hc, hcPkt := cur.hc, cur.hcPkt
	p.mu.Lock()
	prev := p.counters[i.ID]
	p.counters[i.ID] = cur
	p.mu.Unlock()
	if prev == nil || rebooted || cur.uptime < prev.uptime {
		return
	}
	dt := now.Sub(prev.ts).Seconds()
	if dt < 1 {
		return
	}
	s.InBps = rate(prev.inOct, cur.inOct, hc, dt) * 8
	s.OutBps = rate(prev.outOct, cur.outOct, hc, dt) * 8
	s.InErrRate = rate(prev.inErr, cur.inErr, false, dt)
	s.OutErrRate = rate(prev.outErr, cur.outErr, false, dt)
	s.InPps = rate(prev.inPkt, cur.inPkt, hcPkt, dt)
	s.OutPps = rate(prev.outPkt, cur.outPkt, hcPkt, dt)
	s.InDropRate = rate(prev.inDrp, cur.inDrp, false, dt)
	s.OutDropRate = rate(prev.outDrp, cur.outDrp, false, dt)
	s.HaveRates = true
	if i.SpeedMbps > 0 {
		s.InUtil = s.InBps * 100 / (float64(i.SpeedMbps) * 1e6)
		s.OutUtil = s.OutBps * 100 / (float64(i.SpeedMbps) * 1e6)
	}
	// important interfaces (uplinks, LLDP peers, starred) keep every
	// sample; the rest are stored every 5 minutes — live rates in
	// the console are unaffected, only history is coarser.
	store := i.Important || prev.lastStore.IsZero() || now.Sub(prev.lastStore) >= 5*time.Minute
	if store {
		ts := now.Unix()
		p.db.Append("if_in_bps|"+i.ID, ts, s.InBps)
		p.db.Append("if_out_bps|"+i.ID, ts, s.OutBps)
		if s.InErrRate > 0 || s.OutErrRate > 0 {
			p.db.Append("if_in_err|"+i.ID, ts, s.InErrRate)
			p.db.Append("if_out_err|"+i.ID, ts, s.OutErrRate)
		}
		// drops behave like errors: a series only exists while they happen.
		if s.InDropRate > 0 || s.OutDropRate > 0 {
			p.db.Append("if_in_drop|"+i.ID, ts, s.InDropRate)
			p.db.Append("if_out_drop|"+i.ID, ts, s.OutDropRate)
		}
		// packet rates are kept for important interfaces only, so the
		// documented per-port storage cost is unchanged for the rest.
		if i.Important {
			p.db.Append("if_in_pps|"+i.ID, ts, s.InPps)
			p.db.Append("if_out_pps|"+i.ID, ts, s.OutPps)
		}
	}
	p.mu.Lock()
	if c := p.counters[i.ID]; c != nil {
		if store {
			c.lastStore = now
		} else {
			c.lastStore = prev.lastStore
		}
	}
	p.mu.Unlock()
}

func rate(prev, cur uint64, hc bool, dt float64) float64 {
	if cur >= prev {
		return float64(cur-prev) / dt
	}
	// wrap
	if hc {
		return 0 // 64-bit wrap in one cycle is a reset, not a wrap
	}
	return float64(math.MaxUint32-prev+cur+1) / dt
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// hrCPUPct averages HOST-RESOURCES hrProcessorLoad when the profile has no
// vendor CPU OID (net-snmp hosts, many small appliances).
func hrCPUPct(c *snmp.Client) (float64, bool) {
	vbs, err := c.Walk(profile.OIDHrProcessorLoad)
	if err != nil || len(vbs) == 0 {
		return 0, false
	}
	sum := 0.0
	n := 0
	for _, vb := range vbs {
		if vb.Value.Kind == snmp.KindInteger || vb.Value.Kind == snmp.KindGauge32 || vb.Value.Kind == snmp.KindCounter32 {
			sum += vb.Value.Float()
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}
