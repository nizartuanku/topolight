package syslog

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
)

// LogStore appends log entries to daily JSONL files (yesterday and older are
// gzipped) and searches them by time window.
type LogStore struct {
	dir     string
	memOnly bool

	mu      sync.Mutex
	day     string
	f       *os.File
	w       *bufio.Writer
	ring    []model.LogEntry // memory-only mode / recent cache
	ringN   int
	Count   int64
	Dropped int64
}

// OpenLogStore creates the store. Empty dir keeps a memory ring only.
func OpenLogStore(dir string) (*LogStore, error) {
	ls := &LogStore{dir: dir, memOnly: dir == "", ringN: 20000}
	if !ls.memOnly {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return ls, nil
}

// Append stores one entry.
func (ls *LogStore) Append(e model.LogEntry) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.Count++
	ls.ring = append(ls.ring, e)
	if len(ls.ring) > ls.ringN {
		ls.ring = ls.ring[len(ls.ring)-ls.ringN:]
	}
	if ls.memOnly {
		return
	}
	day := e.Recv.UTC().Format("2006-01-02")
	if ls.f == nil || ls.day != day {
		ls.closeLocked()
		f, err := os.OpenFile(filepath.Join(ls.dir, day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			ls.Dropped++
			return
		}
		ls.f, ls.w, ls.day = f, bufio.NewWriterSize(f, 128<<10), day
		go ls.compressOld(day)
	}
	b, _ := json.Marshal(e)
	ls.w.Write(append(b, '\n'))
	if ls.w.Buffered() > 96<<10 {
		ls.w.Flush()
	}
}

func (ls *LogStore) closeLocked() {
	if ls.w != nil {
		ls.w.Flush()
	}
	if ls.f != nil {
		ls.f.Close()
	}
	ls.f, ls.w = nil, nil
}

// Flush writes buffered lines.
func (ls *LogStore) Flush() {
	ls.mu.Lock()
	if ls.w != nil {
		ls.w.Flush()
	}
	ls.mu.Unlock()
}

// Close flushes and closes the current file.
func (ls *LogStore) Close() {
	ls.mu.Lock()
	ls.closeLocked()
	ls.mu.Unlock()
}

// compressOld gzips every plain day file except the current one.
func (ls *LogStore) compressOld(current string) {
	names, _ := filepath.Glob(filepath.Join(ls.dir, "*.jsonl"))
	for _, n := range names {
		if strings.TrimSuffix(filepath.Base(n), ".jsonl") == current {
			continue
		}
		in, err := os.Open(n)
		if err != nil {
			continue
		}
		out, err := os.Create(n + ".gz.tmp")
		if err != nil {
			in.Close()
			continue
		}
		gz := gzip.NewWriter(out)
		_, err = io.Copy(gz, in)
		gz.Close()
		out.Close()
		in.Close()
		if err == nil && os.Rename(n+".gz.tmp", n+".gz") == nil {
			os.Remove(n)
		} else {
			os.Remove(n + ".gz.tmp")
		}
	}
}

// Prune deletes day files older than keep.
func (ls *LogStore) Prune(keep time.Duration) int {
	if ls.memOnly {
		return 0
	}
	n := 0
	names, _ := filepath.Glob(filepath.Join(ls.dir, "*.jsonl*"))
	for _, name := range names {
		base := strings.SplitN(filepath.Base(name), ".", 2)[0]
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
	return n
}

// Query describes a search.
type Query struct {
	From, To time.Time
	DeviceID string
	MaxSev   int // include severities <= MaxSev (0 emerg .. 7 debug); -1 = all
	Text     string
	Source   string
	Limit    int
}

// Search returns matching entries, newest first.
func (ls *LogStore) Search(q Query) []model.LogEntry {
	if q.Limit <= 0 {
		q.Limit = 500
	}
	if q.To.IsZero() {
		q.To = time.Now()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-24 * time.Hour)
	}
	text := strings.ToLower(q.Text)
	match := func(e model.LogEntry) bool {
		if e.Recv.Before(q.From) || e.Recv.After(q.To) {
			return false
		}
		if q.DeviceID != "" && e.DeviceID != q.DeviceID {
			return false
		}
		if q.MaxSev >= 0 && e.Severity > q.MaxSev {
			return false
		}
		if q.Source != "" && e.Source != q.Source {
			return false
		}
		if text != "" && !strings.Contains(strings.ToLower(e.Message), text) && !strings.Contains(strings.ToLower(e.Mnemonic), text) {
			return false
		}
		return true
	}
	var out []model.LogEntry
	if ls.memOnly {
		ls.mu.Lock()
		for i := len(ls.ring) - 1; i >= 0 && len(out) < q.Limit; i-- {
			if match(ls.ring[i]) {
				out = append(out, ls.ring[i])
			}
		}
		ls.mu.Unlock()
		return out
	}
	ls.Flush()
	// iterate days newest first
	for day := q.To.UTC().Truncate(24 * time.Hour); !day.Before(q.From.UTC().Truncate(24 * time.Hour)); day = day.Add(-24 * time.Hour) {
		base := filepath.Join(ls.dir, day.Format("2006-01-02")+".jsonl")
		var r io.ReadCloser
		if f, err := os.Open(base); err == nil {
			r = f
		} else if f, err := os.Open(base + ".gz"); err == nil {
			gz, err := gzip.NewReader(f)
			if err != nil {
				f.Close()
				continue
			}
			r = struct {
				io.Reader
				io.Closer
			}{gz, f}
		} else {
			continue
		}
		var dayHits []model.LogEntry
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var e model.LogEntry
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if match(e) {
				dayHits = append(dayHits, e)
			}
		}
		r.Close()
		sort.SliceStable(dayHits, func(i, j int) bool { return dayHits[i].Recv.After(dayHits[j].Recv) })
		out = append(out, dayHits...)
		if len(out) >= q.Limit {
			return out[:q.Limit]
		}
	}
	return out
}

// Histogram counts entries per bucket for the UI bar.
func (ls *LogStore) Histogram(q Query, buckets int) []int {
	if buckets <= 0 {
		buckets = 48
	}
	q.Limit = 100000
	entries := ls.Search(q)
	out := make([]int, buckets)
	span := q.To.Sub(q.From)
	if span <= 0 {
		return out
	}
	for _, e := range entries {
		i := int(float64(e.Recv.Sub(q.From)) / float64(span) * float64(buckets))
		if i >= buckets {
			i = buckets - 1
		}
		if i < 0 {
			i = 0
		}
		out[i]++
	}
	return out
}
