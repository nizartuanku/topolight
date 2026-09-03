// Package endpoint keeps the "what is plugged in where" table: MAC addresses
// learned from switch forwarding tables (BRIDGE-MIB / Q-BRIDGE-MIB), joined
// with ARP/ND tables from routers and firewalls for the IP, and placed on the
// access port where they most plausibly live.
//
// Placement: a MAC is usually seen on several switches at once — on the
// access port of the edge switch and on the uplink ports of everything
// upstream. Ports with an LLDP/CDP neighbour or a monitored device's chassis
// MAC are uplinks and never count; among the rest, the port with the fewest
// learned MACs wins (an uplink that LLDP missed still carries hundreds of
// addresses, an access port a handful).
package endpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/oui"
)

// Endpoint is one MAC address and everything known about it.
type Endpoint struct {
	MAC       string    `json:"mac"`
	Vendor    string    `json:"vendor,omitempty"`
	IPs       []string  `json:"ips,omitempty"` // newest first, at most 4
	DeviceID  string    `json:"device_id,omitempty"`
	IfIndex   int       `json:"ifindex,omitempty"`
	IfName    string    `json:"if_name,omitempty"`
	VLAN      int       `json:"vlan,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	ARPDevice string    `json:"arp_device,omitempty"` // router that resolved the IP
	ARPSeen   time.Time `json:"arp_seen,omitempty"`
	Moves     int       `json:"moves,omitempty"`
	// Ports is how many switch ports currently see this MAC (derived, not stored).
	Ports int `json:"ports,omitempty"`
}

// FDBEntry is one learned MAC from a switch forwarding table.
type FDBEntry struct {
	MAC     string
	IfIndex int
	IfName  string
	VLAN    int
}

// ARPEntry is one row of an ARP/ND table.
type ARPEntry struct {
	MAC     string
	IP      string
	IfIndex int
}

// Move is reported when a MAC's placement changes.
type Move struct {
	MAC                                string
	FromDevice, FromIf, ToDevice, ToIf string
}

type sighting struct {
	ifIndex int
	ifName  string
	vlan    int
	count   int // MACs on that port at the time
	ts      time.Time
}

// Store is the in-memory table with a JSON snapshot on disk.
type Store struct {
	mu        sync.RWMutex
	path      string
	eps       map[string]*Endpoint
	sight     map[string]map[string]sighting // mac → deviceID → where
	ports     map[string]map[int]int         // deviceID → ifIndex → MAC count
	dirty     bool
	MaxAge    time.Duration // prune after this much silence (default 90 d)
	MaxMACs   int           // hard cap (default 200k)
	Observed  int64         // FDB rows processed (stats)
	ARPRows   int64
	lastFlush time.Time
}

// Open loads <dir>/endpoints.json (missing is fine). dir "" keeps everything in memory.
func Open(dir string) (*Store, error) {
	s := &Store{eps: map[string]*Endpoint{}, sight: map[string]map[string]sighting{}, ports: map[string]map[int]int{}, MaxAge: 90 * 24 * time.Hour, MaxMACs: 200000}
	if dir == "" {
		return s, nil
	}
	s.path = filepath.Join(dir, "endpoints.json")
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var list []*Endpoint
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, e := range list {
		s.eps[e.MAC] = e
	}
	return s, nil
}

// Flush writes the snapshot when something changed (or force).
func (s *Store) Flush(force bool) error {
	s.mu.Lock()
	if s.path == "" || (!s.dirty && !force) || (!force && time.Since(s.lastFlush) < 5*time.Minute) {
		s.mu.Unlock()
		return nil
	}
	list := make([]*Endpoint, 0, len(s.eps))
	for _, e := range s.eps {
		list = append(list, e)
	}
	s.dirty = false
	s.lastFlush = time.Now()
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].MAC < list[j].MAC })
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Close flushes.
func (s *Store) Close() error { return s.Flush(true) }

// NormMAC canonicalises any MAC spelling to aa:bb:cc:dd:ee:ff ("" if not a MAC).
func NormMAC(in string) string {
	var h []byte
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			h = append(h, c)
		case c >= 'A' && c <= 'F':
			h = append(h, c+32)
		}
	}
	if len(h) != 12 {
		return ""
	}
	out := make([]byte, 0, 17)
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, h[i], h[i+1])
	}
	return string(out)
}

// ObserveFDB replaces everything known from one switch with a fresh table.
// uplinks lists ifIndexes that must not host endpoints. Returns placement moves.
func (s *Store) ObserveFDB(deviceID string, rows []FDBEntry, uplinks map[int]bool, now time.Time) []Move {
	counts := map[int]int{}
	for _, r := range rows {
		counts[r.IfIndex]++
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Observed += int64(len(rows))
	// drop the previous sightings of this device
	for mac, m := range s.sight {
		if _, ok := m[deviceID]; ok {
			delete(m, deviceID)
			if len(m) == 0 {
				delete(s.sight, mac)
			}
		}
	}
	s.ports[deviceID] = counts
	touched := map[string]struct{}{}
	for _, r := range rows {
		mac := NormMAC(r.MAC)
		if mac == "" || uplinks[r.IfIndex] {
			continue
		}
		if isMulticast(mac) {
			continue
		}
		m := s.sight[mac]
		if m == nil {
			m = map[string]sighting{}
			s.sight[mac] = m
		}
		// a MAC on several VLANs/ports of one switch: keep the least busy port
		if old, ok := m[deviceID]; !ok || counts[r.IfIndex] < old.count {
			m[deviceID] = sighting{ifIndex: r.IfIndex, ifName: r.IfName, vlan: r.VLAN, count: counts[r.IfIndex], ts: now}
		}
		touched[mac] = struct{}{}
	}
	var moves []Move
	for mac := range touched {
		if mv, ok := s.placeLocked(mac, now); ok {
			moves = append(moves, mv)
		}
	}
	s.dirty = true
	s.capLocked()
	return moves
}

func isMulticast(mac string) bool {
	// low bit of the first octet
	c := mac[1]
	return c == '1' || c == '3' || c == '5' || c == '7' || c == '9' || c == 'b' || c == 'd' || c == 'f'
}

// placeLocked recomputes the best port for a MAC from its current sightings.
func (s *Store) placeLocked(mac string, now time.Time) (Move, bool) {
	var bestDev string
	var best sighting
	for dev, sg := range s.sight[mac] {
		if bestDev == "" || sg.count < best.count || (sg.count == best.count && sg.ts.After(best.ts)) {
			bestDev, best = dev, sg
		}
	}
	e := s.eps[mac]
	if e == nil {
		e = &Endpoint{MAC: mac, Vendor: oui.Vendor(mac), FirstSeen: now}
		s.eps[mac] = e
	}
	prevSeen := e.LastSeen
	e.LastSeen = now
	if bestDev == "" {
		return Move{}, false
	}
	// A change of port is a move only when the old port no longer sees the
	// MAC (a refinement from a trunk to the real access port is not a move)
	// and the old placement was fresh — a laptop that reappears a week later
	// on another desk is not news either.
	changed := e.DeviceID != "" && (e.DeviceID != bestDev || e.IfIndex != best.ifIndex)
	oldStillThere := false
	if sg, ok := s.sight[mac][e.DeviceID]; ok && sg.ifIndex == e.IfIndex {
		oldStillThere = true
	}
	moved := changed && !oldStillThere && now.Sub(prevSeen) < 24*time.Hour
	mv := Move{MAC: mac, FromDevice: e.DeviceID, FromIf: e.IfName, ToDevice: bestDev, ToIf: best.ifName}
	e.DeviceID, e.IfIndex, e.IfName, e.VLAN = bestDev, best.ifIndex, best.ifName, best.vlan
	if moved {
		e.Moves++
	}
	return mv, moved
}

// ObserveARP merges an ARP/ND table from a router or firewall.
func (s *Store) ObserveARP(deviceID string, rows []ARPEntry, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ARPRows += int64(len(rows))
	for _, r := range rows {
		mac := NormMAC(r.MAC)
		if mac == "" || isMulticast(mac) || r.IP == "" {
			continue
		}
		e := s.eps[mac]
		if e == nil {
			e = &Endpoint{MAC: mac, Vendor: oui.Vendor(mac), FirstSeen: now}
			s.eps[mac] = e
		}
		e.LastSeen, e.ARPSeen, e.ARPDevice = now, now, deviceID
		// newest first, no duplicates, at most 4
		ips := []string{r.IP}
		for _, ip := range e.IPs {
			if ip != r.IP && len(ips) < 4 {
				ips = append(ips, ip)
			}
		}
		e.IPs = ips
	}
	s.dirty = true
	s.capLocked()
}

func (s *Store) capLocked() {
	if s.MaxMACs <= 0 || len(s.eps) <= s.MaxMACs {
		return
	}
	// drop the longest-silent entries
	list := make([]*Endpoint, 0, len(s.eps))
	for _, e := range s.eps {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].LastSeen.Before(list[j].LastSeen) })
	for _, e := range list[:len(list)-s.MaxMACs] {
		delete(s.eps, e.MAC)
		delete(s.sight, e.MAC)
	}
}

// Prune drops endpoints silent for longer than MaxAge (or keep, when > 0).
func (s *Store) Prune(keep time.Duration, now time.Time) int {
	if keep <= 0 {
		keep = s.MaxAge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for mac, e := range s.eps {
		if now.Sub(e.LastSeen) > keep {
			delete(s.eps, mac)
			delete(s.sight, mac)
			n++
		}
	}
	if n > 0 {
		s.dirty = true
	}
	return n
}

// Forget removes everything learned from a deleted device.
func (s *Store) Forget(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ports, deviceID)
	for mac, m := range s.sight {
		delete(m, deviceID)
		if len(m) == 0 {
			delete(s.sight, mac)
		}
	}
	for _, e := range s.eps {
		if e.DeviceID == deviceID {
			e.DeviceID, e.IfIndex, e.IfName, e.VLAN = "", 0, "", 0
		}
		if e.ARPDevice == deviceID {
			e.ARPDevice = ""
		}
	}
	s.dirty = true
}

// Query returns endpoints matching q (MAC, IP, vendor or port substring),
// optionally restricted to one device / ifIndex, newest first.
func (s *Store) Query(q, deviceID string, ifIndex int, limit int) []Endpoint {
	q = strings.ToLower(strings.TrimSpace(q))
	qmac := NormMAC(q)
	qh := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			return r
		}
		return -1
	}, q)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Endpoint, 0, 256)
	for _, e := range s.eps {
		if deviceID != "" && e.DeviceID != deviceID && e.ARPDevice != deviceID {
			continue
		}
		if ifIndex > 0 && !(e.DeviceID == deviceID && e.IfIndex == ifIndex) {
			continue
		}
		if q != "" && !matches(e, q, qmac, qh) {
			continue
		}
		x := *e
		x.Ports = len(s.sight[e.MAC])
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].MAC < out[j].MAC
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func matches(e *Endpoint, q, qmac, qh string) bool {
	if qmac != "" && e.MAC == qmac {
		return true
	}
	if len(qh) >= 4 && strings.Contains(strings.ReplaceAll(e.MAC, ":", ""), qh) && looksLikeMAC(q) {
		return true
	}
	for _, ip := range e.IPs {
		if strings.HasPrefix(ip, q) {
			return true
		}
	}
	if strings.Contains(strings.ToLower(e.Vendor), q) || (e.IfName != "" && strings.Contains(strings.ToLower(e.IfName), q)) {
		return true
	}
	return false
}

func looksLikeMAC(q string) bool {
	for i := 0; i < len(q); i++ {
		c := q[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == ':' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// Get returns one endpoint.
func (s *Store) Get(mac string) (Endpoint, bool) {
	mac = NormMAC(mac)
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.eps[mac]
	if !ok {
		return Endpoint{}, false
	}
	x := *e
	x.Ports = len(s.sight[mac])
	return x, true
}

// PortCounts is ifIndex → number of endpoints placed on that port of a device.
func (s *Store) PortCounts(deviceID string) map[int]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[int]int{}
	for _, e := range s.eps {
		if e.DeviceID == deviceID {
			out[e.IfIndex]++
		}
	}
	return out
}

// Stats for Admin → System.
func (s *Store) Stats(now time.Time) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	withIP, placed, day := 0, 0, 0
	for _, e := range s.eps {
		if len(e.IPs) > 0 {
			withIP++
		}
		if e.DeviceID != "" {
			placed++
		}
		if now.Sub(e.LastSeen) < 24*time.Hour {
			day++
		}
	}
	return map[string]any{"endpoints": len(s.eps), "with_ip": withIP, "placed": placed, "seen_24h": day, "fdb_rows": s.Observed, "arp_rows": s.ARPRows, "oui_entries": oui.Size()}
}
