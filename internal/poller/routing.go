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

// Routing / layer-2 OIDs.
const (
	oidIPCidrRouteNumber   = "1.3.6.1.2.1.4.24.3.0"
	oidInetCidrRouteNumber = "1.3.6.1.2.1.4.24.6.0"
	oidBgpLocalAS          = "1.3.6.1.2.1.15.2.0"
	oidBgpIdentifier       = "1.3.6.1.2.1.15.4.0"
	oidBgpPeerState        = "1.3.6.1.2.1.15.3.1.2"          // .peerIP → 1..6
	oidBgpPeerRemoteAS     = "1.3.6.1.2.1.15.3.1.9"          // .peerIP
	oidBgpPeerFsmEstTime   = "1.3.6.1.2.1.15.3.1.16"         // seconds
	oidBgpPeerLastError    = "1.3.6.1.2.1.15.3.1.14"         // 2 octets
	oidCbgpPeer2Accepted   = "1.3.6.1.4.1.9.9.187.1.2.8.1.1" // .type.len.addr.afi.safi (CISCO-BGP4-MIB cbgpPeer2AcceptedPrefixes)
	oidCbgpPeerAccepted    = "1.3.6.1.4.1.9.9.187.1.2.4.1.1" // .peerIP.afi.safi (cbgpPeerAcceptedPrefixes, older)
	oidOspfRouterID        = "1.3.6.1.2.1.14.1.1.0"
	oidOspfNbrRtrID        = "1.3.6.1.2.1.14.10.1.3" // .nbrIP.ifIndex
	oidOspfNbrPriority     = "1.3.6.1.2.1.14.10.1.5"
	oidOspfNbrState        = "1.3.6.1.2.1.14.10.1.6" // 1..8
	oidDot1qVlanStaticName = "1.3.6.1.2.1.17.7.1.4.3.1.1"
	oidDot1qVlanEgress     = "1.3.6.1.2.1.17.7.1.4.3.1.2"
	oidDot1qVlanCurEgress  = "1.3.6.1.2.1.17.7.1.4.2.1.4" // .timeMark.vlan (fallback)
	oidDot1dStpProtocol    = "1.3.6.1.2.1.17.2.1.0"
	oidDot1dStpDesigRoot   = "1.3.6.1.2.1.17.2.5.0"
	oidDot1dStpRootCost    = "1.3.6.1.2.1.17.2.6.0"
	oidDot1dStpRootPort    = "1.3.6.1.2.1.17.2.7.0"
	oidDot1dStpTopChanges  = "1.3.6.1.2.1.17.2.4.0"
	oidDot1dStpTimeSince   = "1.3.6.1.2.1.17.2.3.0" // TimeTicks
	oidDot1dBaseBridgeAddr = "1.3.6.1.2.1.17.1.1.0"
	oidDot1dStpPortState   = "1.3.6.1.2.1.17.2.15.1.3"         // .port → 1 disabled 2 blocking 3 listening 4 learning 5 forwarding 6 broken
	oidLagPortAttached     = "1.2.840.10006.300.43.1.2.1.1.13" // dot3adAggPortAttachedAggID .ifIndex → aggregator ifIndex
)

var bgpStates = []string{"", "idle", "connect", "active", "opensent", "openconfirm", "established"}
var ospfStates = []string{"", "down", "attempt", "init", "twoWay", "exchangeStart", "exchange", "loading", "full"}

// pollRouting walks BGP, OSPF, VLAN, STP and LAG tables and raises events on
// peer and topology changes. Called on the 5-minute slow cycle.
func (p *Poller) pollRouting(ctx context.Context, c *snmp.Client, d model.Device, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	prev, hadPrev := p.st.Routing(d.ID)
	r := model.Routing{DeviceID: d.ID, TS: now}
	ifs := p.st.Interfaces(d.ID)
	nameOf := map[int]string{}
	for _, i := range ifs {
		nameOf[i.Index] = i.Name
	}
	portIf := bridgePorts(ctx, c)

	// scalars
	if vbs, err := c.Get(oidInetCidrRouteNumber, oidIPCidrRouteNumber, oidBgpLocalAS, oidBgpIdentifier, oidOspfRouterID); err == nil {
		for _, vb := range vbs {
			switch vb.OID {
			case oidInetCidrRouteNumber:
				if vb.Value.Int > 0 {
					r.Routes = int(vb.Value.Int)
				}
			case oidIPCidrRouteNumber:
				if r.Routes == 0 && vb.Value.Int > 0 {
					r.Routes = int(vb.Value.Int)
				}
			case oidBgpLocalAS:
				r.LocalAS = vb.Value.Int
			case oidBgpIdentifier:
				if len(vb.Value.Bytes) == 4 {
					r.RouterID = net.IP(vb.Value.Bytes).String()
				}
			case oidOspfRouterID:
				if r.RouterID == "" && len(vb.Value.Bytes) == 4 {
					r.RouterID = net.IP(vb.Value.Bytes).String()
				}
			}
		}
	}

	// BGP
	if vbs, err := c.WalkContext(ctx, oidBgpPeerState); err == nil && len(vbs) > 0 {
		peers := map[string]*model.BGPPeer{}
		var order []string
		for _, vb := range vbs {
			ip := snmp.OIDSuffix(vb.OID, oidBgpPeerState)
			st := int(vb.Value.Int)
			pr := &model.BGPPeer{Peer: ip, Up: st == 6}
			if st >= 1 && st <= 6 {
				pr.State = bgpStates[st]
			}
			peers[ip] = pr
			order = append(order, ip)
		}
		col := func(oid string, f func(pr *model.BGPPeer, v snmp.Value)) {
			if vbs, err := c.WalkContext(ctx, oid); err == nil {
				for _, vb := range vbs {
					if pr := peers[snmp.OIDSuffix(vb.OID, oid)]; pr != nil {
						f(pr, vb.Value)
					}
				}
			}
		}
		col(oidBgpPeerRemoteAS, func(pr *model.BGPPeer, v snmp.Value) { pr.RemoteAS = v.Int })
		col(oidBgpPeerFsmEstTime, func(pr *model.BGPPeer, v snmp.Value) { pr.UptimeS = v.Int })
		col(oidBgpPeerLastError, func(pr *model.BGPPeer, v snmp.Value) {
			if len(v.Bytes) == 2 && (v.Bytes[0] != 0 || v.Bytes[1] != 0) {
				pr.LastError = bgpNotification(v.Bytes[0], v.Bytes[1])
			}
		})
		// accepted prefixes (Cisco). cbgpPeer2: index type(1).len(4).a.b.c.d.afi.safi
		if vbs, err := c.WalkContext(ctx, oidCbgpPeer2Accepted); err == nil && len(vbs) > 0 {
			for _, vb := range vbs {
				parts := strings.Split(snmp.OIDSuffix(vb.OID, oidCbgpPeer2Accepted), ".")
				if len(parts) >= 8 && parts[0] == "1" && parts[1] == "4" {
					ip := strings.Join(parts[2:6], ".")
					if pr := peers[ip]; pr != nil && parts[6] == "1" { // ipv4 afi
						pr.Prefixes += vb.Value.Int
					}
				}
			}
		} else if vbs, err := c.WalkContext(ctx, oidCbgpPeerAccepted); err == nil {
			for _, vb := range vbs {
				parts := strings.Split(snmp.OIDSuffix(vb.OID, oidCbgpPeerAccepted), ".")
				if len(parts) >= 6 {
					if pr := peers[strings.Join(parts[:4], ".")]; pr != nil {
						pr.Prefixes += vb.Value.Int
					}
				}
			}
		}
		for _, ip := range order {
			r.BGP = append(r.BGP, *peers[ip])
		}
	}

	// OSPF
	if vbs, err := c.WalkContext(ctx, oidOspfNbrState); err == nil && len(vbs) > 0 {
		nbrs := map[string]*model.OSPFNbr{}
		var order []string
		for _, vb := range vbs {
			parts := strings.Split(snmp.OIDSuffix(vb.OID, oidOspfNbrState), ".")
			if len(parts) < 5 {
				continue
			}
			ip := strings.Join(parts[:4], ".")
			st := int(vb.Value.Int)
			n := &model.OSPFNbr{Neighbor: ip, Full: st == 8}
			if st >= 1 && st <= 8 {
				n.State = ospfStates[st]
			}
			nbrs[snmp.OIDSuffix(vb.OID, oidOspfNbrState)] = n
			order = append(order, snmp.OIDSuffix(vb.OID, oidOspfNbrState))
		}
		if vbs, err := c.WalkContext(ctx, oidOspfNbrRtrID); err == nil {
			for _, vb := range vbs {
				if n := nbrs[snmp.OIDSuffix(vb.OID, oidOspfNbrRtrID)]; n != nil && len(vb.Value.Bytes) == 4 {
					n.RouterID = net.IP(vb.Value.Bytes).String()
				}
			}
		}
		if vbs, err := c.WalkContext(ctx, oidOspfNbrPriority); err == nil {
			for _, vb := range vbs {
				if n := nbrs[snmp.OIDSuffix(vb.OID, oidOspfNbrPriority)]; n != nil {
					n.Priority = int(vb.Value.Int)
				}
			}
		}
		for _, k := range order {
			r.OSPF = append(r.OSPF, *nbrs[k])
		}
	}

	// VLANs (Q-BRIDGE static table; names + egress port bitmaps)
	if vbs, err := c.WalkContext(ctx, oidDot1qVlanStaticName); err == nil && len(vbs) > 0 {
		egress := map[string][]byte{}
		if evbs, err := c.WalkContext(ctx, oidDot1qVlanEgress); err == nil {
			for _, vb := range evbs {
				egress[snmp.OIDSuffix(vb.OID, oidDot1qVlanEgress)] = vb.Value.Bytes
			}
		}
		for _, vb := range vbs {
			idx := snmp.OIDSuffix(vb.OID, oidDot1qVlanStaticName)
			id, _ := strconv.Atoi(idx)
			v := model.VLAN{ID: id, Name: snmp.PrintableOrHex(vb.Value.Bytes)}
			ports := portList(egress[idx])
			v.NPort = len(ports)
			for i, bp := range ports {
				if i >= 64 {
					break
				}
				ix := ifIndexFor(portIf, bp)
				if n := nameOf[ix]; n != "" {
					v.Ports = append(v.Ports, n)
				} else {
					v.Ports = append(v.Ports, "port "+strconv.Itoa(bp))
				}
			}
			r.VLANs = append(r.VLANs, v)
		}
		sort.Slice(r.VLANs, func(i, j int) bool { return r.VLANs[i].ID < r.VLANs[j].ID })
		if len(r.VLANs) > 512 {
			r.VLANs = r.VLANs[:512]
		}
	}

	// STP
	if vbs, err := c.Get(oidDot1dStpProtocol, oidDot1dStpDesigRoot, oidDot1dStpRootCost, oidDot1dStpRootPort, oidDot1dStpTopChanges, oidDot1dStpTimeSince, oidDot1dBaseBridgeAddr); err == nil {
		stp := &model.STP{}
		var rootPort int
		var bridgeMAC []byte
		got := false
		for _, vb := range vbs {
			if vb.Value.Exception() {
				continue
			}
			switch vb.OID {
			case oidDot1dStpProtocol:
				stp.Protocol = map[int64]string{1: "unknown", 2: "decLb100", 3: "ieee8021d"}[vb.Value.Int]
				got = true
			case oidDot1dStpDesigRoot:
				stp.RootID = bridgeID(vb.Value.Bytes)
				got = true
			case oidDot1dStpRootCost:
				stp.RootCost = vb.Value.Int
			case oidDot1dStpRootPort:
				rootPort = int(vb.Value.Int)
			case oidDot1dStpTopChanges:
				stp.TopChanges = vb.Value.Int
			case oidDot1dStpTimeSince:
				stp.LastChangeS = vb.Value.Int / 100
			case oidDot1dBaseBridgeAddr:
				bridgeMAC = vb.Value.Bytes
			}
		}
		if got && stp.RootID != "" {
			if len(bridgeMAC) == 6 && len(stp.RootID) >= 17 && strings.HasSuffix(stp.RootID, snmp.MACString(bridgeMAC)) {
				stp.IsRoot = true
			}
			if rootPort > 0 {
				stp.RootPort = nameOf[ifIndexFor(portIf, rootPort)]
				if stp.RootPort == "" {
					stp.RootPort = "port " + strconv.Itoa(rootPort)
				}
			}
			if svbs, err := c.WalkContext(ctx, oidDot1dStpPortState); err == nil {
				for _, vb := range svbs {
					port, _ := strconv.Atoi(snmp.OIDSuffix(vb.OID, oidDot1dStpPortState))
					switch vb.Value.Int {
					case 5:
						stp.Forwarding++
					case 2, 3, 4:
						stp.Blocking++
						if len(stp.BlockedPorts) < 32 {
							n := nameOf[ifIndexFor(portIf, port)]
							if n == "" {
								n = "port " + strconv.Itoa(port)
							}
							stp.BlockedPorts = append(stp.BlockedPorts, n)
						}
					}
				}
			}
			r.STP = stp
		}
	}

	// LAG membership
	if vbs, err := c.WalkContext(ctx, oidLagPortAttached); err == nil && len(vbs) > 0 {
		groups := map[int][]int{}
		for _, vb := range vbs {
			member, _ := strconv.Atoi(snmp.OIDSuffix(vb.OID, oidLagPortAttached))
			agg := int(vb.Value.Int)
			if agg > 0 && member > 0 && agg != member {
				groups[agg] = append(groups[agg], member)
			}
		}
		operUp := map[int]bool{}
		for _, i := range ifs {
			operUp[i.Index] = i.OperUp
		}
		var aggs []int
		for a := range groups {
			aggs = append(aggs, a)
		}
		sort.Ints(aggs)
		for _, a := range aggs {
			lag := model.LAG{Name: nameOf[a]}
			if lag.Name == "" {
				lag.Name = "ifIndex " + strconv.Itoa(a)
			}
			sort.Ints(groups[a])
			for _, m := range groups[a] {
				n := nameOf[m]
				if n == "" {
					n = "ifIndex " + strconv.Itoa(m)
				}
				lag.Members = append(lag.Members, n)
				if operUp[m] {
					lag.Up++
				}
			}
			r.LAGs = append(r.LAGs, lag)
		}
	}

	if len(r.BGP) == 0 && len(r.OSPF) == 0 && len(r.VLANs) == 0 && r.STP == nil && len(r.LAGs) == 0 && r.Routes == 0 {
		return // nothing routing/L2 about this device; keep the store small
	}
	p.st.SetRouting(r)
	if hadPrev {
		p.routingEvents(d, prev, r, now)
	}
}

// routingEvents compares two walks and raises the protocol events.
func (p *Poller) routingEvents(d model.Device, prev, cur model.Routing, now time.Time) {
	was := map[string]model.BGPPeer{}
	for _, b := range prev.BGP {
		was[b.Peer] = b
	}
	for _, b := range cur.BGP {
		w, ok := was[b.Peer]
		key := "bgp:" + d.ID + ":" + b.Peer
		switch {
		case ok && w.Up && !b.Up:
			p.emitEvent(model.Event{Kind: "bgp_neighbor_down", DeviceID: d.ID, Source: "snmp", Severity: model.SevMajor, Domain: d.Domain,
				Message: fmt.Sprintf("%s: BGP peer %s (AS %d) left Established — now %s%s", d.Name, b.Peer, b.RemoteAS, b.State, errSuffix(b.LastError)), DedupKey: key, Attrs: map[string]string{"peer": b.Peer, "as": strconv.FormatInt(b.RemoteAS, 10)}})
		case ok && !w.Up && b.Up:
			p.emitEvent(model.Event{Kind: "bgp_neighbor_up", DeviceID: d.ID, Source: "snmp", Severity: model.SevInfo, Domain: d.Domain,
				Message: fmt.Sprintf("%s: BGP peer %s (AS %d) Established, %d prefixes", d.Name, b.Peer, b.RemoteAS, b.Prefixes), DedupKey: key})
		case ok && w.Up && b.Up && w.Prefixes > 0 && b.Prefixes*4 < w.Prefixes:
			p.emitEvent(model.Event{Kind: "bgp_prefixes_dropped", DeviceID: d.ID, Source: "snmp", Severity: model.SevMinor, Domain: d.Domain,
				Message: fmt.Sprintf("%s: BGP peer %s now sends %d prefixes (was %d)", d.Name, b.Peer, b.Prefixes, w.Prefixes), DedupKey: "bgp_prefixes:" + d.ID + ":" + b.Peer})
		}
	}
	for peer, w := range was {
		found := false
		for _, b := range cur.BGP {
			if b.Peer == peer {
				found = true
			}
		}
		if !found && w.Up {
			p.emitEvent(model.Event{Kind: "bgp_neighbor_down", DeviceID: d.ID, Source: "snmp", Severity: model.SevMajor, Domain: d.Domain,
				Message: fmt.Sprintf("%s: BGP peer %s (AS %d) disappeared from the peer table", d.Name, peer, w.RemoteAS), DedupKey: "bgp:" + d.ID + ":" + peer})
		}
	}
	// OSPF: any full neighbour that is no longer full
	wasFull := map[string]bool{}
	for _, n := range prev.OSPF {
		wasFull[n.Neighbor] = n.Full
	}
	lost, back := 0, 0
	for _, n := range cur.OSPF {
		if wasFull[n.Neighbor] && !n.Full {
			lost++
			p.emitEvent(model.Event{Kind: "ospf_adjacency_down", DeviceID: d.ID, Source: "snmp", Severity: model.SevMajor, Domain: d.Domain,
				Message: fmt.Sprintf("%s: OSPF neighbour %s (router-id %s) left Full — now %s", d.Name, n.Neighbor, n.RouterID, n.State), DedupKey: "ospf:" + d.ID})
		}
		if f, ok := wasFull[n.Neighbor]; ok && !f && n.Full {
			back++
		}
	}
	for nb, f := range wasFull {
		found := false
		for _, n := range cur.OSPF {
			if n.Neighbor == nb {
				found = true
			}
		}
		if !found && f {
			lost++
			p.emitEvent(model.Event{Kind: "ospf_adjacency_down", DeviceID: d.ID, Source: "snmp", Severity: model.SevMajor, Domain: d.Domain,
				Message: fmt.Sprintf("%s: OSPF neighbour %s disappeared", d.Name, nb), DedupKey: "ospf:" + d.ID})
		}
	}
	if lost == 0 && back > 0 {
		allFull := true
		for _, n := range cur.OSPF {
			if !n.Full {
				allFull = false
			}
		}
		if allFull {
			p.emitEvent(model.Event{Kind: "ospf_adjacency_up", DeviceID: d.ID, Source: "snmp", Severity: model.SevInfo, Domain: d.Domain,
				Message: fmt.Sprintf("%s: all %d OSPF neighbours Full again", d.Name, len(cur.OSPF)), DedupKey: "ospf:" + d.ID})
		}
	}
	// STP
	if prev.STP != nil && cur.STP != nil {
		if prev.STP.RootID != "" && cur.STP.RootID != "" && prev.STP.RootID != cur.STP.RootID {
			p.emitEvent(model.Event{Kind: "stp_root_changed", DeviceID: d.ID, Source: "snmp", Severity: model.SevMajor, Domain: d.Domain,
				Message: fmt.Sprintf("%s: spanning-tree root changed from %s to %s%s", d.Name, prev.STP.RootID, cur.STP.RootID, map[bool]string{true: " (this bridge is now root)", false: ""}[cur.STP.IsRoot]), DedupKey: "stp_root:" + d.ID})
		} else if cur.STP.TopChanges > prev.STP.TopChanges {
			p.emitEvent(model.Event{Kind: "stp_topology_change", DeviceID: d.ID, Source: "snmp", Severity: model.SevMinor, Domain: d.Domain,
				Message: fmt.Sprintf("%s: %d spanning-tree topology change(s) in the last 5 minutes (root port %s, %d blocked)", d.Name, cur.STP.TopChanges-prev.STP.TopChanges, cur.STP.RootPort, cur.STP.Blocking), DedupKey: "stp_tc:" + d.ID})
		}
	}
	// LAG members lost
	prevLag := map[string]int{}
	for _, l := range prev.LAGs {
		prevLag[l.Name] = l.Up
	}
	for _, l := range cur.LAGs {
		if w, ok := prevLag[l.Name]; ok && l.Up < w {
			p.emitEvent(model.Event{Kind: "lag_member_down", DeviceID: d.ID, Source: "snmp", Severity: model.SevMinor, Domain: d.Domain,
				Message: fmt.Sprintf("%s: %s has %d of %d members up (was %d)", d.Name, l.Name, l.Up, len(l.Members), w), DedupKey: "lag:" + d.ID + ":" + l.Name})
		} else if ok && l.Up > w && l.Up == len(l.Members) {
			p.emitEvent(model.Event{Kind: "lag_member_up", DeviceID: d.ID, Source: "snmp", Severity: model.SevInfo, Domain: d.Domain,
				Message: fmt.Sprintf("%s: %s back to %d of %d members", d.Name, l.Name, l.Up, len(l.Members)), DedupKey: "lag:" + d.ID + ":" + l.Name})
		}
	}
}

func errSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

// bgpNotification names the last error code/subcode (RFC 4271).
func bgpNotification(code, sub byte) string {
	codes := map[byte]string{1: "message header", 2: "OPEN", 3: "UPDATE", 4: "hold timer expired", 5: "FSM", 6: "cease"}
	cease := map[byte]string{1: "max prefixes reached", 2: "administrative shutdown", 3: "peer deconfigured", 4: "administrative reset", 5: "connection rejected", 6: "other configuration change", 7: "connection collision", 8: "out of resources"}
	s := codes[code]
	if s == "" {
		s = "code " + strconv.Itoa(int(code))
	}
	if code == 6 && cease[sub] != "" {
		return s + ": " + cease[sub]
	}
	if sub != 0 {
		s += "/" + strconv.Itoa(int(sub))
	}
	return s
}

// bridgeID renders the 8-byte STP bridge identifier as "priority/mac".
func bridgeID(b []byte) string {
	if len(b) != 8 {
		return ""
	}
	pri := int(b[0])<<8 | int(b[1])
	return strconv.Itoa(pri) + "/" + snmp.MACString(b[2:])
}

// portList decodes a PortList bitmap (bit 8 of octet 1 = port 1).
func portList(b []byte) []int {
	var out []int
	for i, oct := range b {
		for bit := 0; bit < 8; bit++ {
			if oct&(0x80>>bit) != 0 {
				out = append(out, i*8+bit+1)
			}
		}
	}
	return out
}
