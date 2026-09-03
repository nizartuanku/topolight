package poller

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/snmp"
)

// Wireless controllers and SD-WAN health read over SNMP, once per slow cycle.
//
//   - Cisco WLC (AireOS and Catalyst 9800): AIRESPACE-WIRELESS-MIB bsnAPTable /
//     bsnAPIfTable / bsnAPIfLoadParametersTable / bsnMobileStationTable
//   - Aruba mobility controllers: WLSX-WLAN-MIB wlanAPTable / wlanAPRadioTable
//   - FortiGate SD-WAN: FORTINET-FORTIGATE-MIB fgVWLHealthCheckLinkTable
//
// Access points become managed devices ("wlc:<controller id>") so they get a
// status, alerts and a Wireless tab of their own without being polled directly.

const (
	// AIRESPACE-WIRELESS-MIB
	oidBsnAPName     = "1.3.6.1.4.1.14179.2.2.1.1.3"
	oidBsnAPLocation = "1.3.6.1.4.1.14179.2.2.1.1.4"
	oidBsnAPOper     = "1.3.6.1.4.1.14179.2.2.1.1.6" // 1 associated, 2 disassociating, 3 downloading
	oidBsnAPSoftware = "1.3.6.1.4.1.14179.2.2.1.1.8"
	oidBsnAPModel    = "1.3.6.1.4.1.14179.2.2.1.1.16"
	oidBsnAPSerial   = "1.3.6.1.4.1.14179.2.2.1.1.17"
	oidBsnAPIP       = "1.3.6.1.4.1.14179.2.2.1.1.19"
	oidBsnAPIfType   = "1.3.6.1.4.1.14179.2.2.2.1.2"  // 1 dot11b/g, 2 dot11a, 3 uwb, 4 dot11abgn, 5 dot11ac...; 6 GHz reported as 3+ on 9800
	oidBsnAPIfChan   = "1.3.6.1.4.1.14179.2.2.2.1.4"  // bsnAPIfPhyChannelNumber
	oidBsnAPIfTxPwr  = "1.3.6.1.4.1.14179.2.2.2.1.6"  // bsnAPIfPhyTxPowerLevel (1 = max)
	oidBsnAPIfOper   = "1.3.6.1.4.1.14179.2.2.2.1.12" // 1 up 2 down
	oidBsnAPIfUsers  = "1.3.6.1.4.1.14179.2.2.2.1.15" // bsnApIfNoOfUsers
	oidBsnAPIfChUtil = "1.3.6.1.4.1.14179.2.2.13.1.3" // bsnAPIfLoadChannelUtilization
	oidBsnStaAPMac   = "1.3.6.1.4.1.14179.2.1.4.1.4"  // bsnMobileStationAPMacAddr
	oidBsnStaSSID    = "1.3.6.1.4.1.14179.2.1.4.1.7"  // bsnMobileStationSsid

	// WLSX-WLAN-MIB (Aruba)
	oidArubaAPIP      = "1.3.6.1.4.1.14823.2.2.1.5.2.1.4.1.2"
	oidArubaAPName    = "1.3.6.1.4.1.14823.2.2.1.5.2.1.4.1.3"
	oidArubaAPGroup   = "1.3.6.1.4.1.14823.2.2.1.5.2.1.4.1.4"
	oidArubaAPModel   = "1.3.6.1.4.1.14823.2.2.1.5.2.1.4.1.5"
	oidArubaAPSerial  = "1.3.6.1.4.1.14823.2.2.1.5.2.1.4.1.6"
	oidArubaAPVersion = "1.3.6.1.4.1.14823.2.2.1.5.2.1.4.1.8"
	oidArubaAPStatus  = "1.3.6.1.4.1.14823.2.2.1.5.2.1.4.1.19" // 1 up 2 down
	oidArubaRadioType = "1.3.6.1.4.1.14823.2.2.1.5.2.1.5.1.2"  // 1 dot11a 2 dot11b 3 dot11g 4 dot11ag? (vendor enum)
	oidArubaRadioChan = "1.3.6.1.4.1.14823.2.2.1.5.2.1.5.1.3"
	oidArubaRadioPwr  = "1.3.6.1.4.1.14823.2.2.1.5.2.1.5.1.4"
	oidArubaRadioSta  = "1.3.6.1.4.1.14823.2.2.1.5.2.1.5.1.6" // wlanAPRadioNumAssociatedClients
	oidArubaRadioUtil = "1.3.6.1.4.1.14823.2.2.1.5.2.1.5.1.9" // wlanAPRadioUtilization (when present)

	// FORTINET-FORTIGATE-MIB fgVWLHealthCheckLinkTable
	oidFgVWLName   = "1.3.6.1.4.1.12356.101.4.9.2.1.2"
	oidFgVWLState  = "1.3.6.1.4.1.12356.101.4.9.2.1.4" // 0 alive, 1 dead
	oidFgVWLLat    = "1.3.6.1.4.1.12356.101.4.9.2.1.5"
	oidFgVWLJitter = "1.3.6.1.4.1.12356.101.4.9.2.1.6"
	oidFgVWLLoss   = "1.3.6.1.4.1.12356.101.4.9.2.1.9"
	oidFgVWLIfName = "1.3.6.1.4.1.12356.101.4.9.2.1.11"
)

// Caps, when set, reports (licence device limit, monitored devices) so AP
// import stops at the cap like discovery does.
type wlAP struct {
	key, name, ip, model, serial, version, location string
	up                                              bool
	clients                                         int
	radios                                          []model.Radio
	ssids                                           map[string]int
}

// pollWireless dispatches on the controller's profile.
func (p *Poller) pollWireless(ctx context.Context, c *snmp.Client, d model.Device, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	switch d.ProfileID {
	case "cisco-wlc":
		if aps, ok := walkCiscoWLC(ctx, c); ok {
			p.applyAPs(d, aps, "Cisco", now)
		}
	case "aruba-controller":
		if aps, ok := walkArubaController(ctx, c); ok {
			p.applyAPs(d, aps, "Aruba", now)
		}
	case "fortinet-fortigate":
		if links, ok := walkFortiSDWAN(ctx, c, now); ok {
			p.applySDWAN(d, links, now)
		}
	}
}

func walkTable(ctx context.Context, c *snmp.Client, oid string, f func(idx string, v snmp.Value)) bool {
	vbs, err := c.WalkContext(ctx, oid)
	if err != nil {
		return false
	}
	for _, vb := range vbs {
		f(snmp.OIDSuffix(vb.OID, oid), vb.Value)
	}
	return len(vbs) > 0
}

// macKey turns an OID index of six sub-identifiers into aa:bb:cc:dd:ee:ff.
func macKey(idx string) (string, string) {
	parts := strings.Split(idx, ".")
	if len(parts) < 6 {
		return "", ""
	}
	b := make([]string, 6)
	for i := 0; i < 6; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return "", ""
		}
		b[i] = fmt.Sprintf("%02x", n)
	}
	return strings.Join(b, ":"), strings.Join(parts[6:], ".")
}

func walkCiscoWLC(ctx context.Context, c *snmp.Client) (map[string]*wlAP, bool) {
	aps := map[string]*wlAP{}
	get := func(idx string) *wlAP {
		mac, _ := macKey(idx)
		if mac == "" {
			return nil
		}
		a := aps[mac]
		if a == nil {
			a = &wlAP{key: mac}
			aps[mac] = a
		}
		return a
	}
	if !walkTable(ctx, c, oidBsnAPName, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.name = v.String()
		}
	}) {
		return nil, false
	}
	walkTable(ctx, c, oidBsnAPOper, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.up = v.Int == 1
		}
	})
	walkTable(ctx, c, oidBsnAPIP, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			if len(v.Bytes) == 4 {
				a.ip = net.IP(v.Bytes).String()
			} else {
				a.ip = v.String()
			}
		}
	})
	walkTable(ctx, c, oidBsnAPModel, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.model = strings.TrimSpace(v.String())
		}
	})
	walkTable(ctx, c, oidBsnAPSerial, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.serial = strings.TrimSpace(v.String())
		}
	})
	walkTable(ctx, c, oidBsnAPSoftware, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.version = strings.TrimSpace(v.String())
		}
	})
	walkTable(ctx, c, oidBsnAPLocation, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.location = strings.TrimSpace(v.String())
		}
	})
	// radios: index mac.slot
	radio := func(idx string) (*wlAP, *model.Radio) {
		mac, rest := macKey(idx)
		a := aps[mac]
		if a == nil {
			return nil, nil
		}
		slot, _ := strconv.Atoi(rest)
		for len(a.radios) <= slot {
			a.radios = append(a.radios, model.Radio{Name: "slot" + strconv.Itoa(len(a.radios))})
		}
		return a, &a.radios[slot]
	}
	walkTable(ctx, c, oidBsnAPIfType, func(idx string, v snmp.Value) {
		if _, r := radio(idx); r != nil {
			switch v.Int {
			case 1:
				r.Name = "2.4 GHz"
			case 2:
				r.Name = "5 GHz"
			case 3:
				r.Name = "6 GHz"
			default:
				r.Name = "radio " + strconv.FormatInt(v.Int, 10)
			}
		}
	})
	walkTable(ctx, c, oidBsnAPIfChan, func(idx string, v snmp.Value) {
		if _, r := radio(idx); r != nil {
			r.Channel = int(v.Int)
		}
	})
	walkTable(ctx, c, oidBsnAPIfTxPwr, func(idx string, v snmp.Value) {
		if _, r := radio(idx); r != nil {
			r.TxLevel = int(v.Int) // power level 1..8, 1 = maximum
		}
	})
	walkTable(ctx, c, oidBsnAPIfUsers, func(idx string, v snmp.Value) {
		if _, r := radio(idx); r != nil {
			r.Clients = int(v.Int)
		}
	})
	walkTable(ctx, c, oidBsnAPIfChUtil, func(idx string, v snmp.Value) {
		if _, r := radio(idx); r != nil {
			r.Util = float64(v.Int)
		}
	})
	// stations: SSID per client, AP by MAC
	staAP := map[string]string{}
	walkTable(ctx, c, oidBsnStaAPMac, func(idx string, v snmp.Value) {
		if len(v.Bytes) == 6 {
			staAP[idx] = snmp.MACString(v.Bytes)
		}
	})
	if len(staAP) > 0 {
		walkTable(ctx, c, oidBsnStaSSID, func(idx string, v snmp.Value) {
			if a := aps[strings.ToLower(staAP[idx])]; a != nil {
				if a.ssids == nil {
					a.ssids = map[string]int{}
				}
				a.ssids[v.String()]++
			}
		})
	}
	for _, a := range aps {
		for _, r := range a.radios {
			a.clients += r.Clients
		}
		if a.clients == 0 {
			for _, n := range a.ssids {
				a.clients += n
			}
		}
	}
	return aps, true
}

func walkArubaController(ctx context.Context, c *snmp.Client) (map[string]*wlAP, bool) {
	aps := map[string]*wlAP{}
	get := func(idx string) *wlAP {
		mac, _ := macKey(idx)
		if mac == "" {
			return nil
		}
		a := aps[mac]
		if a == nil {
			a = &wlAP{key: mac}
			aps[mac] = a
		}
		return a
	}
	if !walkTable(ctx, c, oidArubaAPName, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.name = v.String()
		}
	}) {
		return nil, false
	}
	walkTable(ctx, c, oidArubaAPStatus, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.up = v.Int == 1
		}
	})
	walkTable(ctx, c, oidArubaAPIP, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			if len(v.Bytes) == 4 {
				a.ip = net.IP(v.Bytes).String()
			} else {
				a.ip = v.String()
			}
		}
	})
	walkTable(ctx, c, oidArubaAPModel, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.model = strings.TrimSpace(v.String())
		}
	})
	walkTable(ctx, c, oidArubaAPSerial, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.serial = strings.TrimSpace(v.String())
		}
	})
	walkTable(ctx, c, oidArubaAPVersion, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.version = strings.TrimSpace(v.String())
		}
	})
	walkTable(ctx, c, oidArubaAPGroup, func(idx string, v snmp.Value) {
		if a := get(idx); a != nil {
			a.location = strings.TrimSpace(v.String())
		}
	})
	radio := func(idx string) *model.Radio {
		mac, rest := macKey(idx)
		a := aps[mac]
		if a == nil {
			return nil
		}
		n, _ := strconv.Atoi(rest)
		for len(a.radios) <= n {
			a.radios = append(a.radios, model.Radio{Name: "radio " + strconv.Itoa(len(a.radios))})
		}
		return &a.radios[n]
	}
	walkTable(ctx, c, oidArubaRadioType, func(idx string, v snmp.Value) {
		if r := radio(idx); r != nil {
			switch v.Int {
			case 1, 4, 6:
				r.Name = "5 GHz"
			case 2, 3, 5:
				r.Name = "2.4 GHz"
			case 7, 8:
				r.Name = "6 GHz"
			}
		}
	})
	walkTable(ctx, c, oidArubaRadioChan, func(idx string, v snmp.Value) {
		if r := radio(idx); r != nil {
			r.Channel = int(v.Int)
		}
	})
	walkTable(ctx, c, oidArubaRadioPwr, func(idx string, v snmp.Value) {
		if r := radio(idx); r != nil {
			r.TxPower = int(v.Int)
		}
	})
	walkTable(ctx, c, oidArubaRadioSta, func(idx string, v snmp.Value) {
		if r := radio(idx); r != nil {
			r.Clients = int(v.Int)
		}
	})
	walkTable(ctx, c, oidArubaRadioUtil, func(idx string, v snmp.Value) {
		if r := radio(idx); r != nil {
			r.Util = float64(v.Int)
		}
	})
	for _, a := range aps {
		for _, r := range a.radios {
			a.clients += r.Clients
		}
	}
	return aps, true
}

// applyAPs turns controller rows into managed devices, samples, wireless
// state and metrics; the controller itself gets an aggregate Wireless entry.
func (p *Poller) applyAPs(ctrl model.Device, aps map[string]*wlAP, vendor string, now time.Time) {
	managed := "wlc:" + ctrl.ID
	byKey := map[string]model.Device{}
	for _, d := range p.st.Devices() {
		if d.Managed == managed {
			byKey[strings.TrimPrefix(d.Notes, "key:")] = d
		}
	}
	keys := make([]string, 0, len(aps))
	for k := range aps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	total, up, clients := 0, 0, 0
	seen := map[string]bool{}
	for _, k := range keys {
		a := aps[k]
		total++
		clients += a.clients
		if a.up {
			up++
		}
		seen[k] = true
		d, ok := byKey[k]
		if !ok {
			if p.Caps != nil {
				if maxDev, mon := p.Caps(); maxDev > 0 && mon >= maxDev {
					continue
				}
			}
			name := a.name
			if name == "" {
				name = vendor + " AP " + k
			}
			d = model.Device{ID: model.NewID("dev"), SiteID: ctrl.SiteID, Name: name, IP: a.ip, Domain: model.DomainNetwork, Role: model.RoleAP, Vendor: vendor, Model: a.model,
				OSVersion: a.version, Serial: a.serial, ChassisMAC: k, Location: a.location, Managed: managed, Notes: "key:" + k, PollEvery: ctrl.PollEvery, Status: model.StatusUnknown, StatusSince: now, Created: now, Monitored: true, ProfileID: ctrl.ProfileID}
			p.st.PutDevice(d)
			byKey[k] = d
			p.emitEvent(model.Event{Kind: "device_discovered", DeviceID: d.ID, Source: "snmp", Severity: model.SevInfo, Domain: d.Domain,
				Message: fmt.Sprintf("%s discovered on controller %s (%s)", d.Name, ctrl.Name, a.model), DedupKey: "device_discovered:" + d.ID})
		} else {
			p.st.UpdateDevice(d.ID, func(x *model.Device) {
				if a.name != "" {
					x.Name = a.name
				}
				if a.ip != "" {
					x.IP = a.ip
				}
				x.Model, x.OSVersion, x.Serial, x.Location = a.model, a.version, a.serial, a.location
			})
		}
		if !d.Monitored {
			continue
		}
		s := model.DeviceSample{DeviceID: d.ID, TS: now, Reachable: a.up, SNMPOK: a.up, Uptime: -1, CPU: -1, MemPct: -1, TempC: -1000, Sessions: -1}
		if !a.up {
			s.Err = "controller " + ctrl.Name + " reports the access point down"
		}
		p.publishDevice(s)
		p.db.Append("wifi_clients|"+d.ID, now.Unix(), float64(a.clients))
		p.st.SetWireless(model.Wireless{DeviceID: d.ID, TS: now, Clients: a.clients, Radios: a.radios, SSIDs: a.ssids, Controller: ctrl.Name, Model: a.model, Version: a.version})
	}
	for k, d := range byKey {
		if !seen[k] && d.Monitored {
			p.publishDevice(model.DeviceSample{DeviceID: d.ID, TS: now, Reachable: false, Uptime: -1, CPU: -1, MemPct: -1, TempC: -1000, Sessions: -1, Err: "no longer listed by controller " + ctrl.Name})
		}
	}
	p.db.Append("wifi_clients|"+ctrl.ID, now.Unix(), float64(clients))
	p.db.Append("wifi_aps_up|"+ctrl.ID, now.Unix(), float64(up))
	p.st.SetWireless(model.Wireless{DeviceID: ctrl.ID, TS: now, Clients: clients, APs: total, APsUp: up, Model: ctrl.Model, Version: ctrl.OSVersion})
}

// walkFortiSDWAN reads the SD-WAN health-check members.
func walkFortiSDWAN(ctx context.Context, c *snmp.Client, now time.Time) ([]model.SDWANLink, bool) {
	rows := map[string]*model.SDWANLink{}
	order := []string{}
	get := func(idx string) *model.SDWANLink {
		l := rows[idx]
		if l == nil {
			l = &model.SDWANLink{TS: now}
			rows[idx] = l
			order = append(order, idx)
		}
		return l
	}
	if !walkTable(ctx, c, oidFgVWLName, func(idx string, v snmp.Value) { get(idx).Name = v.String() }) {
		return nil, false
	}
	walkTable(ctx, c, oidFgVWLIfName, func(idx string, v snmp.Value) { get(idx).Interface = v.String() })
	walkTable(ctx, c, oidFgVWLState, func(idx string, v snmp.Value) {
		l := get(idx)
		l.Up = v.Int == 0
		if l.Up {
			l.State = "alive"
		} else {
			l.State = "dead"
		}
	})
	num := func(v snmp.Value) float64 {
		if len(v.Bytes) > 0 {
			f, err := strconv.ParseFloat(strings.TrimSpace(v.String()), 64)
			if err == nil {
				return f
			}
		}
		return float64(v.Int)
	}
	walkTable(ctx, c, oidFgVWLLat, func(idx string, v snmp.Value) { get(idx).LatencyMs = num(v) })
	walkTable(ctx, c, oidFgVWLJitter, func(idx string, v snmp.Value) { get(idx).JitterMs = num(v) })
	walkTable(ctx, c, oidFgVWLLoss, func(idx string, v snmp.Value) { get(idx).LossPct = num(v) })
	out := make([]model.SDWANLink, 0, len(order))
	for _, idx := range order {
		l := rows[idx]
		if l.Name == "" {
			l.Name = "health-check " + idx
		}
		out = append(out, *l)
	}
	return out, true
}

// applySDWAN stores the paths, appends metrics and raises path events.
func (p *Poller) applySDWAN(d model.Device, links []model.SDWANLink, now time.Time) {
	for i := range links {
		links[i].DeviceID = d.ID
	}
	prev := p.st.SDWAN(d.ID)
	p.st.SetSDWAN(d.ID, links)
	down, up := model.SDWANChanges(prev, links)
	for _, l := range down {
		p.emitEvent(model.Event{Kind: "sdwan_link_down", DeviceID: d.ID, Source: "snmp", Severity: model.SevMajor, Domain: d.Domain,
			Message: fmt.Sprintf("%s: SD-WAN path %s via %s is %s", d.Name, l.Name, l.Interface, l.State), DedupKey: "sdwan:" + d.ID + ":" + l.Name + "|" + l.Interface})
	}
	for _, l := range up {
		p.emitEvent(model.Event{Kind: "sdwan_link_up", DeviceID: d.ID, Source: "snmp", Severity: model.SevInfo, Domain: d.Domain,
			Message: fmt.Sprintf("%s: SD-WAN path %s via %s is %s again", d.Name, l.Name, l.Interface, l.State), DedupKey: "sdwan:" + d.ID + ":" + l.Name + "|" + l.Interface})
	}
	for _, l := range links {
		key := d.ID + "|" + l.Name + "/" + l.Interface
		upv := 0.0
		if l.Up {
			upv = 1
		}
		p.db.Append("sdwan_up|"+key, now.Unix(), upv)
		if l.Up {
			p.db.Append("sdwan_latency|"+key, now.Unix(), l.LatencyMs)
			p.db.Append("sdwan_jitter|"+key, now.Unix(), l.JitterMs)
			p.db.Append("sdwan_loss|"+key, now.Unix(), l.LossPct)
			if l.LossPct >= 5 || l.LatencyMs >= 250 {
				p.emitEvent(model.Event{Kind: "sdwan_degraded", DeviceID: d.ID, Source: "snmp", Severity: model.SevMinor, Domain: d.Domain,
					Message: fmt.Sprintf("%s: SD-WAN path %s via %s degraded — %.0f ms latency, %.1f%% loss", d.Name, l.Name, l.Interface, l.LatencyMs, l.LossPct), DedupKey: "sdwan_degraded:" + d.ID + ":" + l.Name + "|" + l.Interface})
			}
		}
	}
}
