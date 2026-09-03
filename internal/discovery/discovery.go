// Package discovery sweeps site subnets, identifies SNMP-capable devices and
// registers them (respecting the licence cap), and follows neighbour hints.
package discovery

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/icmp"
	"github.com/nizartuanku/topolight/internal/license"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/poller"
	"github.com/nizartuanku/topolight/internal/profile"
	"github.com/nizartuanku/topolight/internal/snmp"
	"github.com/nizartuanku/topolight/internal/store"
)

// Discovery runs sweeps.
type Discovery struct {
	st   *store.Store
	lib  *profile.Library
	ping *icmp.Pinger
	Caps func() license.Caps
	// Events receives discovery events (new device, cap reached).
	Events chan model.Event

	mu      sync.Mutex
	running map[string]*Progress
}

// Progress is the live state of one sweep (shown in the wizard).
type Progress struct {
	SiteID    string    `json:"site_id"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished,omitempty"`
	Total     int       `json:"total"`
	Scanned   int       `json:"scanned"`
	Answered  int       `json:"answered"` // ICMP
	Found     int       `json:"found"`    // SNMP devices (new or existing)
	Added     int       `json:"added"`    // new devices registered
	Skipped   int       `json:"skipped"`  // over licence cap
	Errors    []string  `json:"errors,omitempty"`
	Running   bool      `json:"running"`
	LastFound string    `json:"last_found,omitempty"`
}

// New builds the discovery service.
func New(st *store.Store, lib *profile.Library, ping *icmp.Pinger, caps func() license.Caps) *Discovery {
	return &Discovery{st: st, lib: lib, ping: ping, Caps: caps, Events: make(chan model.Event, 1024), running: map[string]*Progress{}}
}

// Progress returns the current/last sweep for a site.
func (d *Discovery) Progress(siteID string) *Progress {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.running[siteID]
	if p == nil {
		return nil
	}
	cp := *p
	cp.Errors = append([]string(nil), p.Errors...)
	return &cp
}

// Sweep scans every subnet of a site. It returns immediately when a sweep is
// already running for the site.
func (d *Discovery) Sweep(ctx context.Context, siteID string) (*Progress, error) {
	site, err := d.st.Site(siteID)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if p := d.running[siteID]; p != nil && p.Running {
		d.mu.Unlock()
		return d.Progress(siteID), nil
	}
	prog := &Progress{SiteID: siteID, Started: time.Now(), Running: true}
	d.running[siteID] = prog
	d.mu.Unlock()

	var ips []string
	for _, cidr := range site.Subnets {
		hosts, err := expandCIDR(cidr, 4096)
		if err != nil {
			d.addErr(prog, fmt.Sprintf("%s: %v", cidr, err))
			continue
		}
		ips = append(ips, hosts...)
	}
	d.mu.Lock()
	prog.Total = len(ips)
	d.mu.Unlock()

	creds := d.credsFor(site)
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	for _, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			d.probe(ctx, site, ip, creds, prog)
		}(ip)
	}
	wg.Wait()
	d.mu.Lock()
	prog.Running = false
	prog.Finished = time.Now()
	d.mu.Unlock()
	return d.Progress(siteID), nil
}

func (d *Discovery) addErr(p *Progress, s string) {
	d.mu.Lock()
	if len(p.Errors) < 50 {
		p.Errors = append(p.Errors, s)
	}
	d.mu.Unlock()
}

func (d *Discovery) credsFor(site model.Site) []model.Credential {
	var all []model.Credential
	for _, c := range d.st.Creds() {
		if c.IsSNMP() {
			all = append(all, c)
		}
	}
	if site.CredID == "" {
		return all
	}
	var out []model.Credential
	for _, c := range all {
		if c.ID == site.CredID {
			out = append(out, c)
		}
	}
	for _, c := range all {
		if c.ID != site.CredID {
			out = append(out, c)
		}
	}
	return out
}

func (d *Discovery) probe(ctx context.Context, site model.Site, ip string, creds []model.Credential, prog *Progress) {
	defer func() {
		d.mu.Lock()
		prog.Scanned++
		d.mu.Unlock()
	}()
	if _, exists := d.st.DeviceByIP(ip); exists {
		d.mu.Lock()
		prog.Found++
		d.mu.Unlock()
		return
	}
	answered := false
	if d.ping != nil {
		r, err := d.ping.Probe(ip, 1, 0, 700*time.Millisecond)
		answered = err == nil && r.Reachable()
		if answered {
			d.mu.Lock()
			prog.Answered++
			d.mu.Unlock()
		}
	}
	// Many firewalls block ICMP but answer SNMP — always try SNMP, but with a
	// short timeout when nothing answered ping.
	for _, cred := range creds {
		c := poller.NewClient(ip, cred)
		c.Timeout = 900 * time.Millisecond
		c.Retries = 0
		if answered {
			c.Timeout = 1500 * time.Millisecond
		}
		vbs, err := c.Get(profile.OIDSysName, profile.OIDSysDescr, profile.OIDSysObjectID)
		c.Close()
		if err != nil || len(vbs) < 3 {
			continue
		}
		name := snmp.PrintableOrHex(vbs[0].Value.Bytes)
		descr := snmp.PrintableOrHex(vbs[1].Value.Bytes)
		oid := vbs[2].Value.OID
		d.Register(site.ID, ip, name, descr, oid, cred.ID, "discovery", prog)
		return
	}
	if answered && site.AddPingOnly {
		d.RegisterPingOnly(site.ID, ip, "", "discovery", prog)
	}
}

// RegisterPingOnly adds a device watched with ICMP only. The name comes from
// reverse DNS when it answers within a second, else the address.
func (d *Discovery) RegisterPingOnly(siteID, ip, name, source string, prog *Progress) (model.Device, bool) {
	if dev, exists := d.st.DeviceByIP(ip); exists {
		return dev, false
	}
	if name == "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if names, err := net.DefaultResolver.LookupAddr(ctx, ip); err == nil && len(names) > 0 {
			name = strings.TrimSuffix(names[0], ".")
		}
		cancel()
	}
	dev, added := d.Register(siteID, ip, name, "", "", "", source, prog)
	if !added {
		return dev, false
	}
	dev.PingOnly = true
	dev.Role = model.RoleServer
	if source == "discovery" {
		dev.Notes = strings.TrimSpace(dev.Notes + " Ping-only: answers ICMP, no SNMP credential worked.")
	}
	d.st.PutDevice(dev)
	return dev, true
}

// Register adds a device (idempotent by IP). It enforces the licence cap: a
// device beyond the cap is stored unmonitored and reported, never dropped
// silently.
func (d *Discovery) Register(siteID, ip, name, descr, sysObjectID, credID, source string, prog *Progress) (model.Device, bool) {
	if dev, exists := d.st.DeviceByIP(ip); exists {
		return dev, false
	}
	prof := d.lib.Match(sysObjectID, descr)
	if name == "" {
		name = ip
	}
	caps := d.Caps()
	_, monitored := d.st.DeviceCount()
	settings := d.st.Settings()
	dev := model.Device{ID: model.NewID("dev"), SiteID: siteID, Name: name, IP: ip, Domain: model.DomainNetwork, Role: prof.Role,
		SysDescr: descr, SysObjectID: sysObjectID, ProfileID: prof.ID, Vendor: prof.Vendor, CredID: credID,
		PollEvery: settings.DefaultPoll, Status: model.StatusUnknown, StatusSince: time.Now(), Created: time.Now(), Monitored: true}
	if prof.Domain != "" {
		dev.Domain = prof.Domain
	}
	if dev.Role == "" {
		dev.Role = model.RoleOther
	}
	if dev.PollEvery <= 0 {
		dev.PollEvery = 60
	}
	if !license.Unlimited(caps.MaxDevices) && monitored >= caps.MaxDevices {
		dev.Monitored = false
		dev.Notes = fmt.Sprintf("Not monitored: %s licence allows %d devices.", caps.Tier.Title(), caps.MaxDevices)
	}
	d.st.PutDevice(dev)
	if prog != nil {
		d.mu.Lock()
		prog.Found++
		if dev.Monitored {
			prog.Added++
		} else {
			prog.Skipped++
		}
		prog.LastFound = dev.Name + " (" + ip + ")"
		d.mu.Unlock()
	}
	ev := model.Event{Kind: "device_discovered", DeviceID: dev.ID, Source: source, Severity: model.SevInfo, Domain: dev.Domain,
		Message: fmt.Sprintf("Discovered %s (%s) — %s", dev.Name, ip, firstLine(descr)), DedupKey: "device_discovered:" + dev.ID}
	if !dev.Monitored {
		ev.Kind = "device_over_cap"
		ev.Severity = model.SevMinor
		ev.Message = fmt.Sprintf("%s (%s) found but not monitored — %s licence limit of %d devices reached", dev.Name, ip, caps.Tier.Title(), caps.MaxDevices)
	}
	select {
	case d.Events <- ev:
	default:
	}
	return dev, true
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// expandCIDR lists host addresses of a CIDR (or a single IP / "a-b" range).
func expandCIDR(cidr string, limit int) ([]string, error) {
	cidr = strings.TrimSpace(cidr)
	if ip := net.ParseIP(cidr); ip != nil {
		return []string{ip.String()}, nil
	}
	if strings.Contains(cidr, "-") && !strings.Contains(cidr, "/") {
		parts := strings.SplitN(cidr, "-", 2)
		a := net.ParseIP(strings.TrimSpace(parts[0])).To4()
		if a == nil {
			return nil, fmt.Errorf("bad range")
		}
		bs := strings.TrimSpace(parts[1])
		var b net.IP
		if strings.Contains(bs, ".") {
			b = net.ParseIP(bs).To4()
		} else {
			b = make(net.IP, 4)
			copy(b, a)
			var last int
			fmt.Sscanf(bs, "%d", &last)
			b[3] = byte(last)
		}
		if b == nil {
			return nil, fmt.Errorf("bad range end")
		}
		var out []string
		for x := ip4int(a); x <= ip4int(b) && len(out) < limit; x++ {
			out = append(out, int2ip4(x).String())
		}
		return out, nil
	}
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	if n.IP.To4() == nil {
		return nil, fmt.Errorf("IPv6 ranges are not supported yet")
	}
	ones, bits := n.Mask.Size()
	size := 1 << uint(bits-ones)
	if size > limit {
		return nil, fmt.Errorf("range has %d addresses; largest allowed is /%d", size, bits-log2(limit))
	}
	start := ip4int(n.IP.To4())
	var out []string
	for i := 0; i < size; i++ {
		if size > 2 && (i == 0 || i == size-1) {
			continue // network & broadcast
		}
		out = append(out, int2ip4(start+uint32(i)).String())
	}
	return out, nil
}

func log2(n int) int {
	r := 0
	for n > 1 {
		n >>= 1
		r++
	}
	return r
}

func ip4int(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func int2ip4(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// ApplyCap re-evaluates which devices are monitored after a licence change:
// oldest-created devices keep their slots so nothing flaps.
func ApplyCap(st *store.Store, caps license.Caps) (monitored, unmonitored int) {
	devs := st.Devices()
	sort.Slice(devs, func(i, j int) bool { return devs[i].Created.Before(devs[j].Created) })
	for _, d := range devs {
		want := license.Unlimited(caps.MaxDevices) || monitored < caps.MaxDevices
		if strings.HasPrefix(d.Notes, "Disabled") {
			want = false
		}
		if want {
			monitored++
		} else {
			unmonitored++
		}
		if d.Monitored != want {
			st.UpdateDevice(d.ID, func(dev *model.Device) {
				dev.Monitored = want
				if want {
					dev.Notes = ""
				} else if !strings.HasPrefix(dev.Notes, "Disabled") {
					dev.Notes = fmt.Sprintf("Not monitored: %s licence allows %d devices.", caps.Tier.Title(), caps.MaxDevices)
				}
			})
		}
	}
	return
}
