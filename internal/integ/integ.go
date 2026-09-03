// Package integ reads wireless controllers and cloud dashboards — UniFi
// (Network application / UDM), Cisco Meraki — and turns what they know into
// TopoLight devices, samples and per-AP wireless state, plus MX uplink
// health for SD-WAN. Controllers reached over SNMP (Cisco WLC, Aruba) are
// handled by the poller; this package is for HTTP APIs.
package integ

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
)

// Kinds lists the supported integration kinds.
var Kinds = []string{"unifi", "meraki"}

// Sink receives what an integration learned.
type Sink interface {
	Append(series string, t int64, v float64)
}

// Runner schedules every enabled integration.
type Runner struct {
	st      *store.Store
	db      Sink
	Samples chan<- model.DeviceSample
	Events  chan model.Event
	Caps    func() (maxDevices int, monitored int)

	mu      sync.Mutex
	next    map[string]time.Time
	running map[string]bool
	clients map[string]*unifiClient
	Runs    int64
	Fails   int64
}

// New builds a runner; samples go to the poller's channel so the engine treats
// controller-reported status like any other poll.
func New(st *store.Store, db Sink, samples chan<- model.DeviceSample) *Runner {
	return &Runner{st: st, db: db, Samples: samples, Events: make(chan model.Event, 256), next: map[string]time.Time{}, running: map[string]bool{}, clients: map[string]*unifiClient{}}
}

// Run until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, in := range r.st.Integrations() {
				if !in.Enabled {
					continue
				}
				r.mu.Lock()
				nx, ok := r.next[in.ID]
				if !ok {
					nx = now
					r.next[in.ID] = nx
				}
				due := !now.Before(nx) && !r.running[in.ID]
				if due {
					r.running[in.ID] = true
					every := in.Every
					if every < 30 {
						every = 60
					}
					r.next[in.ID] = now.Add(time.Duration(every) * time.Second)
				}
				r.mu.Unlock()
				if due {
					go func(in model.Integration) {
						r.RunOnce(ctx, in)
						r.mu.Lock()
						r.running[in.ID] = false
						r.mu.Unlock()
					}(in)
				}
			}
		}
	}
}

// Now schedules an immediate run.
func (r *Runner) Now(id string) {
	r.mu.Lock()
	r.next[id] = time.Now()
	r.mu.Unlock()
}

// Forget drops cached sessions.
func (r *Runner) Forget(id string) {
	r.mu.Lock()
	delete(r.next, id)
	delete(r.clients, id)
	r.mu.Unlock()
}

// RunOnce fetches one integration and records the outcome.
func (r *Runner) RunOnce(ctx context.Context, in model.Integration) error {
	r.mu.Lock()
	r.Runs++
	r.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var res *Result
	var err error
	switch in.Kind {
	case "unifi":
		res, err = r.unifi(cctx, in)
	case "meraki":
		res, err = merakiFetch(cctx, in)
	default:
		err = fmt.Errorf("unknown integration kind %q", in.Kind)
	}
	now := time.Now()
	if err != nil {
		r.mu.Lock()
		r.Fails++
		r.mu.Unlock()
		if cur, e := r.st.Integration(in.ID); e == nil {
			if cur.LastErr == "" {
				r.emit(model.Event{Kind: "integration_failed", Source: "api", Severity: model.SevMinor, Domain: model.DomainNetwork,
					Message: fmt.Sprintf("%s (%s): %s", in.Name, in.Kind, err.Error()), DedupKey: "integration:" + in.ID})
			}
			cur.LastRun, cur.LastErr = now, err.Error()
			r.st.PutIntegration(cur)
		}
		return err
	}
	devices, clients := r.apply(in, res, now)
	if cur, e := r.st.Integration(in.ID); e == nil {
		if cur.LastErr != "" {
			r.emit(model.Event{Kind: "integration_ok", Source: "api", Severity: model.SevInfo, Domain: model.DomainNetwork,
				Message: fmt.Sprintf("%s (%s) reachable again", in.Name, in.Kind), DedupKey: "integration:" + in.ID})
		}
		cur.LastRun, cur.LastErr, cur.Devices, cur.Clients = now, "", devices, clients
		r.st.PutIntegration(cur)
	}
	return nil
}

func (r *Runner) emit(ev model.Event) {
	ev.TS = time.Now()
	select {
	case r.Events <- ev:
	default:
	}
}

// ---- common result shape ----------------------------------------------------------------

// APInfo is one device as a controller sees it.
type APInfo struct {
	Key     string // unique within the integration (mac or serial)
	Name    string
	IP      string
	MAC     string
	Model   string
	Version string
	Serial  string
	Kind    string // ap|switch|gateway|other
	Up      bool
	UptimeS int64
	CPU     float64 // -1 unknown
	MemPct  float64 // -1 unknown
	Clients int
	Radios  []model.Radio
	SSIDs   map[string]int
	Upgrad  bool
	Satisf  int
	Uplinks []model.SDWANLink
	Site    string
}

// Result is everything one fetch produced.
type Result struct {
	Devices    []APInfo
	Controller string // display name
}

// apply turns a result into devices, samples, wireless state and metrics.
func (r *Runner) apply(in model.Integration, res *Result, now time.Time) (int, int) {
	managed := in.Kind + ":" + in.ID
	byKey := map[string]model.Device{}
	for _, d := range r.st.Devices() {
		if d.Managed == managed {
			byKey[strings.TrimPrefix(d.Notes, "key:")] = d
		}
	}
	clients := 0
	seen := map[string]bool{}
	for _, ap := range res.Devices {
		seen[ap.Key] = true
		clients += ap.Clients
		d, ok := byKey[ap.Key]
		if !ok {
			// new device under the cap
			if r.Caps != nil {
				if maxDev, mon := r.Caps(); maxDev > 0 && mon >= maxDev {
					continue
				}
			}
			role := model.RoleAP
			switch ap.Kind {
			case "switch":
				role = model.RoleAccess
			case "gateway":
				role = model.RoleFirewall
			}
			d = model.Device{ID: model.NewID("dev"), SiteID: in.SiteID, Name: ap.Name, IP: ap.IP, Domain: model.DomainNetwork, Role: role, Vendor: vendorOf(in.Kind), Model: ap.Model,
				OSVersion: ap.Version, Serial: ap.Serial, ChassisMAC: ap.MAC, Managed: managed, Notes: "key:" + ap.Key, PollEvery: 60, Status: model.StatusUnknown, StatusSince: now, Created: now, Monitored: true, ProfileID: in.Kind}
			if d.Name == "" {
				d.Name = ap.Model + " " + ap.MAC
			}
			if d.SiteID == "" {
				if sites := r.st.Sites(); len(sites) > 0 {
					d.SiteID = sites[0].ID
				}
			}
			r.st.PutDevice(d)
			byKey[ap.Key] = d
			r.emit(model.Event{Kind: "device_discovered", DeviceID: d.ID, Source: in.Kind, Severity: model.SevInfo, Domain: d.Domain,
				Message: fmt.Sprintf("%s imported from %s (%s %s)", d.Name, in.Name, ap.Model, ap.Kind), DedupKey: "device_discovered:" + d.ID})
		} else {
			r.st.UpdateDevice(d.ID, func(x *model.Device) {
				if ap.Name != "" {
					x.Name = ap.Name
				}
				if ap.IP != "" {
					x.IP = ap.IP
				}
				x.Model, x.OSVersion, x.Serial = ap.Model, ap.Version, ap.Serial
				if ap.MAC != "" {
					x.ChassisMAC = ap.MAC
				}
			})
		}
		if !d.Monitored {
			continue
		}
		// status sample: the controller's word is what we have
		s := model.DeviceSample{DeviceID: d.ID, TS: now, Reachable: ap.Up, SNMPOK: ap.Up, Uptime: ap.UptimeS, CPU: ap.CPU, MemPct: ap.MemPct, TempC: -1000, Sessions: -1}
		if !ap.Up {
			// stale figures from the controller's last contact are not "now"
			s.Err = "controller reports the device down"
			s.Uptime, s.CPU, s.MemPct = -1, -1, -1
			ap.CPU, ap.MemPct = -1, -1
		}
		if r.Samples != nil {
			select {
			case r.Samples <- s:
			default:
			}
		}
		if ap.CPU >= 0 {
			r.db.Append("cpu_pct|"+d.ID, now.Unix(), ap.CPU)
		}
		if ap.MemPct >= 0 {
			r.db.Append("mem_pct|"+d.ID, now.Unix(), ap.MemPct)
		}
		if ap.Kind == "ap" {
			r.db.Append("wifi_clients|"+d.ID, now.Unix(), float64(ap.Clients))
			r.st.SetWireless(model.Wireless{DeviceID: d.ID, TS: now, Clients: ap.Clients, Radios: ap.Radios, SSIDs: ap.SSIDs, Controller: in.Name, Model: ap.Model, Version: ap.Version, Upgradable: ap.Upgrad, Satisf: ap.Satisf})
		}
		if len(ap.Uplinks) > 0 {
			for i := range ap.Uplinks {
				ap.Uplinks[i].DeviceID, ap.Uplinks[i].TS = d.ID, now
			}
			prev := r.st.SDWAN(d.ID)
			r.st.SetSDWAN(d.ID, ap.Uplinks)
			r.sdwanEvents(d, prev, ap.Uplinks)
			for _, u := range ap.Uplinks {
				up := 0.0
				if u.Up {
					up = 1
				}
				key := d.ID + "|" + u.Name + "/" + u.Interface
				r.db.Append("sdwan_up|"+key, now.Unix(), up)
				if u.Up && (u.LatencyMs > 0 || u.LossPct > 0) {
					r.db.Append("sdwan_latency|"+key, now.Unix(), u.LatencyMs)
					r.db.Append("sdwan_loss|"+key, now.Unix(), u.LossPct)
				}
			}
		}
	}
	// devices the controller no longer lists: leave them, mark unreachable
	for key, d := range byKey {
		if !seen[key] && d.Monitored && r.Samples != nil {
			select {
			case r.Samples <- model.DeviceSample{DeviceID: d.ID, TS: now, Reachable: false, Uptime: -1, CPU: -1, MemPct: -1, TempC: -1000, Sessions: -1, Err: "no longer listed by " + in.Name}:
			default:
			}
		}
	}
	r.db.Append("wifi_clients|integ_"+in.ID, now.Unix(), float64(clients))
	return len(res.Devices), clients
}

// sdwanEvents raises link up/down events.
func (r *Runner) sdwanEvents(d model.Device, prev, cur []model.SDWANLink) {
	down, up := model.SDWANChanges(prev, cur)
	for _, l := range down {
		r.emit(model.Event{Kind: "sdwan_link_down", DeviceID: d.ID, Source: "api", Severity: model.SevMajor, Domain: d.Domain,
			Message: fmt.Sprintf("%s: WAN link %s (%s) is %s", d.Name, l.Name, l.Interface, l.State), DedupKey: "sdwan:" + d.ID + ":" + l.Name + "|" + l.Interface})
	}
	for _, l := range up {
		r.emit(model.Event{Kind: "sdwan_link_up", DeviceID: d.ID, Source: "api", Severity: model.SevInfo, Domain: d.Domain,
			Message: fmt.Sprintf("%s: WAN link %s (%s) back to %s", d.Name, l.Name, l.Interface, l.State), DedupKey: "sdwan:" + d.ID + ":" + l.Name + "|" + l.Interface})
	}
}

func vendorOf(kind string) string {
	switch kind {
	case "unifi":
		return "Ubiquiti"
	case "meraki":
		return "Cisco Meraki"
	}
	return kind
}

// ---- HTTP helpers ------------------------------------------------------------------------

func httpClient(insecure bool, jar http.CookieJar) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Jar: jar, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, Proxy: http.ProxyFromEnvironment}}
}

func getJSON(ctx context.Context, c *http.Client, url string, hdr map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(firstLine(string(b), 200)))
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: not JSON (%s)", url, firstLine(string(b), 80))
	}
	return nil
}

func firstLine(s string, n int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// ---- UniFi ---------------------------------------------------------------------------------

type unifiClient struct {
	c        *http.Client
	base     string
	udm      bool // UDM/UniFi OS: /proxy/network prefix and /api/auth/login
	csrf     string
	loggedAt time.Time
}

func (r *Runner) unifi(ctx context.Context, in model.Integration) (*Result, error) {
	r.mu.Lock()
	uc := r.clients[in.ID]
	r.mu.Unlock()
	if uc == nil || time.Since(uc.loggedAt) > 50*time.Minute {
		jar, _ := cookiejar.New(nil)
		uc = &unifiClient{c: httpClient(in.Insecure, jar), base: strings.TrimRight(in.URL, "/")}
		if err := uc.login(ctx, in.Username, in.Password); err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.clients[in.ID] = uc
		r.mu.Unlock()
	}
	site := in.Site
	if site == "" {
		site = "default"
	}
	res, err := uc.fetch(ctx, site)
	if err != nil && (strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403")) {
		// session expired: log in again once
		r.mu.Lock()
		delete(r.clients, in.ID)
		r.mu.Unlock()
		jar, _ := cookiejar.New(nil)
		uc = &unifiClient{c: httpClient(in.Insecure, jar), base: strings.TrimRight(in.URL, "/")}
		if err := uc.login(ctx, in.Username, in.Password); err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.clients[in.ID] = uc
		r.mu.Unlock()
		res, err = uc.fetch(ctx, site)
	}
	return res, err
}

func (u *unifiClient) login(ctx context.Context, user, pass string) error {
	body, _ := json.Marshal(map[string]any{"username": user, "password": pass, "remember": true})
	// UniFi OS (UDM, Cloud Key Gen2+, self-hosted 8.x with UniFi OS) first
	for _, try := range []struct {
		path string
		udm  bool
	}{{"/api/auth/login", true}, {"/api/login", false}} {
		req, _ := http.NewRequestWithContext(ctx, "POST", u.base+try.path, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := u.c.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == 200 {
			u.udm = try.udm
			u.csrf = resp.Header.Get("X-CSRF-Token")
			u.loggedAt = time.Now()
			return nil
		}
		if resp.StatusCode == 401 || resp.StatusCode == 400 {
			if try.udm {
				continue // maybe a classic controller
			}
			return errors.New("UniFi login refused (check user name / password; a local read-only admin is enough)")
		}
	}
	return errors.New("UniFi login failed on both /api/auth/login and /api/login")
}

func (u *unifiClient) api(site, path string) string {
	if u.udm {
		return u.base + "/proxy/network/api/s/" + site + path
	}
	return u.base + "/api/s/" + site + path
}

type unifiDevice struct {
	Type       string  `json:"type"`
	Name       string  `json:"name"`
	IP         string  `json:"ip"`
	MAC        string  `json:"mac"`
	Model      string  `json:"model"`
	Version    string  `json:"version"`
	Serial     string  `json:"serial"`
	State      int     `json:"state"`
	Uptime     int64   `json:"uptime"`
	NumSta     int     `json:"num_sta"`
	Upgradable bool    `json:"upgradable"`
	Satisf     float64 `json:"satisfaction"`
	SysStats   struct {
		CPU json.RawMessage `json:"cpu"`
		Mem json.RawMessage `json:"mem"`
	} `json:"system-stats"`
	RadioTable []struct {
		Name    string `json:"name"`
		Radio   string `json:"radio"`
		Channel any    `json:"channel"`
		HT      any    `json:"ht"`
		TxPower any    `json:"tx_power"`
	} `json:"radio_table"`
	RadioStats []struct {
		Name    string  `json:"name"`
		Radio   string  `json:"radio"`
		NumSta  int     `json:"num_sta"`
		CuTotal float64 `json:"cu_total"`
		Channel any     `json:"channel"`
	} `json:"radio_table_stats"`
	VapTable []struct {
		Essid  string `json:"essid"`
		NumSta int    `json:"num_sta"`
		Radio  string `json:"radio"`
	} `json:"vap_table"`
	Uplink struct {
		Type string `json:"type"`
	} `json:"uplink"`
	WAN1 *struct {
		Up     bool   `json:"up"`
		IP     string `json:"ip"`
		Name   string `json:"name"`
		Ifname string `json:"ifname"`
	} `json:"wan1"`
	WAN2 *struct {
		Up     bool   `json:"up"`
		IP     string `json:"ip"`
		Name   string `json:"name"`
		Ifname string `json:"ifname"`
	} `json:"wan2"`
}

func (u *unifiClient) fetch(ctx context.Context, site string) (*Result, error) {
	var resp struct {
		Data []unifiDevice `json:"data"`
		Meta struct {
			RC  string `json:"rc"`
			Msg string `json:"msg"`
		} `json:"meta"`
	}
	if err := getJSON(ctx, u.c, u.api(site, "/stat/device"), map[string]string{"X-CSRF-Token": u.csrf}, &resp); err != nil {
		return nil, err
	}
	if resp.Meta.RC != "" && resp.Meta.RC != "ok" {
		return nil, fmt.Errorf("UniFi: %s", resp.Meta.Msg)
	}
	res := &Result{}
	for _, d := range resp.Data {
		ap := APInfo{Key: strings.ToLower(d.MAC), Name: d.Name, IP: d.IP, MAC: strings.ToLower(d.MAC), Model: d.Model, Version: d.Version, Serial: d.Serial, Up: d.State == 1, UptimeS: d.Uptime, Clients: d.NumSta, CPU: -1, MemPct: -1, Upgrad: d.Upgradable, Satisf: int(d.Satisf), Site: site}
		switch d.Type {
		case "uap":
			ap.Kind = "ap"
		case "usw":
			ap.Kind = "switch"
		case "ugw", "udm", "uxg", "ucg":
			ap.Kind = "gateway"
		default:
			ap.Kind = "other"
		}
		ap.CPU = rawFloat(d.SysStats.CPU)
		ap.MemPct = rawFloat(d.SysStats.Mem)
		for _, rs := range d.RadioStats {
			rd := model.Radio{Name: rs.Radio, Clients: rs.NumSta, Util: rs.CuTotal, Channel: anyInt(rs.Channel)}
			for _, rt := range d.RadioTable {
				if rt.Name == rs.Name {
					if rd.Channel == 0 {
						rd.Channel = anyInt(rt.Channel)
					}
					rd.Width, rd.TxPower = anyInt(rt.HT), anyInt(rt.TxPower)
				}
			}
			ap.Radios = append(ap.Radios, rd)
		}
		if len(d.VapTable) > 0 {
			ap.SSIDs = map[string]int{}
			for _, v := range d.VapTable {
				ap.SSIDs[v.Essid] += v.NumSta
			}
		}
		for _, w := range []*struct {
			Up     bool   `json:"up"`
			IP     string `json:"ip"`
			Name   string `json:"name"`
			Ifname string `json:"ifname"`
		}{d.WAN1, d.WAN2} {
			if w == nil || (w.IP == "" && !w.Up) {
				continue
			}
			state := "down"
			if w.Up {
				state = "up"
			}
			name := w.Name
			if name == "" {
				name = w.Ifname
			}
			ap.Uplinks = append(ap.Uplinks, model.SDWANLink{Name: name, Interface: w.Ifname, Up: w.Up, State: state, IP: w.IP})
		}
		res.Devices = append(res.Devices, ap)
	}
	return res, nil
}

func rawFloat(b json.RawMessage) float64 {
	if len(b) == 0 {
		return -1
	}
	var f float64
	if json.Unmarshal(b, &f) == nil {
		return f
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		fmt.Sscan(s, &f)
		return f
	}
	return -1
}

func anyInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n := 0
		fmt.Sscan(x, &n)
		return n
	}
	return 0
}

// ---- Meraki --------------------------------------------------------------------------------

func merakiFetch(ctx context.Context, in model.Integration) (*Result, error) {
	base := strings.TrimRight(in.URL, "/")
	if base == "" {
		base = "https://api.meraki.com"
	}
	c := httpClient(in.Insecure, nil)
	hdr := map[string]string{"X-Cisco-Meraki-API-Key": in.APIKey}
	var orgs []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := getJSON(ctx, c, base+"/api/v1/organizations", hdr, &orgs); err != nil {
		return nil, err
	}
	res := &Result{}
	for _, o := range orgs {
		if in.Site != "" && in.Site != o.ID && !strings.EqualFold(in.Site, o.Name) {
			continue
		}
		var devs []struct {
			Serial      string `json:"serial"`
			Name        string `json:"name"`
			Model       string `json:"model"`
			MAC         string `json:"mac"`
			LanIP       string `json:"lanIp"`
			Firmware    string `json:"firmware"`
			NetworkID   string `json:"networkId"`
			ProductType string `json:"productType"`
		}
		if err := getJSON(ctx, c, base+"/api/v1/organizations/"+o.ID+"/devices?perPage=1000", hdr, &devs); err != nil {
			return nil, err
		}
		var statuses []struct {
			Serial   string `json:"serial"`
			Status   string `json:"status"`
			PublicIP string `json:"publicIp"`
			LanIP    string `json:"lanIp"`
		}
		_ = getJSON(ctx, c, base+"/api/v1/organizations/"+o.ID+"/devices/statuses?perPage=1000", hdr, &statuses)
		stat := map[string]string{}
		for _, s := range statuses {
			stat[s.Serial] = s.Status
		}
		var uplinks []struct {
			Serial  string `json:"serial"`
			Uplinks []struct {
				Interface string `json:"interface"`
				Status    string `json:"status"`
				IP        string `json:"ip"`
				PublicIP  string `json:"publicIp"`
			} `json:"uplinks"`
		}
		_ = getJSON(ctx, c, base+"/api/v1/organizations/"+o.ID+"/appliance/uplink/statuses?perPage=1000", hdr, &uplinks)
		// loss / latency towards the uplink's probe target (last 5 minutes, newest sample)
		var lal []struct {
			Serial     string `json:"serial"`
			Uplink     string `json:"uplink"`
			TimeSeries []struct {
				LossPercent *float64 `json:"lossPercent"`
				LatencyMs   *float64 `json:"latencyMs"`
			} `json:"timeSeries"`
		}
		_ = getJSON(ctx, c, base+"/api/v1/organizations/"+o.ID+"/devices/uplinksLossAndLatency?timespan=300", hdr, &lal)
		type ll struct{ loss, lat float64 }
		health := map[string]ll{}
		for _, x := range lal {
			for i := len(x.TimeSeries) - 1; i >= 0; i-- {
				t := x.TimeSeries[i]
				if t.LatencyMs != nil || t.LossPercent != nil {
					v := ll{}
					if t.LossPercent != nil {
						v.loss = *t.LossPercent
					}
					if t.LatencyMs != nil {
						v.lat = *t.LatencyMs
					}
					health[x.Serial+"|"+x.Uplink] = v
					break
				}
			}
		}
		upBySerial := map[string][]model.SDWANLink{}
		for _, u := range uplinks {
			for _, l := range u.Uplinks {
				h := health[u.Serial+"|"+l.Interface]
				upBySerial[u.Serial] = append(upBySerial[u.Serial], model.SDWANLink{Name: l.Interface, Interface: l.Interface, Up: l.Status == "active" || l.Status == "ready", State: l.Status, IP: firstNonEmpty(l.PublicIP, l.IP), LatencyMs: h.lat, LossPct: h.loss})
			}
		}
		// wireless client counts per AP: clients of each network, grouped by recentDeviceSerial
		clientsBySerial := map[string]int{}
		ssidBySerial := map[string]map[string]int{}
		nets := map[string]bool{}
		for _, d := range devs {
			if d.ProductType == "wireless" && d.NetworkID != "" {
				nets[d.NetworkID] = true
			}
		}
		for net := range nets {
			var cls []struct {
				RecentDeviceSerial string `json:"recentDeviceSerial"`
				SSID               string `json:"ssid"`
				Status             string `json:"status"`
			}
			if err := getJSON(ctx, c, base+"/api/v1/networks/"+net+"/clients?perPage=1000&timespan=300", hdr, &cls); err != nil {
				continue
			}
			for _, cl := range cls {
				if cl.Status != "" && cl.Status != "Online" {
					continue
				}
				clientsBySerial[cl.RecentDeviceSerial]++
				if cl.SSID != "" {
					if ssidBySerial[cl.RecentDeviceSerial] == nil {
						ssidBySerial[cl.RecentDeviceSerial] = map[string]int{}
					}
					ssidBySerial[cl.RecentDeviceSerial][cl.SSID]++
				}
			}
		}
		for _, d := range devs {
			ap := APInfo{Key: d.Serial, Name: d.Name, IP: d.LanIP, MAC: strings.ToLower(d.MAC), Model: d.Model, Version: d.Firmware, Serial: d.Serial, CPU: -1, MemPct: -1, Site: o.Name}
			switch d.ProductType {
			case "wireless":
				ap.Kind = "ap"
			case "switch":
				ap.Kind = "switch"
			case "appliance":
				ap.Kind = "gateway"
			default:
				ap.Kind = "other"
			}
			st := stat[d.Serial]
			ap.Up = st == "online" || st == "alerting"
			ap.Clients = clientsBySerial[d.Serial]
			ap.SSIDs = ssidBySerial[d.Serial]
			ap.Uplinks = upBySerial[d.Serial]
			res.Devices = append(res.Devices, ap)
		}
	}
	if len(orgs) == 0 {
		return nil, errors.New("Meraki: the API key sees no organisation")
	}
	return res, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Test connects once and reports what it saw (for the dialog's Test button).
func (r *Runner) Test(ctx context.Context, in model.Integration) (string, error) {
	var res *Result
	var err error
	switch in.Kind {
	case "unifi":
		jar, _ := cookiejar.New(nil)
		uc := &unifiClient{c: httpClient(in.Insecure, jar), base: strings.TrimRight(in.URL, "/")}
		if err = uc.login(ctx, in.Username, in.Password); err == nil {
			site := in.Site
			if site == "" {
				site = "default"
			}
			res, err = uc.fetch(ctx, site)
		}
	case "meraki":
		res, err = merakiFetch(ctx, in)
	default:
		return "", fmt.Errorf("unknown kind")
	}
	if err != nil {
		return "", err
	}
	aps, up, cl := 0, 0, 0
	for _, d := range res.Devices {
		if d.Kind == "ap" {
			aps++
		}
		if d.Up {
			up++
		}
		cl += d.Clients
	}
	log.Printf("integration test %s: %d devices", in.Name, len(res.Devices))
	return fmt.Sprintf("%d devices (%d access points, %d up), %d wireless clients", len(res.Devices), aps, up, cl), nil
}
