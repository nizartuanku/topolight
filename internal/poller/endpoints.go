package poller

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/nizartuanku/topolight/internal/endpoint"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/snmp"
)

// OIDs for the endpoint tables.
const (
	oidDot1dBasePortIfIndex = "1.3.6.1.2.1.17.1.4.1.2"     // BRIDGE-MIB: bridge port → ifIndex
	oidDot1dTpFdbPort       = "1.3.6.1.2.1.17.4.3.1.2"     // BRIDGE-MIB: mac → bridge port
	oidDot1dTpFdbStatus     = "1.3.6.1.2.1.17.4.3.1.3"     // 3 learned
	oidDot1qTpFdbPort       = "1.3.6.1.2.1.17.7.1.2.2.1.2" // Q-BRIDGE-MIB: vlan.mac → bridge port
	oidDot1qTpFdbStatus     = "1.3.6.1.2.1.17.7.1.2.2.1.3"
	oidIPNetToMediaPhys     = "1.3.6.1.2.1.4.22.1.2"         // ifIndex.ip → mac
	oidIPNetToMediaType     = "1.3.6.1.2.1.4.22.1.4"         // 2 invalid
	oidIPNetToPhysicalPhys  = "1.3.6.1.2.1.4.35.1.4"         // ifIndex.addrType.len.addr → mac (IPv4 + IPv6)
	oidVtpVlanState         = "1.3.6.1.4.1.9.9.46.1.3.1.1.2" // Cisco: 1.<vlan> → 1 operational
)

// pollEndpoints walks the forwarding and ARP tables of one device and feeds
// the endpoint store. Bounded to 30 s per device; failures are silent (many
// devices simply have no bridge tables).
func (p *Poller) pollEndpoints(ctx context.Context, c *snmp.Client, d model.Device, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p.mu.Lock()
	p.EPWalks++
	p.mu.Unlock()

	ifs := p.st.Interfaces(d.ID)
	nameOf := map[int]string{}
	idxOf := map[string]int{}
	for _, i := range ifs {
		nameOf[i.Index] = i.Name
		idxOf[i.Name] = i.Index
	}
	// uplinks: ports with an LLDP/CDP neighbour or a link in the topology
	uplinks := map[int]bool{}
	for _, n := range p.st.Neighbors(d.ID) {
		if ix, ok := idxOf[n.LocalIf]; ok {
			uplinks[ix] = true
		}
	}
	for _, l := range p.st.Links() {
		if l.ADevice == d.ID {
			if ix, ok := idxOf[l.AIf]; ok {
				uplinks[ix] = true
			}
		}
		if l.BDevice == d.ID {
			if ix, ok := idxOf[l.BIf]; ok {
				uplinks[ix] = true
			}
		}
	}
	// chassis MACs of monitored devices: a port that learns one is an uplink
	chassis := map[string]bool{}
	for _, o := range p.st.Devices() {
		if m := endpoint.NormMAC(o.ChassisMAC); m != "" && o.ID != d.ID {
			chassis[m] = true
		}
	}

	rows := p.fdb(ctx, c, d, nameOf)
	for _, r := range rows {
		if chassis[endpoint.NormMAC(r.MAC)] {
			uplinks[r.IfIndex] = true
		}
	}
	if len(rows) > 0 {
		for _, mv := range p.Endpoints.ObserveFDB(d.ID, rows, uplinks, now) {
			from := mv.FromIf
			if mv.FromDevice != d.ID {
				if od, err := p.st.Device(mv.FromDevice); err == nil {
					from = od.Name + " " + mv.FromIf
				}
			}
			p.emitEvent(model.Event{Kind: "endpoint_moved", DeviceID: d.ID, Source: "snmp", Severity: model.SevInfo, Domain: d.Domain,
				Message: fmt.Sprintf("%s moved from %s to %s %s", mv.MAC, from, d.Name, mv.ToIf), Attrs: map[string]string{"mac": mv.MAC, "from": from, "to": mv.ToIf}, DedupKey: "endpoint_moved:" + mv.MAC})
		}
	}
	if arp := arpTable(ctx, c); len(arp) > 0 {
		p.Endpoints.ObserveARP(d.ID, arp, now)
	}
}

// fdb collects learned MACs: Q-BRIDGE first, then BRIDGE-MIB, then (Cisco
// IOS) BRIDGE-MIB per VLAN through community indexing / v3 contexts.
func (p *Poller) fdb(ctx context.Context, c *snmp.Client, d model.Device, nameOf map[int]string) []endpoint.FDBEntry {
	portIf := bridgePorts(ctx, c)
	rows := qbridge(ctx, c, portIf, nameOf)
	if len(rows) == 0 {
		rows = bridge(ctx, c, portIf, nameOf, 0)
	}
	if len(rows) == 0 && strings.HasPrefix(d.ProfileID, "cisco-ios") {
		rows = p.ciscoPerVLAN(ctx, c, d, nameOf)
	}
	return rows
}

func bridgePorts(ctx context.Context, c *snmp.Client) map[int]int {
	out := map[int]int{}
	vbs, err := c.WalkContext(ctx, oidDot1dBasePortIfIndex)
	if err != nil {
		return out
	}
	for _, vb := range vbs {
		if port, err := strconv.Atoi(snmp.OIDSuffix(vb.OID, oidDot1dBasePortIfIndex)); err == nil {
			out[port] = int(vb.Value.Int)
		}
	}
	return out
}

func ifIndexFor(portIf map[int]int, port int) int {
	if ix, ok := portIf[port]; ok && ix > 0 {
		return ix
	}
	return port // agents without dot1dBasePortTable number ports by ifIndex
}

// macFromIndex turns the trailing 6 sub-identifiers of an OID into a MAC.
func macFromIndex(parts []string) (string, bool) {
	if len(parts) < 6 {
		return "", false
	}
	parts = parts[len(parts)-6:]
	b := make([]byte, 0, 17)
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 255 {
			return "", false
		}
		if i > 0 {
			b = append(b, ':')
		}
		b = append(b, "0123456789abcdef"[n>>4], "0123456789abcdef"[n&15])
	}
	return string(b), true
}

func qbridge(ctx context.Context, c *snmp.Client, portIf map[int]int, nameOf map[int]string) []endpoint.FDBEntry {
	vbs, err := c.WalkContext(ctx, oidDot1qTpFdbPort)
	if err != nil || len(vbs) == 0 {
		return nil
	}
	status := map[string]int64{}
	if svbs, err := c.WalkContext(ctx, oidDot1qTpFdbStatus); err == nil {
		for _, vb := range svbs {
			status[snmp.OIDSuffix(vb.OID, oidDot1qTpFdbStatus)] = vb.Value.Int
		}
	}
	var out []endpoint.FDBEntry
	for _, vb := range vbs {
		suf := snmp.OIDSuffix(vb.OID, oidDot1qTpFdbPort)
		if st, ok := status[suf]; ok && st != 3 { // learned only
			continue
		}
		parts := strings.Split(suf, ".")
		if len(parts) != 7 {
			continue
		}
		vlan, _ := strconv.Atoi(parts[0])
		mac, ok := macFromIndex(parts[1:])
		port := int(vb.Value.Int)
		if !ok || port <= 0 {
			continue
		}
		ix := ifIndexFor(portIf, port)
		out = append(out, endpoint.FDBEntry{MAC: mac, IfIndex: ix, IfName: nameOf[ix], VLAN: vlan})
	}
	return out
}

func bridge(ctx context.Context, c *snmp.Client, portIf map[int]int, nameOf map[int]string, vlan int) []endpoint.FDBEntry {
	vbs, err := c.WalkContext(ctx, oidDot1dTpFdbPort)
	if err != nil || len(vbs) == 0 {
		return nil
	}
	status := map[string]int64{}
	if svbs, err := c.WalkContext(ctx, oidDot1dTpFdbStatus); err == nil {
		for _, vb := range svbs {
			status[snmp.OIDSuffix(vb.OID, oidDot1dTpFdbStatus)] = vb.Value.Int
		}
	}
	var out []endpoint.FDBEntry
	for _, vb := range vbs {
		suf := snmp.OIDSuffix(vb.OID, oidDot1dTpFdbPort)
		if st, ok := status[suf]; ok && st != 3 {
			continue
		}
		mac, ok := macFromIndex(strings.Split(suf, "."))
		port := int(vb.Value.Int)
		if !ok || port <= 0 {
			continue
		}
		ix := ifIndexFor(portIf, port)
		out = append(out, endpoint.FDBEntry{MAC: mac, IfIndex: ix, IfName: nameOf[ix], VLAN: vlan})
	}
	return out
}

// ciscoPerVLAN walks BRIDGE-MIB once per operational VLAN using community
// indexing (v2c: community@vlan) or the vlan-N context (v3). Capped at 64
// VLANs so a big trunk switch does not take minutes.
func (p *Poller) ciscoPerVLAN(ctx context.Context, c *snmp.Client, d model.Device, nameOf map[int]string) []endpoint.FDBEntry {
	vbs, err := c.WalkContext(ctx, oidVtpVlanState)
	if err != nil {
		return nil
	}
	var vlans []int
	for _, vb := range vbs {
		parts := strings.Split(snmp.OIDSuffix(vb.OID, oidVtpVlanState), ".")
		if len(parts) != 2 || vb.Value.Int != 1 {
			continue
		}
		v, _ := strconv.Atoi(parts[1])
		if v <= 0 || (v >= 1002 && v <= 1005) || v > 4094 {
			continue
		}
		vlans = append(vlans, v)
	}
	if len(vlans) > 64 {
		vlans = vlans[:64]
	}
	cred, err := p.credFor(d)
	if err != nil {
		return nil
	}
	var out []endpoint.FDBEntry
	for _, v := range vlans {
		if ctx.Err() != nil {
			break
		}
		vc := NewClient(d.IP, cred)
		if vc.Version == snmp.V3 {
			vc.ContextName = "vlan-" + strconv.Itoa(v)
		} else {
			vc.Community = cred.Community + "@" + strconv.Itoa(v)
		}
		portIf := bridgePorts(ctx, vc)
		out = append(out, bridge(ctx, vc, portIf, nameOf, v)...)
		vc.Close()
	}
	return out
}

// arpTable reads ipNetToMediaTable (IPv4) and ipNetToPhysicalTable (IPv4+IPv6).
func arpTable(ctx context.Context, c *snmp.Client) []endpoint.ARPEntry {
	var out []endpoint.ARPEntry
	seen := map[string]bool{}
	if vbs, err := c.WalkContext(ctx, oidIPNetToMediaPhys); err == nil {
		types := map[string]int64{}
		if tv, err := c.WalkContext(ctx, oidIPNetToMediaType); err == nil {
			for _, vb := range tv {
				types[snmp.OIDSuffix(vb.OID, oidIPNetToMediaType)] = vb.Value.Int
			}
		}
		for _, vb := range vbs {
			suf := snmp.OIDSuffix(vb.OID, oidIPNetToMediaPhys)
			if types[suf] == 2 || len(vb.Value.Bytes) != 6 {
				continue
			}
			parts := strings.Split(suf, ".")
			if len(parts) != 5 {
				continue
			}
			ix, _ := strconv.Atoi(parts[0])
			ip := strings.Join(parts[1:], ".")
			mac := net.HardwareAddr(vb.Value.Bytes).String()
			if !seen[mac+ip] {
				seen[mac+ip] = true
				out = append(out, endpoint.ARPEntry{MAC: mac, IP: ip, IfIndex: ix})
			}
		}
	}
	if vbs, err := c.WalkContext(ctx, oidIPNetToPhysicalPhys); err == nil {
		for _, vb := range vbs {
			if len(vb.Value.Bytes) != 6 {
				continue
			}
			parts := strings.Split(snmp.OIDSuffix(vb.OID, oidIPNetToPhysicalPhys), ".")
			if len(parts) < 4 {
				continue
			}
			ix, _ := strconv.Atoi(parts[0])
			typ, _ := strconv.Atoi(parts[1])
			n, _ := strconv.Atoi(parts[2])
			if len(parts) != 3+n {
				continue
			}
			raw := make([]byte, n)
			for i := 0; i < n; i++ {
				b, _ := strconv.Atoi(parts[3+i])
				raw[i] = byte(b)
			}
			var ip string
			switch {
			case typ == 1 && n == 4:
				ip = net.IP(raw).String()
			case typ == 2 && n == 16:
				ip = net.IP(raw).String()
				if strings.HasPrefix(ip, "fe80:") { // link-local adds nothing for placement
					continue
				}
			default:
				continue
			}
			mac := net.HardwareAddr(vb.Value.Bytes).String()
			if !seen[mac+ip] {
				seen[mac+ip] = true
				out = append(out, endpoint.ARPEntry{MAC: mac, IP: ip, IfIndex: ix})
			}
		}
	}
	return out
}
