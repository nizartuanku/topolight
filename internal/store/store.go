// Package store keeps the whole object model in memory behind one mutex and
// persists it as an atomic JSON snapshot. Events are journaled to daily JSONL
// files. Every read returns a copy so callers never share slices with the
// store (see Clone methods) — a lesson from the AuditLight race.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
)

// ErrNotFound is returned by Get* when the id is unknown.
var ErrNotFound = errors.New("not found")

type snapshot struct {
	Version     int                            `json:"version"`
	SavedAt     time.Time                      `json:"saved_at"`
	Settings    model.Settings                 `json:"settings"`
	Notify      model.Notify                   `json:"notify"`
	Sites       map[string]model.Site          `json:"sites"`
	Creds       map[string]model.Credential    `json:"creds"`
	Devices     map[string]model.Device        `json:"devices"`
	Interfaces  map[string]model.Interface     `json:"interfaces"`
	Links       map[string]model.Link          `json:"links"`
	Neighbors   map[string][]model.NeighborObs `json:"neighbors"` // by device id
	Alerts      map[string]model.Alert         `json:"alerts"`    // open + acked; resolved archived
	Maintenance map[string]model.Maintenance   `json:"maintenance"`
	Users       map[string]model.User          `json:"users"`
	Tokens      map[string]model.APIToken      `json:"tokens,omitempty"`
	Probes      map[string]model.Probe         `json:"probes,omitempty"`
	Reports     map[string]model.Report        `json:"reports,omitempty"`
	Rules       map[string]model.Rule          `json:"rules"`
	TopoVersion int                            `json:"topo_version"`
	TopoSavedAt time.Time                      `json:"topo_saved_at"`
	Layout      map[string][3]float64          `json:"layout"` // device id -> x,y,z
	Routing     map[string]model.Routing       `json:"routing,omitempty"`
	Integs      map[string]model.Integration   `json:"integrations,omitempty"`
	Wireless    map[string]model.Wireless      `json:"wireless,omitempty"`
	SDWAN       map[string][]model.SDWANLink   `json:"sdwan,omitempty"`
}

// Store is the in-memory model with persistence.
type Store struct {
	mu      sync.RWMutex
	dir     string
	memOnly bool
	s       snapshot
	dirty   bool
	stop    chan struct{}

	evMu     sync.Mutex
	evRing   []model.Event
	evMax    int
	evFile   *os.File
	evDay    string
	evWriter *bufio.Writer
	readOnly bool
}

// Open loads (or initialises) the store in dir. An empty dir keeps everything
// in memory only.
func Open(dir string) (*Store, error) {
	st := &Store{dir: dir, memOnly: dir == "", stop: make(chan struct{}), evMax: 5000}
	st.s = snapshot{Version: 1, Sites: map[string]model.Site{}, Creds: map[string]model.Credential{},
		Devices: map[string]model.Device{}, Interfaces: map[string]model.Interface{}, Links: map[string]model.Link{},
		Neighbors: map[string][]model.NeighborObs{}, Alerts: map[string]model.Alert{}, Maintenance: map[string]model.Maintenance{},
		Users: map[string]model.User{}, Rules: map[string]model.Rule{}, Layout: map[string][3]float64{}, Routing: map[string]model.Routing{}, Integs: map[string]model.Integration{}, Wireless: map[string]model.Wireless{}, SDWAN: map[string][]model.SDWANLink{}}
	st.s.Settings = model.Settings{InstanceName: "TopoLight", DefaultPoll: 60, DiscoveryEvery: 60, TopologyEvery: 30}
	st.s.Notify = model.Notify{MinSeverity: model.SevMinor, GroupSeconds: 60, ResolvedToo: true, CriticalAlways: true}
	if !st.memOnly {
		if err := os.MkdirAll(filepath.Join(dir, "events"), 0o700); err != nil {
			return nil, err
		}
		if err := st.load(); err != nil {
			return nil, err
		}
		if err := st.loadRecentEvents(); err != nil {
			return nil, err
		}
		go st.flushLoop()
	}
	return st, nil
}

// OpenReadOnly loads state.json from dir but never writes back — a cluster
// standby reads the leader's mirrored snapshot this way. Reload re-reads it.
func OpenReadOnly(dir string) (*Store, error) {
	st, err := Open("")
	if err != nil {
		return nil, err
	}
	st.dir, st.memOnly, st.readOnly = dir, true, true
	if err := st.load(); err != nil {
		return nil, err
	}
	return st, nil
}

// Reload re-reads state.json (read-only stores).
func (st *Store) Reload() error {
	if !st.readOnly {
		return nil
	}
	return st.load()
}

func (st *Store) load() error {
	path := filepath.Join(st.dir, "state.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		// Keep a copy of the unreadable file rather than silently starting empty.
		_ = os.Rename(path, path+".corrupt-"+time.Now().Format("20060102-150405"))
		return fmt.Errorf("state.json unreadable (moved aside): %w", err)
	}
	// nil maps from an older snapshot must not panic.
	if s.Sites == nil {
		s.Sites = map[string]model.Site{}
	}
	if s.Creds == nil {
		s.Creds = map[string]model.Credential{}
	}
	if s.Devices == nil {
		s.Devices = map[string]model.Device{}
	}
	if s.Interfaces == nil {
		s.Interfaces = map[string]model.Interface{}
	}
	if s.Links == nil {
		s.Links = map[string]model.Link{}
	}
	if s.Neighbors == nil {
		s.Neighbors = map[string][]model.NeighborObs{}
	}
	if s.Alerts == nil {
		s.Alerts = map[string]model.Alert{}
	}
	if s.Maintenance == nil {
		s.Maintenance = map[string]model.Maintenance{}
	}
	if s.Tokens == nil {
		s.Tokens = map[string]model.APIToken{}
	}
	if s.Probes == nil {
		s.Probes = map[string]model.Probe{}
	}
	if s.Reports == nil {
		s.Reports = map[string]model.Report{}
	}
	if s.Users == nil {
		s.Users = map[string]model.User{}
	}
	if s.Rules == nil {
		s.Rules = map[string]model.Rule{}
	}
	if s.Layout == nil {
		s.Layout = map[string][3]float64{}
	}
	if s.Routing == nil {
		s.Routing = map[string]model.Routing{}
	}
	if s.Integs == nil {
		s.Integs = map[string]model.Integration{}
	}
	if s.Wireless == nil {
		s.Wireless = map[string]model.Wireless{}
	}
	if s.SDWAN == nil {
		s.SDWAN = map[string][]model.SDWANLink{}
	}
	if s.Settings.DefaultPoll == 0 {
		s.Settings.DefaultPoll = 60
	}
	st.mu.Lock()
	st.s = s
	st.mu.Unlock()
	return nil
}

// Save writes the snapshot atomically. Safe to call often; cheap when clean.
func (st *Store) Save() error {
	if st.memOnly {
		return nil
	}
	st.mu.RLock()
	if !st.dirty {
		st.mu.RUnlock()
		return nil
	}
	st.s.SavedAt = time.Now()
	b, err := json.Marshal(&st.s)
	st.mu.RUnlock()
	if err != nil {
		return err
	}
	path := filepath.Join(st.dir, "state.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	st.mu.Lock()
	st.dirty = false
	st.mu.Unlock()
	return nil
}

func (st *Store) flushLoop() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = st.Save()
		case <-st.stop:
			return
		}
	}
}

// Close flushes and stops background work.
func (st *Store) Close() error {
	select {
	case <-st.stop:
	default:
		close(st.stop)
	}
	st.evMu.Lock()
	if st.evWriter != nil {
		_ = st.evWriter.Flush()
	}
	if st.evFile != nil {
		_ = st.evFile.Close()
		st.evFile = nil
	}
	st.evMu.Unlock()
	return st.Save()
}

// Dir returns the data directory ("" when memory-only).
func (st *Store) Dir() string { return st.dir }

func (st *Store) touch() { st.dirty = true }

// ---- settings & notify ----

func (st *Store) Settings() model.Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s.Settings
}

func (st *Store) SetSettings(s model.Settings) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Settings = s
	st.touch()
}

func (st *Store) Notify() model.Notify {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s.Notify.Clone()
}

func (st *Store) SetNotify(n model.Notify) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Notify = n.Clone()
	st.touch()
}

// ---- sites ----

func (st *Store) Sites() []model.Site {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Site, 0, len(st.s.Sites))
	for _, s := range st.s.Sites {
		s.Subnets = append([]string(nil), s.Subnets...)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *Store) Site(id string) (model.Site, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s, ok := st.s.Sites[id]
	if !ok {
		return model.Site{}, ErrNotFound
	}
	s.Subnets = append([]string(nil), s.Subnets...)
	return s, nil
}

func (st *Store) PutSite(s model.Site) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s.Subnets = append([]string(nil), s.Subnets...)
	st.s.Sites[s.ID] = s
	st.touch()
}

func (st *Store) DeleteSite(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Sites, id)
	st.touch()
}

// ---- credentials ----

func (st *Store) Creds() []model.Credential {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Credential, 0, len(st.s.Creds))
	for _, c := range st.s.Creds {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *Store) Cred(id string) (model.Credential, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	c, ok := st.s.Creds[id]
	if !ok {
		return model.Credential{}, ErrNotFound
	}
	return c, nil
}

func (st *Store) PutCred(c model.Credential) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Creds[c.ID] = c
	st.touch()
}

func (st *Store) DeleteCred(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Creds, id)
	st.touch()
}

// ---- devices ----

func cloneDevice(d model.Device) model.Device {
	if d.Metrics != nil {
		m := make(map[string]float64, len(d.Metrics))
		for k, v := range d.Metrics {
			m[k] = v
		}
		d.Metrics = m
	}
	return d
}

func (st *Store) Devices() []model.Device {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Device, 0, len(st.s.Devices))
	for _, d := range st.s.Devices {
		out = append(out, cloneDevice(d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *Store) Device(id string) (model.Device, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	d, ok := st.s.Devices[id]
	if !ok {
		return model.Device{}, ErrNotFound
	}
	return cloneDevice(d), nil
}

// DeviceByIP finds a device by management IP.
func (st *Store) DeviceByIP(ip string) (model.Device, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, d := range st.s.Devices {
		if d.IP == ip {
			return cloneDevice(d), true
		}
	}
	return model.Device{}, false
}

// DeviceCount returns the number of monitored devices.
func (st *Store) DeviceCount() (total, monitored int) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, d := range st.s.Devices {
		total++
		if d.Monitored {
			monitored++
		}
	}
	return
}

func (st *Store) PutDevice(d model.Device) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Devices[d.ID] = cloneDevice(d)
	st.touch()
}

// UpdateDevice applies fn under the lock; fn receives a copy and returns the
// value to store. It is the safe way to change a few fields concurrently.
func (st *Store) UpdateDevice(id string, fn func(*model.Device)) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	d, ok := st.s.Devices[id]
	if !ok {
		return false
	}
	d = cloneDevice(d)
	fn(&d)
	st.s.Devices[id] = d
	st.touch()
	return true
}

func (st *Store) DeleteDevice(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Devices, id)
	delete(st.s.Routing, id)
	delete(st.s.Wireless, id)
	delete(st.s.SDWAN, id)
	for k, i := range st.s.Interfaces {
		if i.DeviceID == id {
			delete(st.s.Interfaces, k)
		}
	}
	for k, l := range st.s.Links {
		if l.ADevice == id || l.BDevice == id {
			delete(st.s.Links, k)
		}
	}
	delete(st.s.Neighbors, id)
	delete(st.s.Layout, id)
	st.touch()
}

// ---- interfaces ----

func (st *Store) Interfaces(deviceID string) []model.Interface {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := []model.Interface{}
	for _, i := range st.s.Interfaces {
		if deviceID == "" || i.DeviceID == deviceID {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceID != out[j].DeviceID {
			return out[i].DeviceID < out[j].DeviceID
		}
		return out[i].Index < out[j].Index
	})
	return out
}

func (st *Store) Interface(id string) (model.Interface, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	i, ok := st.s.Interfaces[id]
	if !ok {
		return model.Interface{}, ErrNotFound
	}
	return i, nil
}

// InterfaceByName finds an interface by device and name (case-insensitive).
func (st *Store) InterfaceByName(deviceID, name string) (model.Interface, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, i := range st.s.Interfaces {
		if i.DeviceID == deviceID && strings.EqualFold(i.Name, name) {
			return i, true
		}
	}
	return model.Interface{}, false
}

func (st *Store) PutInterface(i model.Interface) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Interfaces[i.ID] = i
	st.touch()
}

func (st *Store) UpdateInterface(id string, fn func(*model.Interface)) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	i, ok := st.s.Interfaces[id]
	if !ok {
		return false
	}
	fn(&i)
	st.s.Interfaces[id] = i
	st.touch()
	return true
}

// PutInterfaces replaces the interface set of a device with the given list,
// preserving status fields of interfaces that still exist.
func (st *Store) PutInterfaces(deviceID string, list []model.Interface) {
	st.mu.Lock()
	defer st.mu.Unlock()
	keep := map[string]bool{}
	for _, i := range list {
		if old, ok := st.s.Interfaces[i.ID]; ok {
			i.Status, i.StatusSince, i.Important = old.Status, old.StatusSince, old.Important || i.Important
			i.InBps, i.OutBps, i.InUtil, i.OutUtil, i.InErrRate, i.OutErrRate = old.InBps, old.OutBps, old.InUtil, old.OutUtil, old.InErrRate, old.OutErrRate
			if old.LastChange.After(i.LastChange) {
				i.LastChange = old.LastChange
			}
		}
		st.s.Interfaces[i.ID] = i
		keep[i.ID] = true
	}
	for k, i := range st.s.Interfaces {
		if i.DeviceID == deviceID && !keep[k] {
			delete(st.s.Interfaces, k)
		}
	}
	st.touch()
}

// ---- neighbours & links ----

func (st *Store) Neighbors(deviceID string) []model.NeighborObs {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return append([]model.NeighborObs(nil), st.s.Neighbors[deviceID]...)
}

func (st *Store) AllNeighbors() map[string][]model.NeighborObs {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make(map[string][]model.NeighborObs, len(st.s.Neighbors))
	for k, v := range st.s.Neighbors {
		out[k] = append([]model.NeighborObs(nil), v...)
	}
	return out
}

// ---- integrations, wireless, SD-WAN ----

func (st *Store) Integrations() []model.Integration {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Integration, 0, len(st.s.Integs))
	for _, i := range st.s.Integs {
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *Store) Integration(id string) (model.Integration, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	i, ok := st.s.Integs[id]
	if !ok {
		return model.Integration{}, ErrNotFound
	}
	return i, nil
}

func (st *Store) PutIntegration(i model.Integration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.Integs == nil {
		st.s.Integs = map[string]model.Integration{}
	}
	st.s.Integs[i.ID] = i
	st.touch()
}

func (st *Store) DeleteIntegration(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Integs, id)
	st.touch()
}

func (st *Store) Wireless(deviceID string) (model.Wireless, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	w, ok := st.s.Wireless[deviceID]
	return w, ok
}

func (st *Store) SetWireless(w model.Wireless) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.Wireless == nil {
		st.s.Wireless = map[string]model.Wireless{}
	}
	st.s.Wireless[w.DeviceID] = w
	st.touch()
}

func (st *Store) AllWireless() map[string]model.Wireless {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make(map[string]model.Wireless, len(st.s.Wireless))
	for k, v := range st.s.Wireless {
		out[k] = v
	}
	return out
}

func (st *Store) SDWAN(deviceID string) []model.SDWANLink {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return append([]model.SDWANLink(nil), st.s.SDWAN[deviceID]...)
}

func (st *Store) SetSDWAN(deviceID string, links []model.SDWANLink) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.SDWAN == nil {
		st.s.SDWAN = map[string][]model.SDWANLink{}
	}
	if len(links) == 0 {
		delete(st.s.SDWAN, deviceID)
	} else {
		st.s.SDWAN[deviceID] = append([]model.SDWANLink(nil), links...)
	}
	st.touch()
}

func (st *Store) AllSDWAN() map[string][]model.SDWANLink {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make(map[string][]model.SDWANLink, len(st.s.SDWAN))
	for k, v := range st.s.SDWAN {
		out[k] = append([]model.SDWANLink(nil), v...)
	}
	return out
}

// Routing returns the last routing/L2 walk of a device.
func (st *Store) Routing(deviceID string) (model.Routing, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	r, ok := st.s.Routing[deviceID]
	return r, ok
}

// SetRouting stores a routing/L2 walk.
func (st *Store) SetRouting(r model.Routing) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.Routing == nil {
		st.s.Routing = map[string]model.Routing{}
	}
	st.s.Routing[r.DeviceID] = r
	st.touch()
}

// AllRouting returns every device's routing state.
func (st *Store) AllRouting() map[string]model.Routing {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make(map[string]model.Routing, len(st.s.Routing))
	for k, v := range st.s.Routing {
		out[k] = v
	}
	return out
}

func (st *Store) SetNeighbors(deviceID string, obs []model.NeighborObs) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Neighbors[deviceID] = append([]model.NeighborObs(nil), obs...)
	st.touch()
}

func (st *Store) Links() []model.Link {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Link, 0, len(st.s.Links))
	for _, l := range st.s.Links {
		out = append(out, l.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (st *Store) Link(id string) (model.Link, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	l, ok := st.s.Links[id]
	if !ok {
		return model.Link{}, ErrNotFound
	}
	return l.Clone(), nil
}

func (st *Store) PutLink(l model.Link) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Links[l.ID] = l.Clone()
	st.touch()
}

func (st *Store) UpdateLink(id string, fn func(*model.Link)) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	l, ok := st.s.Links[id]
	if !ok {
		return false
	}
	l = l.Clone()
	fn(&l)
	st.s.Links[id] = l
	st.touch()
	return true
}

func (st *Store) DeleteLink(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Links, id)
	st.touch()
}

// ReplaceLinks swaps the whole computed link set, keeping manual links.
func (st *Store) ReplaceLinks(links []model.Link) {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := map[string]model.Link{}
	for _, l := range st.s.Links {
		if l.Manual {
			n[l.ID] = l
		}
	}
	for _, l := range links {
		if old, ok := st.s.Links[l.ID]; ok {
			if l.FirstSeen.IsZero() {
				l.FirstSeen = old.FirstSeen
			}
		}
		n[l.ID] = l.Clone()
	}
	st.s.Links = n
	st.s.TopoVersion++
	st.s.TopoSavedAt = time.Now()
	st.touch()
}

// TopologyVersion returns the current topology version and time.
func (st *Store) TopologyVersion() (int, time.Time) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s.TopoVersion, st.s.TopoSavedAt
}

func (st *Store) Layout() map[string][3]float64 {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make(map[string][3]float64, len(st.s.Layout))
	for k, v := range st.s.Layout {
		out[k] = v
	}
	return out
}

func (st *Store) SetLayout(m map[string][3]float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Layout = make(map[string][3]float64, len(m))
	for k, v := range m {
		st.s.Layout[k] = v
	}
	st.touch()
}

// ---- alerts ----

func (st *Store) Alerts() []model.Alert {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Alert, 0, len(st.s.Alerts))
	for _, a := range st.s.Alerts {
		out = append(out, a.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.After(out[j].OpenedAt) })
	return out
}

func (st *Store) Alert(id string) (model.Alert, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	a, ok := st.s.Alerts[id]
	if !ok {
		return model.Alert{}, ErrNotFound
	}
	return a.Clone(), nil
}

// AlertByDedup returns the open/acked alert with the dedup key.
func (st *Store) AlertByDedup(key string) (model.Alert, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, a := range st.s.Alerts {
		if a.DedupKey == key && a.State != model.AlertResolved {
			return a.Clone(), true
		}
	}
	return model.Alert{}, false
}

func (st *Store) PutAlert(a model.Alert) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Alerts[a.ID] = a.Clone()
	st.touch()
}

func (st *Store) UpdateAlert(id string, fn func(*model.Alert)) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.s.Alerts[id]
	if !ok {
		return false
	}
	a = a.Clone()
	fn(&a)
	st.s.Alerts[id] = a
	st.touch()
	return true
}

// ArchiveResolved moves alerts resolved longer than keep ago into the journal
// and drops them from the live map.
func (st *Store) ArchiveResolved(keep time.Duration) int {
	st.mu.Lock()
	var old []model.Alert
	for id, a := range st.s.Alerts {
		if a.State == model.AlertResolved && time.Since(a.ResolvedAt) > keep {
			old = append(old, a.Clone())
			delete(st.s.Alerts, id)
		}
	}
	if len(old) > 0 {
		st.touch()
	}
	st.mu.Unlock()
	for _, a := range old {
		st.appendJournal("alerts", a)
	}
	return len(old)
}

// ---- maintenance ----

func (st *Store) Maintenances() []model.Maintenance {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Maintenance, 0, len(st.s.Maintenance))
	for _, m := range st.s.Maintenance {
		out = append(out, m.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From.Before(out[j].From) })
	return out
}

func (st *Store) PutMaintenance(m model.Maintenance) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Maintenance[m.ID] = m.Clone()
	st.touch()
}

func (st *Store) DeleteMaintenance(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Maintenance, id)
	st.touch()
}

// InMaintenance reports whether a device is covered by an active window now.
func (st *Store) InMaintenance(t time.Time, siteID, deviceID string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, m := range st.s.Maintenance {
		if m.Active(t, siteID, deviceID) {
			return true
		}
	}
	return false
}

// ---- users ----

func (st *Store) Users() []model.User {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.User, 0, len(st.s.Users))
	for _, u := range st.s.Users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *Store) UserByName(name string) (model.User, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, u := range st.s.Users {
		if strings.EqualFold(u.Name, name) {
			return u, true
		}
	}
	return model.User{}, false
}

func (st *Store) PutUser(u model.User) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Users[u.ID] = u
	st.touch()
}

func (st *Store) DeleteUser(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Users, id)
	st.touch()
}

// ---- probes ----

func (st *Store) Probes() []model.Probe {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Probe, 0, len(st.s.Probes))
	for _, p := range st.s.Probes {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *Store) Probe(id string) (model.Probe, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	p, ok := st.s.Probes[id]
	if !ok {
		return model.Probe{}, ErrNotFound
	}
	return p, nil
}

func (st *Store) PutProbe(p model.Probe) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Probes[p.ID] = p
	st.touch()
}

func (st *Store) DeleteProbe(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Probes, id)
	st.touch()
}

// ---- reports ----

func (st *Store) Reports() []model.Report {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Report, 0, len(st.s.Reports))
	for _, r := range st.s.Reports {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *Store) Report(id string) (model.Report, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	r, ok := st.s.Reports[id]
	if !ok {
		return model.Report{}, ErrNotFound
	}
	return r, nil
}

func (st *Store) PutReport(r model.Report) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Reports[r.ID] = r
	st.touch()
}

func (st *Store) DeleteReport(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Reports, id)
	st.touch()
}

// ---- API tokens ----

func (st *Store) Tokens() []model.APIToken {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.APIToken, 0, len(st.s.Tokens))
	for _, t := range st.s.Tokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// TokenByHash finds a token by the sha256 of its secret.
func (st *Store) TokenByHash(hash string) (model.APIToken, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, t := range st.s.Tokens {
		if t.Hash == hash {
			return t, true
		}
	}
	return model.APIToken{}, false
}

func (st *Store) PutToken(t model.APIToken) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Tokens[t.ID] = t
	st.touch()
}

func (st *Store) DeleteToken(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.s.Tokens, id)
	st.touch()
}

// TouchToken records use (at most once a minute to keep the snapshot quiet).
func (st *Store) TouchToken(id string, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.s.Tokens[id]
	if !ok || now.Sub(t.LastUsed) < time.Minute {
		return
	}
	t.LastUsed = now
	st.s.Tokens[id] = t
	st.touch()
}

// ---- rules ----

func (st *Store) Rules() []model.Rule {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]model.Rule, 0, len(st.s.Rules))
	for _, r := range st.s.Rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (st *Store) Rule(id string) (model.Rule, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	r, ok := st.s.Rules[id]
	return r, ok
}

func (st *Store) PutRule(r model.Rule) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Rules[r.ID] = r
	st.touch()
}

// ---- events ----

func (st *Store) appendJournal(kind string, v any) {
	if st.memOnly {
		return
	}
	dir := filepath.Join(st.dir, kind)
	_ = os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, time.Now().UTC().Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(v)
	_, _ = f.Write(append(b, '\n'))
}

// AddEvent journals an event and keeps it in the recent ring.
func (st *Store) AddEvent(e model.Event) {
	if e.ID == "" {
		e.ID = model.NewID("evt")
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	st.evMu.Lock()
	st.evRing = append(st.evRing, e.Clone())
	if len(st.evRing) > st.evMax {
		st.evRing = st.evRing[len(st.evRing)-st.evMax:]
	}
	if !st.memOnly {
		day := e.TS.UTC().Format("2006-01-02")
		if st.evFile == nil || st.evDay != day {
			if st.evWriter != nil {
				_ = st.evWriter.Flush()
			}
			if st.evFile != nil {
				_ = st.evFile.Close()
			}
			f, err := os.OpenFile(filepath.Join(st.dir, "events", day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err == nil {
				st.evFile, st.evDay = f, day
				st.evWriter = bufio.NewWriterSize(f, 64<<10)
			}
		}
		if st.evWriter != nil {
			b, _ := json.Marshal(e)
			_, _ = st.evWriter.Write(append(b, '\n'))
			if st.evWriter.Buffered() > 32<<10 {
				_ = st.evWriter.Flush()
			}
		}
	}
	st.evMu.Unlock()
}

// FlushEvents forces buffered event lines to disk.
func (st *Store) FlushEvents() {
	st.evMu.Lock()
	if st.evWriter != nil {
		_ = st.evWriter.Flush()
	}
	st.evMu.Unlock()
}

// RecentEvents returns the newest events (newest first), filtered by device
// when deviceID is non-empty.
func (st *Store) RecentEvents(deviceID string, limit int) []model.Event {
	st.evMu.Lock()
	defer st.evMu.Unlock()
	out := make([]model.Event, 0, limit)
	for i := len(st.evRing) - 1; i >= 0 && len(out) < limit; i-- {
		e := st.evRing[i]
		if deviceID != "" && e.DeviceID != deviceID {
			continue
		}
		out = append(out, e.Clone())
	}
	return out
}

func (st *Store) loadRecentEvents() error {
	dir := filepath.Join(st.dir, "events")
	names, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	sort.Strings(names)
	if len(names) > 3 {
		names = names[len(names)-3:]
	}
	for _, n := range names {
		f, err := os.Open(n)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var e model.Event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				st.evRing = append(st.evRing, e)
			}
		}
		f.Close()
	}
	if len(st.evRing) > st.evMax {
		st.evRing = st.evRing[len(st.evRing)-st.evMax:]
	}
	return nil
}

// AlertsSince returns live alerts plus archived ones whose window touches
// [from, now) — the material for availability and alert reports.
func (st *Store) AlertsSince(from time.Time) []model.Alert {
	out := st.Alerts()
	seen := map[string]bool{}
	for _, a := range out {
		seen[a.ID] = true
	}
	if st.memOnly {
		return out
	}
	names, _ := filepath.Glob(filepath.Join(st.dir, "alerts", "*.jsonl"))
	sort.Strings(names)
	for _, n := range names {
		base := strings.TrimSuffix(filepath.Base(n), ".jsonl")
		if d, err := time.Parse("2006-01-02", base); err == nil && d.Add(24*time.Hour).Before(from.Add(-24*time.Hour)) {
			continue // archived before the window and cannot overlap it (archive happens after resolution)
		}
		f, err := os.Open(n)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var a model.Alert
			if json.Unmarshal(sc.Bytes(), &a) != nil || seen[a.ID] {
				continue
			}
			if !a.ResolvedAt.IsZero() && a.ResolvedAt.Before(from) {
				continue
			}
			seen[a.ID] = true
			out = append(out, a)
		}
		f.Close()
	}
	return out
}

// EventsSince reads events from the journal for the window (oldest first),
// optionally restricted to kinds.
func (st *Store) EventsSince(from time.Time, kinds map[string]bool) []model.Event {
	var out []model.Event
	if st.memOnly {
		for _, e := range st.RecentEvents("", 100000) {
			if !e.TS.Before(from) && (kinds == nil || kinds[e.Kind]) {
				out = append(out, e)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
		return out
	}
	names, _ := filepath.Glob(filepath.Join(st.dir, "events", "*.jsonl"))
	sort.Strings(names)
	for _, n := range names {
		base := strings.TrimSuffix(filepath.Base(n), ".jsonl")
		if d, err := time.Parse("2006-01-02", base); err == nil && d.Add(24*time.Hour).Before(from) {
			continue
		}
		f, err := os.Open(n)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var e model.Event
			if json.Unmarshal(sc.Bytes(), &e) == nil && !e.TS.Before(from) && (kinds == nil || kinds[e.Kind]) {
				out = append(out, e)
			}
		}
		f.Close()
	}
	return out
}

// PruneJournals deletes journal files older than keep.
func (st *Store) PruneJournals(keep time.Duration) int {
	if st.memOnly {
		return 0
	}
	n := 0
	for _, kind := range []string{"events", "alerts"} {
		names, _ := filepath.Glob(filepath.Join(st.dir, kind, "*.jsonl"))
		for _, name := range names {
			base := strings.TrimSuffix(filepath.Base(name), ".jsonl")
			d, err := time.Parse("2006-01-02", base)
			if err != nil {
				continue
			}
			if time.Since(d) > keep+24*time.Hour {
				if os.Remove(name) == nil {
					n++
				}
			}
		}
	}
	return n
}
