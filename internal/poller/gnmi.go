package poller

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nizartuanku/topolight/internal/gnmi"
	"github.com/nizartuanku/topolight/internal/model"
)

// gNMI polling (beta): devices whose credential is a gNMI one are read over
// gRPC with OpenConfig paths instead of SNMP —
//
//	/system/state            hostname, boot-time (uptime), software-version
//	/system/cpus             total/instant averaged over the CPUs
//	/system/memory/state     physical, used|reserved
//	/interfaces              name, ifindex, admin/oper status, description,
//	                         counters, ethernet port-speed
//
// The same interface and device samples come out, so alerts, graphs and
// reports do not know the difference. Vendor-specific health (temperature,
// sessions) and the routing/L2 walk stay SNMP-only for now.

// GNMIClient builds a client for a device and its gNMI credential.
func GNMIClient(ip string, cred model.Credential) *gnmi.Client {
	port := cred.Port
	if port <= 0 {
		port = 6030
	}
	return &gnmi.Client{Target: net.JoinHostPort(ip, strconv.Itoa(port)), Username: cred.User, Password: cred.Password, TLS: !cred.PlainText, Insecure: cred.SkipVerify, Timeout: 15 * time.Second}
}

func (p *Poller) gnmiFor(d model.Device, cred model.Credential) *gnmi.Client {
	fp := "gnmi|" + cred.ID + "|" + cred.User + "|" + cred.Password + "|" + strconv.Itoa(cred.Port) + "|" + strconv.FormatBool(cred.PlainText) + "|" + d.IP
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gnmis == nil {
		p.gnmis = map[string]*gnmi.Client{}
	}
	if c := p.gnmis[d.ID]; c != nil && p.creds[d.ID] == fp {
		return c
	}
	if old := p.gnmis[d.ID]; old != nil {
		old.Close()
	}
	c := GNMIClient(d.IP, cred)
	p.gnmis[d.ID] = c
	p.creds[d.ID] = fp
	return c
}

// pollGNMI is the gNMI counterpart of the SNMP branch in pollDevice.
func (p *Poller) pollGNMI(ctx context.Context, d model.Device, cred model.Credential, ds *model.DeviceSample) {
	now := ds.TS
	c := p.gnmiFor(d, cred)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ups, err := c.Get(cctx, []string{"/system/state", "/system/cpus", "/system/memory/state", "/interfaces"}, 2, gnmi.EncJSONIETF)
	if err != nil {
		// a target that lacks one of the containers answers NOT_FOUND for the
		// whole request on some implementations: retry with the essentials only
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "invalid argument") {
			ups, err = c.Get(cctx, []string{"/system/state", "/interfaces"}, 2, gnmi.EncJSONIETF)
		}
	}
	if err != nil {
		ds.Err = "gnmi: " + err.Error()
		p.mu.Lock()
		p.Failures++
		p.mu.Unlock()
		if p.ping == nil {
			ds.Reachable = false
		}
		p.publishDevice(*ds)
		return
	}
	ds.SNMPOK = true // "management protocol answered"
	if p.ping == nil {
		ds.Reachable = true
	}
	t := gnmi.Tree(ups)

	// identity and uptime
	host := gnmi.Str(gnmi.Lookup(t, "/system/state/hostname"))
	if boot, ok := gnmi.Number(gnmi.Lookup(t, "/system/state/boot-time")); ok && boot > 0 {
		// nanoseconds since the epoch (OpenConfig); a few targets report seconds
		if boot > 1e15 {
			boot /= 1e9
		}
		ds.Uptime = int64(now.Sub(time.Unix(int64(boot), 0)).Seconds())
	}
	ver := gnmi.Str(gnmi.Lookup(t, "/system/state/software-version"))
	p.mu.Lock()
	prevUp, had := p.uptime[d.ID]
	p.uptime[d.ID] = ds.Uptime
	lastInv := p.invAt[d.ID]
	p.mu.Unlock()
	if had && ds.Uptime >= 0 && ds.Uptime < prevUp-5 && prevUp > 0 && ds.Uptime < 24*3600 {
		ds.Rebooted = true
		p.emitEvent(model.Event{Kind: "device_rebooted", DeviceID: d.ID, Source: "gnmi", Severity: model.SevMajor, Domain: d.Domain,
			Message: fmt.Sprintf("%s rebooted (uptime %s → %s)", d.Name, fmtDur(prevUp), fmtDur(ds.Uptime)), DedupKey: "device_rebooted:" + d.ID})
	}

	// CPU: average of every cpu's total/instant (the ALL pseudo-cpu when present)
	if cpus, ok := gnmi.Lookup(t, "/system/cpus/cpu").([]any); ok && len(cpus) > 0 {
		var all, sum float64
		n := 0
		haveAll := false
		for _, x := range cpus {
			m, _ := x.(map[string]any)
			if m == nil {
				continue
			}
			v, ok := gnmi.Number(gnmi.Lookup(m, "/state/total/instant"))
			if !ok {
				continue
			}
			if fmt.Sprint(m["index"]) == "ALL" {
				all, haveAll = v, true
			}
			sum += v
			n++
		}
		switch {
		case haveAll:
			ds.CPU = all
		case n > 0:
			ds.CPU = sum / float64(n)
		}
		if ds.CPU >= 0 {
			p.db.Append("cpu_pct|"+d.ID, now.Unix(), ds.CPU)
		}
	}
	// memory: used/physical, falling back to reserved/physical
	if phys, ok := gnmi.Number(gnmi.Lookup(t, "/system/memory/state/physical")); ok && phys > 0 {
		used, ok := gnmi.Number(gnmi.Lookup(t, "/system/memory/state/used"))
		if !ok {
			used, ok = gnmi.Number(gnmi.Lookup(t, "/system/memory/state/reserved"))
		}
		if ok {
			ds.MemPct = used * 100 / phys
			p.db.Append("mem_pct|"+d.ID, now.Unix(), ds.MemPct)
		}
	}

	// inventory every 15 minutes (or first time / after a reboot)
	ifList, _ := gnmi.Lookup(t, "/interfaces/interface").([]any)
	inventoryDue := lastInv.IsZero() || now.Sub(lastInv) > 15*time.Minute || ds.Rebooted || len(p.st.Interfaces(d.ID)) == 0
	if inventoryDue {
		p.gnmiInventory(d, host, ver, ifList)
		p.mu.Lock()
		p.invAt[d.ID] = now
		p.mu.Unlock()
	}

	// interface counters
	byName := map[string]map[string]any{}
	for _, x := range ifList {
		if m, _ := x.(map[string]any); m != nil {
			byName[gnmi.Str(m["name"])] = m
		}
	}
	for _, i := range p.st.Interfaces(d.ID) {
		m := byName[i.Name]
		s := model.InterfaceSample{IfID: i.ID, DeviceID: d.ID, Name: i.Name, TS: now, Important: i.Important, SpeedMbps: i.SpeedMbps, OperUp: i.OperUp, AdminUp: i.AdminUp}
		if m != nil {
			st, _ := m["state"].(map[string]any)
			if st != nil {
				s.OperUp = gnmi.Str(st["oper-status"]) == "UP"
				s.AdminUp = gnmi.Str(st["admin-status"]) == "UP"
				if cnt, _ := st["counters"].(map[string]any); cnt != nil {
					cur := &ifCounters{ts: now, hc: true, hcPkt: true, uptime: ds.Uptime,
						inOct: gnmi.Uint(cnt["in-octets"]), outOct: gnmi.Uint(cnt["out-octets"]),
						inErr: gnmi.Uint(cnt["in-errors"]), outErr: gnmi.Uint(cnt["out-errors"]),
						inPkt: gnmi.Uint(cnt["in-unicast-pkts"]), outPkt: gnmi.Uint(cnt["out-unicast-pkts"]),
						inDrp: gnmi.Uint(cnt["in-discards"]), outDrp: gnmi.Uint(cnt["out-discards"])}
					p.ifRates(i, &s, cur, now, ds.Rebooted)
				}
			}
		}
		select {
		case p.InterfaceSamples <- s:
		default:
		}
	}
	p.publishDevice(*ds)
}

// gnmiInventory refreshes identity and the interface table from /interfaces.
func (p *Poller) gnmiInventory(d model.Device, host, ver string, ifList []any) {
	p.st.UpdateDevice(d.ID, func(dev *model.Device) {
		if host != "" && (dev.Name == "" || dev.Name == dev.IP) {
			dev.Name = host
		}
		if ver != "" {
			dev.OSVersion = ver
		}
		if dev.SysDescr == "" {
			dev.SysDescr = "gNMI / OpenConfig"
		}
		if dev.Vendor == "" {
			dev.Vendor = "OpenConfig"
		}
	})
	existing := map[string]model.Interface{}
	for _, i := range p.st.Interfaces(d.ID) {
		existing[i.Name] = i
	}
	var list []model.Interface
	synth := 100000 // targets without ifindex get stable synthetic indexes by name order
	names := make([]string, 0, len(ifList))
	byName := map[string]map[string]any{}
	for _, x := range ifList {
		if m, _ := x.(map[string]any); m != nil {
			n := gnmi.Str(m["name"])
			byName[n] = m
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		m := byName[name]
		st, _ := m["state"].(map[string]any)
		if st == nil {
			st = map[string]any{}
		}
		idx := 0
		if v, ok := gnmi.Number(st["ifindex"]); ok {
			idx = int(v)
		}
		if idx == 0 {
			if old, ok := existing[name]; ok {
				idx = old.Index
			} else {
				synth++
				idx = synth
			}
		}
		typ := gnmi.Str(st["type"])
		if i := strings.LastIndex(typ, ":"); i >= 0 {
			typ = typ[i+1:]
		}
		kind := gnmiKind(typ, name)
		if kind == "skip" {
			continue
		}
		speed := int64(0)
		if eth, _ := m["ethernet"].(map[string]any); eth != nil {
			if es, _ := eth["state"].(map[string]any); es != nil {
				speed = portSpeedMbps(gnmi.Str(es["port-speed"]))
				if speed == 0 {
					speed = portSpeedMbps(gnmi.Str(es["negotiated-port-speed"]))
				}
			}
		}
		if speed == 0 {
			if agg, _ := m["aggregation"].(map[string]any); agg != nil {
				if as, _ := agg["state"].(map[string]any); as != nil {
					if v, ok := gnmi.Number(as["lag-speed"]); ok {
						speed = int64(v)
					}
				}
			}
		}
		mac := ""
		if eth, _ := m["ethernet"].(map[string]any); eth != nil {
			if es, _ := eth["state"].(map[string]any); es != nil {
				mac = strings.ToLower(gnmi.Str(es["mac-address"]))
			}
		}
		i := model.Interface{ID: model.IfID(d.ID, idx), DeviceID: d.ID, Index: idx, Name: name, Kind: kind, Alias: gnmi.Str(st["description"]), SpeedMbps: speed, MAC: mac,
			AdminUp: gnmi.Str(st["admin-status"]) == "UP", OperUp: gnmi.Str(st["oper-status"]) == "UP", Status: model.StatusUnknown}
		i.Important = kind == "lag" || importantAlias(i.Alias) || importantName(name)
		list = append(list, i)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Index < list[b].Index })
	p.st.PutInterfaces(d.ID, list)
}

func gnmiKind(ianaType, name string) string {
	switch ianaType {
	case "ethernetCsmacd", "gigabitEthernet":
		return "phys"
	case "ieee8023adLag":
		return "lag"
	case "l2vlan", "l3ipvlan", "propVirtual":
		return "vlan"
	case "tunnel", "mplsTunnel", "gre", "ipsec":
		return "tunnel"
	case "softwareLoopback":
		return "loopback"
	}
	l := strings.ToLower(name)
	switch {
	case strings.HasPrefix(l, "port-channel"), strings.HasPrefix(l, "po"), strings.HasPrefix(l, "ae"), strings.HasPrefix(l, "bond"), strings.HasPrefix(l, "lag"):
		return "lag"
	case strings.HasPrefix(l, "vlan"), strings.HasPrefix(l, "irb"), strings.HasPrefix(l, "bvi"):
		return "vlan"
	case strings.HasPrefix(l, "lo"):
		return "loopback"
	case strings.HasPrefix(l, "null"):
		return "skip"
	}
	return "phys"
}

// portSpeedMbps maps openconfig-if-ethernet SPEED_* identities to Mbps.
func portSpeedMbps(s string) int64 {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimPrefix(s, "SPEED_")
	switch {
	case s == "", s == "UNKNOWN":
		return 0
	case strings.HasSuffix(s, "MB"):
		v, _ := strconv.ParseInt(strings.TrimSuffix(s, "MB"), 10, 64)
		return v
	case strings.HasSuffix(s, "GB"):
		v, _ := strconv.ParseInt(strings.TrimSuffix(s, "GB"), 10, 64)
		return v * 1000
	case strings.HasSuffix(s, "TB"):
		v, _ := strconv.ParseInt(strings.TrimSuffix(s, "TB"), 10, 64)
		return v * 1000000
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}
