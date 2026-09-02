// Package topology collects LLDP/CDP neighbour tables, resolves them against
// the inventory, synthesises links with a confidence score, infers device
// roles, and lays the graph out for the map.
package topology

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/poller"
	"github.com/nizartuanku/topolight/internal/profile"
	"github.com/nizartuanku/topolight/internal/snmp"
	"github.com/nizartuanku/topolight/internal/store"
)

// Builder owns topology collection and synthesis.
type Builder struct {
	st     *store.Store
	lib    *profile.Library
	poller *poller.Poller
	Events chan model.Event
	// OnNewNeighbor is called with a management IP seen in CDP for a device
	// that is not in the inventory (discovery can follow it).
	OnNewNeighbor func(ip, name, siteID string)

	mu      sync.Mutex
	running bool
	LastRun time.Time
}

// New creates a builder.
func New(st *store.Store, lib *profile.Library, p *poller.Poller) *Builder {
	return &Builder{st: st, lib: lib, poller: p, Events: make(chan model.Event, 1024)}
}

// Collect walks neighbour tables of every monitored device, then rebuilds.
func (b *Builder) Collect(ctx context.Context) {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return
	}
	b.running = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.running = false
		b.LastRun = time.Now()
		b.mu.Unlock()
	}()
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for _, d := range b.st.Devices() {
		if !d.Monitored || !d.SNMPOK {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(d model.Device) {
			defer wg.Done()
			defer func() { <-sem }()
			b.collectDevice(ctx, d)
		}(d)
	}
	wg.Wait()
	b.Rebuild()
}

// CollectDevice refreshes one device's neighbours (after link events).
func (b *Builder) CollectDevice(ctx context.Context, d model.Device) {
	b.collectDevice(ctx, d)
}

func (b *Builder) collectDevice(ctx context.Context, d model.Device) {
	c, err := b.poller.ClientFor(d)
	if err != nil {
		return
	}
	prof := b.lib.Match(d.SysObjectID, d.SysDescr)
	now := time.Now()
	var obs []model.NeighborObs
	if prof.LLDP {
		obs = append(obs, lldp(ctx, c, d, now)...)
	}
	if prof.CDP {
		obs = append(obs, cdp(ctx, c, d, now, b.st)...)
	}
	// LLDP local chassis id gives the device's chassis MAC when bridge MIB did not.
	if d.ChassisMAC == "" {
		if vb, err := c.Get(profile.OIDLldpLocChassisID); err == nil && len(vb) == 1 && len(vb[0].Value.Bytes) == 6 {
			mac := snmp.MACString(vb[0].Value.Bytes)
			b.st.UpdateDevice(d.ID, func(dev *model.Device) { dev.ChassisMAC = mac })
		}
	}
	if len(obs) > 0 || !prof.LLDP {
		b.st.SetNeighbors(d.ID, obs)
	} else if err == nil {
		// A device that speaks LLDP but reports no neighbours: keep last known
		// observations for one cycle to survive transient walk failures.
		old := b.st.Neighbors(d.ID)
		if len(old) > 0 && now.Sub(old[0].SeenAt) > 2*time.Hour {
			b.st.SetNeighbors(d.ID, nil)
		}
	}
}

func walkMap(ctx context.Context, c *snmp.Client, oid string) map[string]snmp.Value {
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

func lldp(ctx context.Context, c *snmp.Client, d model.Device, now time.Time) []model.NeighborObs {
	remSys := walkMap(ctx, c, profile.OIDLldpRemSysName)
	if len(remSys) == 0 {
		return nil
	}
	remChassis := walkMap(ctx, c, profile.OIDLldpRemChassisID)
	remChassisSub := walkMap(ctx, c, profile.OIDLldpRemChassisSub)
	remPort := walkMap(ctx, c, profile.OIDLldpRemPortID)
	remPortSub := walkMap(ctx, c, profile.OIDLldpRemPortSub)
	remPortDesc := walkMap(ctx, c, profile.OIDLldpRemPortDesc)
	locPort := walkMap(ctx, c, profile.OIDLldpLocPortID)
	locPortDesc := walkMap(ctx, c, profile.OIDLldpLocPortDesc)
	var out []model.NeighborObs
	for idx, name := range remSys {
		// index = timeMark.localPortNum.remIndex
		parts := strings.Split(idx, ".")
		if len(parts) != 3 {
			continue
		}
		localNum := parts[1]
		local := snmp.PrintableOrHex(locPort[localNum].Bytes)
		if local == "" || looksLikeMAC(local) {
			if dsc := snmp.PrintableOrHex(locPortDesc[localNum].Bytes); dsc != "" {
				local = dsc
			}
		}
		if local == "" {
			// fall back to ifindex == localPortNum (true on most switches)
			local = "ifindex:" + localNum
		}
		o := model.NeighborObs{DeviceID: d.ID, LocalIf: local, Source: "lldp", RemoteName: snmp.PrintableOrHex(name.Bytes), SeenAt: now, Confidence: 0.8}
		ch := remChassis[idx].Bytes
		switch remChassisSub[idx].Int {
		case 4: // macAddress
			o.RemoteMAC = snmp.MACString(ch)
		case 5: // networkAddress: first byte is address family
			if len(ch) == 5 && ch[0] == 1 {
				o.RemoteIP = net.IPv4(ch[1], ch[2], ch[3], ch[4]).String()
			}
		default:
			if len(ch) == 6 {
				o.RemoteMAC = snmp.MACString(ch)
			}
		}
		pb := remPort[idx].Bytes
		switch remPortSub[idx].Int {
		case 3: // macAddress
			o.RemotePort = snmp.MACString(pb)
		default:
			o.RemotePort = snmp.PrintableOrHex(pb)
		}
		o.RemoteDesc = snmp.PrintableOrHex(remPortDesc[idx].Bytes)
		if o.RemotePort == "" || looksLikeMAC(o.RemotePort) {
			if o.RemoteDesc != "" && !looksLikeMAC(o.RemoteDesc) {
				o.RemotePort = o.RemoteDesc
			}
		}
		out = append(out, o)
	}
	return out
}

func cdp(ctx context.Context, c *snmp.Client, d model.Device, now time.Time, st *store.Store) []model.NeighborObs {
	ids := walkMap(ctx, c, profile.OIDCdpCacheDeviceID)
	if len(ids) == 0 {
		return nil
	}
	addrs := walkMap(ctx, c, profile.OIDCdpCacheAddress)
	ports := walkMap(ctx, c, profile.OIDCdpCachePort)
	plats := walkMap(ctx, c, profile.OIDCdpCachePlatform)
	var out []model.NeighborObs
	for idx, id := range ids {
		parts := strings.Split(idx, ".")
		if len(parts) != 2 {
			continue
		}
		ifidx, _ := strconv.Atoi(parts[0])
		local := "ifindex:" + parts[0]
		if i, err := st.Interface(model.IfID(d.ID, ifidx)); err == nil {
			local = i.Name
		}
		o := model.NeighborObs{DeviceID: d.ID, LocalIf: local, Source: "cdp", RemoteName: snmp.PrintableOrHex(id.Bytes), RemotePort: snmp.PrintableOrHex(ports[idx].Bytes),
			RemoteDesc: snmp.PrintableOrHex(plats[idx].Bytes), SeenAt: now, Confidence: 0.8}
		if a := addrs[idx].Bytes; len(a) == 4 {
			o.RemoteIP = net.IPv4(a[0], a[1], a[2], a[3]).String()
		}
		out = append(out, o)
	}
	return out
}

var macRe = regexp.MustCompile(`^([0-9a-f]{2}[:\-]){5}[0-9a-f]{2}$|^[0-9a-f]{4}\.[0-9a-f]{4}\.[0-9a-f]{4}$`)

func looksLikeMAC(s string) bool { return macRe.MatchString(strings.ToLower(strings.TrimSpace(s))) }

// NormalizePort turns vendor long names into the short ifName form used in
// the inventory (GigabitEthernet1/0/1 -> Gi1/0/1) and lower-cases.
func NormalizePort(s string) string {
	s = strings.TrimSpace(s)
	repl := []struct{ long, short string }{
		{"HundredGigE", "Hu"}, {"HundredGigabitEthernet", "Hu"}, {"FortyGigabitEthernet", "Fo"}, {"TwentyFiveGigE", "Twe"},
		{"TenGigabitEthernet", "Te"}, {"TenGigE", "Te"}, {"GigabitEthernet", "Gi"}, {"FastEthernet", "Fa"}, {"Port-channel", "Po"},
		{"Bundle-Ether", "BE"}, {"Ethernet", "Eth"}, {"management", "mgmt"}, {"Management", "mgmt"},
	}
	for _, r := range repl {
		if strings.HasPrefix(s, r.long) {
			s = r.short + s[len(r.long):]
			break
		}
	}
	return strings.ToLower(s)
}

// ---- synthesis ----

type devIndex struct {
	byID    map[string]model.Device
	byMAC   map[string]string
	byIP    map[string]string
	byName  map[string]string
	ifByDev map[string][]model.Interface
}

func (b *Builder) index() *devIndex {
	ix := &devIndex{byID: map[string]model.Device{}, byMAC: map[string]string{}, byIP: map[string]string{}, byName: map[string]string{}, ifByDev: map[string][]model.Interface{}}
	for _, d := range b.st.Devices() {
		ix.byID[d.ID] = d
		if d.ChassisMAC != "" {
			ix.byMAC[strings.ToLower(d.ChassisMAC)] = d.ID
		}
		ix.byIP[d.IP] = d.ID
		ix.byName[shortName(d.Name)] = d.ID
	}
	for _, i := range b.st.Interfaces("") {
		ix.ifByDev[i.DeviceID] = append(ix.ifByDev[i.DeviceID], i)
		if i.MAC != "" {
			if _, taken := ix.byMAC[strings.ToLower(i.MAC)]; !taken {
				ix.byMAC[strings.ToLower(i.MAC)] = i.DeviceID
			}
		}
	}
	return ix
}

func shortName(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	if i := strings.IndexByte(n, '.'); i > 0 {
		n = n[:i]
	}
	return n
}

func (ix *devIndex) resolve(o model.NeighborObs) (string, bool) {
	if o.RemoteMAC != "" {
		if id, ok := ix.byMAC[strings.ToLower(o.RemoteMAC)]; ok {
			return id, true
		}
	}
	if o.RemoteIP != "" {
		if id, ok := ix.byIP[o.RemoteIP]; ok {
			return id, true
		}
	}
	if o.RemoteName != "" {
		if id, ok := ix.byName[shortName(o.RemoteName)]; ok {
			return id, true
		}
		// CDP device ids often carry a serial in parentheses: "SW1(FOC1234)"
		if i := strings.IndexByte(o.RemoteName, '('); i > 0 {
			if id, ok := ix.byName[shortName(o.RemoteName[:i])]; ok {
				return id, true
			}
		}
	}
	return "", false
}

// ifName resolves a local/remote port reference to the inventory ifName.
func (ix *devIndex) ifName(devID, ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "ifindex:") {
		n, _ := strconv.Atoi(strings.TrimPrefix(ref, "ifindex:"))
		for _, i := range ix.ifByDev[devID] {
			if i.Index == n {
				return i.Name
			}
		}
		return ref
	}
	norm := NormalizePort(ref)
	for _, i := range ix.ifByDev[devID] {
		if NormalizePort(i.Name) == norm {
			return i.Name
		}
	}
	if looksLikeMAC(ref) {
		for _, i := range ix.ifByDev[devID] {
			if strings.EqualFold(i.MAC, ref) {
				return i.Name
			}
		}
	}
	return ref
}

type cand struct {
	a, b     string // device ids (a < b)
	aIf, bIf string
	conf     float64
	sources  map[string]bool
	seenA    bool
	seenB    bool
	external bool
	extName  string
	seenAt   time.Time
}

func linkID(a, aIf, b, bIf string) string {
	h := sha1.Sum([]byte(a + "|" + strings.ToLower(aIf) + "|" + b + "|" + strings.ToLower(bIf)))
	return "lnk_" + hex.EncodeToString(h[:6])
}

// Rebuild synthesises links from stored observations, updates roles and
// layout, and emits topology events.
func (b *Builder) Rebuild() {
	ix := b.index()
	all := b.st.AllNeighbors()
	cands := map[string]*cand{}
	now := time.Now()
	for devID, obs := range all {
		for _, o := range obs {
			localIf := ix.ifName(devID, o.LocalIf)
			remID, ok := ix.resolve(o)
			if !ok {
				// external node keyed by name
				name := o.RemoteName
				if name == "" {
					name = o.RemoteMAC
				}
				if name == "" {
					continue
				}
				key := "ext:" + shortName(name)
				a, aIf, bb, bIf := devID, localIf, key, o.RemotePort
				id := linkID(a, aIf, bb, bIf)
				c := cands[id]
				if c == nil {
					c = &cand{a: a, aIf: aIf, b: bb, bIf: bIf, sources: map[string]bool{}, external: true, extName: name}
					cands[id] = c
				}
				c.sources[o.Source] = true
				c.conf = 0.6
				c.seenAt = o.SeenAt
				if o.RemoteIP != "" && b.OnNewNeighbor != nil {
					if d, ok := ix.byID[devID]; ok {
						b.OnNewNeighbor(o.RemoteIP, o.RemoteName, d.SiteID)
					}
				}
				continue
			}
			if remID == devID {
				continue
			}
			remIf := ix.ifName(remID, o.RemotePort)
			a, aIf, bb, bIf := devID, localIf, remID, remIf
			seenA := true
			if a > bb {
				a, aIf, bb, bIf = bb, bIf, a, aIf
				seenA = false
			}
			id := linkID(a, aIf, bb, bIf)
			c := cands[id]
			if c == nil {
				c = &cand{a: a, aIf: aIf, b: bb, bIf: bIf, sources: map[string]bool{}}
				cands[id] = c
			}
			c.sources[o.Source] = true
			if seenA {
				c.seenA = true
			} else {
				c.seenB = true
			}
			if o.SeenAt.After(c.seenAt) {
				c.seenAt = o.SeenAt
			}
		}
	}
	// Merge one-directional pairs that describe the same physical link but
	// were keyed with different port spellings.
	old := map[string]model.Link{}
	for _, l := range b.st.Links() {
		old[l.ID] = l
	}
	var links []model.Link
	for id, c := range cands {
		conf := 0.8
		if c.seenA && c.seenB {
			conf = 1.0
		}
		if c.external {
			conf = c.conf
		}
		if len(c.sources) > 1 {
			conf = math.Min(1, conf+0.05)
		}
		var srcs []string
		for s := range c.sources {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		l := model.Link{ID: id, ADevice: c.a, AIf: c.aIf, BDevice: c.b, BIf: c.bIf, Layer: "L2", Confidence: conf, Sources: srcs,
			Status: model.StatusUp, FirstSeen: now, LastSeen: c.seenAt, External: c.external, ExternalName: c.extName}
		if o, ok := old[id]; ok {
			l.FirstSeen = o.FirstSeen
			l.Status = o.Status
		}
		l.SpeedMbps, l.Util = b.linkSpeedUtil(ix, l)
		links = append(links, l)
	}
	// Age: links seen before but absent now become stale for 7 days, then drop.
	for id, o := range old {
		if _, still := cands[id]; still || o.Manual {
			if o.Manual {
				links = append(links, o)
			}
			continue
		}
		if now.Sub(o.LastSeen) < 7*24*time.Hour {
			wasStale := o.Stale
			o.Stale = true
			links = append(links, o)
			if !wasStale {
				b.emit(model.Event{Kind: "link_lost", DeviceID: o.ADevice, Object: o.ID, Source: "topology", Severity: model.SevInfo,
					Message: fmt.Sprintf("Link no longer observed: %s %s ↔ %s %s", ix.name(o.ADevice), o.AIf, ix.name(o.BDevice), o.BIf), DedupKey: "link_lost:" + o.ID})
			}
		}
	}
	// A physical port has one peer: when a port carries a confirmed (both
	// sides seen) link, drop stale or weaker links on the same port — they
	// were mis-resolutions from before the peer was inventoried.
	best := map[string]float64{}
	portKey := func(dev, ifn string) string { return dev + "|" + strings.ToLower(ifn) }
	for _, l := range links {
		if l.Stale || l.External {
			continue
		}
		for _, k := range []string{portKey(l.ADevice, l.AIf), portKey(l.BDevice, l.BIf)} {
			if l.Confidence > best[k] {
				best[k] = l.Confidence
			}
		}
	}
	kept := links[:0]
	for _, l := range links {
		if !l.Manual && !l.External {
			ka, kb := portKey(l.ADevice, l.AIf), portKey(l.BDevice, l.BIf)
			if (best[ka] >= 1 && (l.Stale || l.Confidence < best[ka])) || (best[kb] >= 1 && (l.Stale || l.Confidence < best[kb])) {
				continue
			}
		}
		kept = append(kept, l)
	}
	links = kept
	for _, l := range links {
		if _, existed := old[l.ID]; !existed && !l.Manual {
			b.emit(model.Event{Kind: "link_new", DeviceID: l.ADevice, Object: l.ID, Source: "topology", Severity: model.SevInfo,
				Message: fmt.Sprintf("New link: %s %s ↔ %s %s (confidence %.2f)", ix.name(l.ADevice), l.AIf, ix.nameOrExt(l), l.BIf, l.Confidence), DedupKey: "link_new:" + l.ID})
		}
	}
	b.st.ReplaceLinks(links)
	b.markImportant(ix, links)
	b.inferRoles(ix, links)
	b.Layout()
}

func (ix *devIndex) name(id string) string {
	if d, ok := ix.byID[id]; ok {
		return d.Name
	}
	return id
}

func (ix *devIndex) nameOrExt(l model.Link) string {
	if l.External {
		return l.ExternalName
	}
	return ix.name(l.BDevice)
}

func (b *Builder) emit(e model.Event) {
	e.TS = time.Now()
	select {
	case b.Events <- e:
	default:
	}
}

func (b *Builder) linkSpeedUtil(ix *devIndex, l model.Link) (int64, float64) {
	var speed int64
	var util float64
	for _, side := range []struct{ dev, ifn string }{{l.ADevice, l.AIf}, {l.BDevice, l.BIf}} {
		for _, i := range ix.ifByDev[side.dev] {
			if strings.EqualFold(i.Name, side.ifn) {
				if i.SpeedMbps > speed {
					speed = i.SpeedMbps
				}
				util = math.Max(util, math.Max(i.InUtil, i.OutUtil))
			}
		}
	}
	return speed, util
}

// markImportant flags interfaces that carry a link to another infrastructure
// device (switch/router/firewall) so their state changes are alerted.
func (b *Builder) markImportant(ix *devIndex, links []model.Link) {
	infra := func(id string) bool {
		d, ok := ix.byID[id]
		if !ok {
			return false
		}
		switch d.Role {
		case model.RoleCore, model.RoleDist, model.RoleAccess, model.RoleRouter, model.RoleFirewall:
			return true
		}
		return false
	}
	for _, l := range links {
		if l.External || l.Stale {
			continue
		}
		if infra(l.ADevice) && infra(l.BDevice) {
			for _, side := range []struct{ dev, ifn string }{{l.ADevice, l.AIf}, {l.BDevice, l.BIf}} {
				if i, ok := b.st.InterfaceByName(side.dev, side.ifn); ok && !i.Important {
					b.st.UpdateInterface(i.ID, func(x *model.Interface) { x.Important = true })
				}
			}
		}
	}
}

// inferRoles promotes switches by connectivity: the best-connected switch per
// site becomes core; switches linking core to >=2 others become distribution.
// inferRoles classifies switches by their place in the graph: the best
// connected switches are the core (a redundant pair counts as two), switches
// hanging off the core with their own downstream switches are distribution,
// and everything else is access. Routers, firewalls, APs and servers keep the
// role their profile gave them. A role edited by hand (RoleLocked) is kept.
func (b *Builder) inferRoles(ix *devIndex, links []model.Link) {
	nbrs := map[string]map[string]bool{}
	add := func(a, b string) {
		if nbrs[a] == nil {
			nbrs[a] = map[string]bool{}
		}
		nbrs[a][b] = true
	}
	for _, l := range links {
		if l.External || l.Stale {
			continue
		}
		add(l.ADevice, l.BDevice)
		add(l.BDevice, l.ADevice)
	}
	bySite := map[string][]model.Device{}
	for _, d := range ix.byID {
		bySite[d.SiteID] = append(bySite[d.SiteID], d)
	}
	isSwitch := func(d model.Device) bool {
		return d.Role == model.RoleAccess || d.Role == model.RoleDist || d.Role == model.RoleCore
	}
	isEdge := func(id string) bool {
		d, ok := ix.byID[id]
		return ok && (d.Role == model.RoleRouter || d.Role == model.RoleFirewall)
	}
	for _, devs := range bySite {
		deg := func(id string) int { return len(nbrs[id]) }
		top := 0
		for _, d := range devs {
			if isSwitch(d) && deg(d.ID) > top {
				top = deg(d.ID)
			}
		}
		core := map[string]bool{}
		if top >= 2 {
			// candidates: within one of the top degree; prefer those touching the edge
			var cand, withEdge []string
			for _, d := range devs {
				if !isSwitch(d) || deg(d.ID) < top-1 || deg(d.ID) < 2 {
					continue
				}
				cand = append(cand, d.ID)
				for nb := range nbrs[d.ID] {
					if isEdge(nb) {
						withEdge = append(withEdge, d.ID)
						break
					}
				}
			}
			pick := cand
			if len(withEdge) > 0 {
				pick = withEdge
			}
			sort.Slice(pick, func(i, j int) bool {
				if deg(pick[i]) != deg(pick[j]) {
					return deg(pick[i]) > deg(pick[j])
				}
				return ix.byID[pick[i]].Name < ix.byID[pick[j]].Name
			})
			if len(pick) > 2 {
				pick = pick[:2]
			}
			for _, id := range pick {
				core[id] = true
			}
		}
		for _, d := range devs {
			if !isSwitch(d) || d.RoleLocked {
				continue
			}
			role := model.RoleAccess
			switch {
			case core[d.ID]:
				role = model.RoleCore
			default:
				touchesCore, down := false, 0
				for nb := range nbrs[d.ID] {
					if core[nb] {
						touchesCore = true
					} else if x, ok := ix.byID[nb]; ok && isSwitch(x) {
						down++
					}
				}
				if touchesCore && down > 0 {
					role = model.RoleDist
				}
			}
			if d.Role != role {
				b.st.UpdateDevice(d.ID, func(x *model.Device) { x.Role = role })
				ix.byID[d.ID] = func() model.Device { x := d; x.Role = role; return x }()
			}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Layout computes deterministic positions per site: tiers by role (z), a ring
// per tier (x, y), children placed near their upstream neighbour's angle.
// Layout assigns every device a position on a stacked-disc layout:
// tier 3 = edge (router/firewall), 2 = core, 1 = distribution, 0 = access and
// everything else. Devices on a disc are ordered by the angle of their
// upstream neighbour so links run mostly straight down; siblings are spread
// evenly around the disc so nothing overlaps. Multiple sites are placed side
// by side on a wide circle.
func (b *Builder) Layout() {
	devs := b.st.Devices()
	links := b.st.Links()
	adj := map[string][]string{}
	for _, l := range links {
		if l.External || l.Stale {
			continue
		}
		adj[l.ADevice] = append(adj[l.ADevice], l.BDevice)
		adj[l.BDevice] = append(adj[l.BDevice], l.ADevice)
	}
	// disc radius grows with the number of devices on it (55 units apart) but
	// never shrinks below the inner disc
	minR := map[int]float64{3: 70, 2: 80, 1: 150, 0: 220}
	radiusFor := func(t, n int, inner float64) float64 {
		r := float64(n) * 55 / (2 * math.Pi)
		if r < minR[t] {
			r = minR[t]
		}
		if r < inner+60 && t < 2 {
			r = inner + 60
		}
		return r
	}
	pos := map[string][3]float64{}
	bySite := map[string][]model.Device{}
	for _, d := range devs {
		bySite[d.SiteID] = append(bySite[d.SiteID], d)
	}
	siteIDs := make([]string, 0, len(bySite))
	for id := range bySite {
		siteIDs = append(siteIDs, id)
	}
	sort.Strings(siteIDs)
	for si, sid := range siteIDs {
		ds := bySite[sid]
		var ox, oy float64
		if len(siteIDs) > 1 {
			a := -math.Pi/2 + float64(si)*2*math.Pi/float64(len(siteIDs))
			ox, oy = math.Cos(a)*900, math.Sin(a)*900
		}
		angle := map[string]float64{}
		sort.Slice(ds, func(i, j int) bool { return ds[i].Name < ds[j].Name })
		inner := 0.0
		for t := 3; t >= 0; t-- {
			var ring []model.Device
			for _, d := range ds {
				if layoutTier(d) == t {
					ring = append(ring, d)
				}
			}
			if len(ring) == 0 {
				continue
			}
			n := len(ring)
			if n == 1 && t == 2 {
				pos[ring[0].ID] = [3]float64{ox, oy, 2}
				angle[ring[0].ID] = -math.Pi / 2
				continue
			}
			// desired angle: circular mean of upstream angles (already placed tiers)
			want := make([]float64, n)
			for i, d := range ring {
				if a, ok := upstreamAngle(d.ID, adj, angle); ok {
					want[i] = a
				} else {
					want[i] = -math.Pi/2 + float64(i)*2*math.Pi/float64(n)
				}
			}
			idx := make([]int, n)
			for i := range idx {
				idx[i] = i
			}
			sort.SliceStable(idx, func(i, j int) bool { return want[idx[i]] < want[idx[j]] })
			r := radiusFor(t, n, inner)
			if t < 2 {
				inner = r
			}
			for k, i := range idx {
				d := ring[i]
				var a float64
				if n == 1 {
					a = want[i]
				} else {
					// even spacing preserving the upstream order, rotated so the
					// first element sits near its wanted angle
					a = want[idx[0]] + float64(k)*2*math.Pi/float64(n)
				}
				pos[d.ID] = [3]float64{ox + math.Cos(a)*r, oy + math.Sin(a)*r, float64(t)}
				angle[d.ID] = a
			}
		}
	}
	b.st.SetLayout(pos)
}

func layoutTier(d model.Device) int {
	switch d.Role {
	case model.RoleRouter, model.RoleFirewall:
		return 3
	case model.RoleCore:
		return 2
	case model.RoleDist:
		return 1
	}
	return 0
}

// upstreamAngle returns the circular mean of the angles of already-placed
// neighbours (i.e. devices on higher tiers), if any.
func upstreamAngle(id string, adj map[string][]string, angle map[string]float64) (float64, bool) {
	var sx, sy float64
	n := 0
	for _, nb := range adj[id] {
		if a, ok := angle[nb]; ok {
			sx += math.Cos(a)
			sy += math.Sin(a)
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	if math.Abs(sx) < 1e-9 && math.Abs(sy) < 1e-9 {
		// opposite neighbours cancel out — pick the first placed one
		for _, nb := range adj[id] {
			if a, ok := angle[nb]; ok {
				return a, true
			}
		}
	}
	return math.Atan2(sy, sx), true
}
