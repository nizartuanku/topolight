package backup

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version is one stored configuration.
type Version struct {
	ID       string    `json:"id"` // timestamp-based, sortable
	DeviceID string    `json:"device_id"`
	TS       time.Time `json:"ts"`
	Hash     string    `json:"hash"`
	Lines    int       `json:"lines"`
	Bytes    int       `json:"bytes"`
	Added    int       `json:"added,omitempty"`   // vs previous version
	Removed  int       `json:"removed,omitempty"` // vs previous version
	Source   string    `json:"source"`            // schedule|user|syslog
	Note     string    `json:"note,omitempty"`
}

// Status is the last attempt per device (stored alongside the versions).
type Status struct {
	DeviceID  string    `json:"device_id"`
	LastTry   time.Time `json:"last_try"`
	LastOK    time.Time `json:"last_ok"`
	Error     string    `json:"error,omitempty"`
	Versions  int       `json:"versions"`
	Latest    string    `json:"latest,omitempty"`
	Unchanged int       `json:"unchanged"` // consecutive runs without change
}

// Store keeps <dir>/configs/<device>/<id>.txt.gz + index.json.
type Store struct {
	mu   sync.Mutex
	dir  string
	idx  map[string]*devIndex // device id → index
	Keep int                  // versions per device (default 50)
}

type devIndex struct {
	Status   Status    `json:"status"`
	Versions []Version `json:"versions"` // oldest first
}

// Open loads the index (lazily per device).
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir, idx: map[string]*devIndex{}, Keep: 50}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o700); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) devDir(id string) string { return filepath.Join(s.dir, "configs", id) }

func (s *Store) loadLocked(id string) *devIndex {
	if d := s.idx[id]; d != nil {
		return d
	}
	d := &devIndex{Status: Status{DeviceID: id}}
	if s.dir != "" {
		if b, err := os.ReadFile(filepath.Join(s.devDir(id), "index.json")); err == nil {
			_ = json.Unmarshal(b, d)
		}
	}
	s.idx[id] = d
	return d
}

func (s *Store) saveLocked(id string) error {
	if s.dir == "" {
		return nil
	}
	d := s.idx[id]
	if err := os.MkdirAll(s.devDir(id), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(d, "", " ")
	tmp := filepath.Join(s.devDir(id), "index.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.devDir(id), "index.json"))
}

// Put stores a configuration if it differs from the latest (after normalising).
// It returns the new version (or the unchanged latest) and whether it changed.
func (s *Store) Put(deviceID, raw, normalised, source, note string, now time.Time, norm func(string) string) (Version, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.loadLocked(deviceID)
	sum := sha256.Sum256([]byte(normalised))
	hash := hex.EncodeToString(sum[:8])
	d.Status.LastTry, d.Status.LastOK, d.Status.Error = now, now, ""
	if n := len(d.Versions); n > 0 && d.Versions[n-1].Hash == hash {
		d.Status.Unchanged++
		return d.Versions[n-1], false, s.saveLocked(deviceID)
	}
	v := Version{ID: now.UTC().Format("20060102T150405Z"), DeviceID: deviceID, TS: now, Hash: hash, Lines: strings.Count(raw, "\n"), Bytes: len(raw), Source: source, Note: note}
	if n := len(d.Versions); n > 0 {
		if prev, err := s.readLocked(deviceID, d.Versions[n-1].ID); err == nil {
			// counts on the normalised text so a timestamp line never counts as a change
			pn := prev
			if norm != nil {
				pn = norm(prev)
			}
			a, r := Counts(Diff(strings.Split(strings.TrimRight(pn, "\n"), "\n"), strings.Split(strings.TrimRight(normalised, "\n"), "\n")))
			v.Added, v.Removed = a, r
		}
	}
	if s.dir != "" {
		if err := os.MkdirAll(s.devDir(deviceID), 0o700); err != nil {
			return v, false, err
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write([]byte(raw))
		zw.Close()
		if err := os.WriteFile(filepath.Join(s.devDir(deviceID), v.ID+".txt.gz"), buf.Bytes(), 0o600); err != nil {
			return v, false, err
		}
	} else {
		s.mem(deviceID)[v.ID] = raw
	}
	d.Versions = append(d.Versions, v)
	d.Status.Unchanged = 0
	// cap
	keep := s.Keep
	if keep <= 0 {
		keep = 50
	}
	for len(d.Versions) > keep {
		old := d.Versions[0]
		d.Versions = d.Versions[1:]
		if s.dir != "" {
			os.Remove(filepath.Join(s.devDir(deviceID), old.ID+".txt.gz"))
		}
	}
	d.Status.Versions, d.Status.Latest = len(d.Versions), v.ID
	return v, true, s.saveLocked(deviceID)
}

var memStore = map[string]map[string]string{}

func (s *Store) mem(dev string) map[string]string {
	m := memStore[dev]
	if m == nil {
		m = map[string]string{}
		memStore[dev] = m
	}
	return m
}

// Fail records a failed attempt.
func (s *Store) Fail(deviceID, errText string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.loadLocked(deviceID)
	d.Status.LastTry, d.Status.Error = now, errText
	_ = s.saveLocked(deviceID)
}

func (s *Store) readLocked(deviceID, id string) (string, error) {
	if s.dir == "" {
		if raw, ok := s.mem(deviceID)[id]; ok {
			return raw, nil
		}
		return "", errors.New("no such version")
	}
	f, err := os.Open(filepath.Join(s.devDir(deviceID), id+".txt.gz"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(zr)
	return string(b), err
}

// Read returns one stored configuration.
func (s *Store) Read(deviceID, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validID(id) {
		return "", errors.New("bad version id")
	}
	return s.readLocked(deviceID, id)
}

func validID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9') && c != 'T' && c != 'Z' {
			return false
		}
	}
	return true
}

// Versions lists a device's versions, newest first, plus status.
func (s *Store) Versions(deviceID string) ([]Version, Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.loadLocked(deviceID)
	out := append([]Version(nil), d.Versions...)
	sort.Slice(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	return out, d.Status
}

// Statuses returns the status of every device that has an index.
func (s *Store) Statuses() map[string]Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Status{}
	if s.dir != "" {
		if ents, err := os.ReadDir(filepath.Join(s.dir, "configs")); err == nil {
			for _, e := range ents {
				if e.IsDir() {
					s.loadLocked(e.Name())
				}
			}
		}
	}
	for id, d := range s.idx {
		out[id] = d.Status
	}
	return out
}

// Forget removes a device's history.
func (s *Store) Forget(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.idx, deviceID)
	delete(memStore, deviceID)
	if s.dir != "" {
		os.RemoveAll(s.devDir(deviceID))
	}
}

// DiskUsage in bytes.
func (s *Store) DiskUsage() int64 {
	if s.dir == "" {
		return 0
	}
	var n int64
	filepath.Walk(filepath.Join(s.dir, "configs"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}
