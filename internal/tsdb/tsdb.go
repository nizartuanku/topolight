// Package tsdb is a small embedded time-series store for monitoring metrics.
//
// Layout: one file per hour ("raw/2026090214.tlb") holding every series that
// received samples in that hour as a compact chunk (delta-encoded seconds +
// float32 values), followed by an index sorted by series id so a query seeks
// straight to the chunk it needs. Hours older than RawDays are compacted into
// one daily file of 5-minute avg/min/max ("rollup/20260902.tlb"). Files older
// than RetentionDays are deleted. The current hour lives in memory and is
// checkpointed to disk every few minutes.
package tsdb

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	magicRaw      = "TLB2" // raw hours, chunks deflated
	magicRollup   = "TLR2" // 5-minute rollups, chunks deflated
	magicRawV1    = "TLB1"
	magicRollupV1 = "TLR1"
	rollupStep    = 300
)

// deflateChunk compresses one encoded series chunk. Counters and gauges are
// highly repetitive (small deltas, flat values), so this typically shrinks a
// chunk 3–8×, which is what makes months of rollups fit on a small disk.
func deflateChunk(b []byte) []byte {
	var out bytes.Buffer
	w, _ := flate.NewWriter(&out, flate.BestSpeed)
	w.Write(b)
	w.Close()
	return out.Bytes()
}

// f32 returns the float32 bits of v with the low 11 mantissa bits cleared.
// Monitoring values do not need 7 significant digits; keeping ~4 (0.05%)
// makes consecutive samples share bytes, which deflate turns into space.
func f32(v float32) uint32 {
	if v == 0 || math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return math.Float32bits(v)
	}
	return math.Float32bits(v) &^ 0x7FF
}

func inflateChunk(b []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(b))
	defer r.Close()
	return io.ReadAll(r)
}

// Point is one sample. For rollups V is the average and Min/Max are filled.
type Point struct {
	T   int64   `json:"t"`
	V   float64 `json:"v"`
	Min float64 `json:"min,omitempty"`
	Max float64 `json:"max,omitempty"`
}

// Options configure retention.
type Options struct {
	RawDays           int // hours older than this are rolled up (default 30)
	RetentionDays     int // rollups older than this are deleted (default 183)
	CheckpointMinutes int // how often the open hour is written to disk (default 5; 1 in a cluster)
}

type sample struct {
	t int64
	v float32
}

// DB is the store.
type DB struct {
	dir  string
	opts Options

	mu       sync.Mutex
	ids      map[string]uint32
	names    []string // id -> name
	idxFile  *os.File
	hour     int64 // unix hour start of the head
	head     map[uint32][]sample
	dirty    bool
	stop     chan struct{}
	wg       sync.WaitGroup
	memOnly  bool
	memPast  map[int64]map[uint32][]sample // memory-only mode keeps closed hours here
	nowFunc  func() time.Time
	cacheMu  sync.Mutex
	idxCache map[string]*fileIndex
}

// Open opens or creates the database in dir. Empty dir = memory only.
func Open(dir string, opts Options) (*DB, error) {
	if opts.RawDays <= 0 {
		opts.RawDays = 30
	}
	if opts.RetentionDays <= 0 {
		opts.RetentionDays = 183
	}
	db := &DB{dir: dir, opts: opts, ids: map[string]uint32{}, head: map[uint32][]sample{}, stop: make(chan struct{}),
		memOnly: dir == "", memPast: map[int64]map[uint32][]sample{}, nowFunc: time.Now, idxCache: map[string]*fileIndex{}}
	if !db.memOnly {
		for _, sub := range []string{"raw", "rollup"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
				return nil, err
			}
		}
		if err := db.loadSeriesIndex(); err != nil {
			return nil, err
		}
		if err := db.loadHead(); err != nil {
			return nil, err
		}
	}
	db.hour = db.nowFunc().Unix() / 3600 * 3600
	db.wg.Add(1)
	go db.loop()
	return db, nil
}

func (db *DB) loadSeriesIndex() error {
	path := filepath.Join(db.dir, "series.idx")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		id, err := strconv.ParseUint(line[:sp], 10, 32)
		if err != nil {
			continue
		}
		name := line[sp+1:]
		for uint32(len(db.names)) <= uint32(id) {
			db.names = append(db.names, "")
		}
		db.names[id] = name
		db.ids[name] = uint32(id)
	}
	db.idxFile = f
	return nil
}

// loadHead restores the checkpoint of the current hour, if any.
func (db *DB) loadHead() error {
	now := db.nowFunc().Unix() / 3600 * 3600
	path := db.rawPath(now)
	fi, err := db.openIndex(path)
	if err != nil {
		return nil // no checkpoint
	}
	for id := range fi.entries {
		pts, err := fi.readChunk(id)
		if err != nil {
			continue
		}
		db.head[id] = pts
	}
	return nil
}

func (db *DB) rawPath(hour int64) string {
	return filepath.Join(db.dir, "raw", time.Unix(hour, 0).UTC().Format("2006010215")+".tlb")
}

func (db *DB) rollupPath(day int64) string {
	return filepath.Join(db.dir, "rollup", time.Unix(day, 0).UTC().Format("20060102")+".tlb")
}

// Close flushes and stops background work.
func (db *DB) Close() error {
	select {
	case <-db.stop:
	default:
		close(db.stop)
	}
	db.wg.Wait()
	err := db.Checkpoint()
	db.mu.Lock()
	if db.idxFile != nil {
		db.idxFile.Close()
		db.idxFile = nil
	}
	db.mu.Unlock()
	return err
}

func (db *DB) loop() {
	defer db.wg.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			now := db.nowFunc()
			db.rollover(now)
			cp := db.opts.CheckpointMinutes
			if cp <= 0 {
				cp = 5
			}
			if now.Minute()%cp == 0 {
				_ = db.Checkpoint()
			}
			if now.Minute() == 7 { // once an hour, off the rollover edge
				_ = db.Compact()
			}
		case <-db.stop:
			return
		}
	}
}

// seriesID returns (creating) the id of a series name.
func (db *DB) seriesID(name string) uint32 {
	if id, ok := db.ids[name]; ok {
		return id
	}
	id := uint32(len(db.names))
	db.names = append(db.names, name)
	db.ids[name] = id
	if db.idxFile != nil {
		fmt.Fprintf(db.idxFile, "%d %s\n", id, name)
	}
	return id
}

// Append records one sample. Samples must be appended roughly in time order
// per series; a sample older than the current head hour is dropped.
func (db *DB) Append(series string, t int64, v float64) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.rolloverLocked(time.Unix(t, 0))
	if t < db.hour {
		return
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	id := db.seriesID(series)
	pts := db.head[id]
	if n := len(pts); n > 0 && pts[n-1].t >= t {
		if pts[n-1].t == t {
			pts[n-1].v = float32(v)
		}
		return
	}
	db.head[id] = append(pts, sample{t: t, v: float32(v)})
	db.dirty = true
}

func (db *DB) rollover(now time.Time) {
	db.mu.Lock()
	db.rolloverLocked(now)
	db.mu.Unlock()
}

// rolloverLocked closes the head hour when now has moved past it.
func (db *DB) rolloverLocked(now time.Time) {
	h := now.Unix() / 3600 * 3600
	if h <= db.hour {
		return
	}
	if len(db.head) > 0 {
		if db.memOnly {
			db.memPast[db.hour] = db.head
		} else {
			_ = writeRaw(db.rawPath(db.hour), db.head)
			db.invalidate(db.rawPath(db.hour))
		}
	}
	db.head = map[uint32][]sample{}
	db.hour = h
	db.dirty = false
}

// Checkpoint writes the current hour to disk so a crash loses little.
func (db *DB) Checkpoint() error {
	db.mu.Lock()
	if db.memOnly || !db.dirty {
		db.mu.Unlock()
		return nil
	}
	snap := make(map[uint32][]sample, len(db.head))
	for id, pts := range db.head {
		snap[id] = append([]sample(nil), pts...)
	}
	hour := db.hour
	db.dirty = false
	db.mu.Unlock()
	err := writeRaw(db.rawPath(hour), snap)
	db.invalidate(db.rawPath(hour))
	return err
}

// ---- file format ----

type fileIndex struct {
	path    string
	entries map[uint32][2]int64 // id -> offset, length
	rollup  bool
	packed  bool
}

func (db *DB) invalidate(path string) {
	db.cacheMu.Lock()
	delete(db.idxCache, path)
	db.cacheMu.Unlock()
}

func writeRaw(path string, head map[uint32][]sample) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	if _, err := w.WriteString(magicRaw); err != nil {
		return err
	}
	ids := make([]uint32, 0, len(head))
	for id := range head {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	offset := int64(4)
	type ent struct {
		id      uint32
		off, ln int64
	}
	ents := make([]ent, 0, len(ids))
	buf := make([]byte, binary.MaxVarintLen64)
	var chunk bytes.Buffer
	for _, id := range ids {
		pts := head[id]
		chunk.Reset()
		n := binary.PutUvarint(buf, uint64(len(pts)))
		chunk.Write(buf[:n])
		var prev int64
		for i, p := range pts {
			var d int64
			if i == 0 {
				d = p.t
			} else {
				d = p.t - prev
			}
			prev = p.t
			n = binary.PutVarint(buf, d)
			chunk.Write(buf[:n])
			var vb [4]byte
			binary.LittleEndian.PutUint32(vb[:], f32(p.v))
			chunk.Write(vb[:])
		}
		packed := deflateChunk(chunk.Bytes())
		w.Write(packed)
		ents = append(ents, ent{id, offset, int64(len(packed))})
		offset += int64(len(packed))
	}
	idxStart := offset
	for _, e := range ents {
		var b [20]byte
		binary.LittleEndian.PutUint32(b[0:], e.id)
		binary.LittleEndian.PutUint64(b[4:], uint64(e.off))
		binary.LittleEndian.PutUint64(b[12:], uint64(e.ln))
		w.Write(b[:])
	}
	var trailer [12]byte
	binary.LittleEndian.PutUint64(trailer[0:], uint64(idxStart))
	binary.LittleEndian.PutUint32(trailer[8:], uint32(len(ents)))
	w.Write(trailer[:])
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (db *DB) openIndex(path string) (*fileIndex, error) {
	db.cacheMu.Lock()
	if fi, ok := db.idxCache[path]; ok {
		db.cacheMu.Unlock()
		return fi, nil
	}
	db.cacheMu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < 16 {
		return nil, errors.New("tsdb: short file")
	}
	magic := make([]byte, 4)
	if _, err := f.ReadAt(magic, 0); err != nil {
		return nil, err
	}
	fi := &fileIndex{path: path, entries: map[uint32][2]int64{}}
	switch string(magic) {
	case magicRaw:
		fi.packed = true
	case magicRollup:
		fi.rollup, fi.packed = true, true
	case magicRawV1:
	case magicRollupV1:
		fi.rollup = true
	default:
		return nil, errors.New("tsdb: bad magic")
	}
	var trailer [12]byte
	if _, err := f.ReadAt(trailer[:], st.Size()-12); err != nil {
		return nil, err
	}
	idxStart := int64(binary.LittleEndian.Uint64(trailer[0:]))
	n := int(binary.LittleEndian.Uint32(trailer[8:]))
	if idxStart < 4 || idxStart+int64(n)*20+12 != st.Size() {
		return nil, errors.New("tsdb: corrupt index")
	}
	idx := make([]byte, n*20)
	if _, err := f.ReadAt(idx, idxStart); err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		b := idx[i*20:]
		id := binary.LittleEndian.Uint32(b[0:])
		off := int64(binary.LittleEndian.Uint64(b[4:]))
		ln := int64(binary.LittleEndian.Uint64(b[12:]))
		fi.entries[id] = [2]int64{off, ln}
	}
	db.cacheMu.Lock()
	if len(db.idxCache) > 512 {
		db.idxCache = map[string]*fileIndex{}
	}
	db.idxCache[path] = fi
	db.cacheMu.Unlock()
	return fi, nil
}

func (fi *fileIndex) readChunk(id uint32) ([]sample, error) {
	e, ok := fi.entries[id]
	if !ok {
		return nil, nil
	}
	f, err := os.Open(fi.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, e[1])
	if _, err := f.ReadAt(buf, e[0]); err != nil && err != io.EOF {
		return nil, err
	}
	if fi.packed {
		if buf, err = inflateChunk(buf); err != nil {
			return nil, err
		}
	}
	return decodeChunk(buf, fi.rollup)
}

// rollup chunks store 3 float32 per point (avg, min, max) — decodeChunk packs
// min/max into consecutive samples handled by readRollup.
func decodeChunk(buf []byte, rollup bool) ([]sample, error) {
	pos := 0
	n, k := binary.Uvarint(buf)
	if k <= 0 {
		return nil, errors.New("tsdb: chunk count")
	}
	pos += k
	per := 4
	if rollup {
		per = 12
	}
	out := make([]sample, 0, n*uint64(per/4))
	var prev int64
	for i := uint64(0); i < n; i++ {
		d, k := binary.Varint(buf[pos:])
		if k <= 0 {
			return nil, errors.New("tsdb: chunk delta")
		}
		pos += k
		if i == 0 {
			prev = d
		} else {
			prev += d
		}
		if pos+per > len(buf) {
			return nil, errors.New("tsdb: chunk truncated")
		}
		for j := 0; j < per/4; j++ {
			v := math.Float32frombits(binary.LittleEndian.Uint32(buf[pos+j*4:]))
			out = append(out, sample{t: prev, v: v})
		}
		pos += per
	}
	return out, nil
}

// ---- queries ----

// Query returns points for series in [from, to]. Raw hours give 60-s points;
// older ranges come from 5-minute rollups (V = avg, Min/Max filled).
func (db *DB) Query(series string, from, to int64) []Point {
	db.mu.Lock()
	id, ok := db.ids[series]
	var headPts []sample
	headHour := db.hour
	if ok {
		headPts = append([]sample(nil), db.head[id]...)
	}
	db.mu.Unlock()
	if !ok {
		return nil
	}
	var out []Point
	rawCut := db.nowFunc().Add(-time.Duration(db.opts.RawDays)*24*time.Hour).Unix() / 86400 * 86400
	// rollup days
	for day := from / 86400 * 86400; day <= to && day < rawCut; day += 86400 {
		if db.memOnly {
			continue
		}
		fi, err := db.openIndex(db.rollupPath(day))
		if err != nil {
			continue
		}
		pts, err := fi.readChunk(id)
		if err != nil {
			continue
		}
		for i := 0; i+2 < len(pts); i += 3 {
			if pts[i].t >= from && pts[i].t <= to {
				out = append(out, Point{T: pts[i].t, V: float64(pts[i].v), Min: float64(pts[i+1].v), Max: float64(pts[i+2].v)})
			}
		}
	}
	// raw hours
	startHour := from / 3600 * 3600
	if startHour < rawCut {
		startHour = rawCut
	}
	for h := startHour; h <= to && h < headHour; h += 3600 {
		var pts []sample
		if db.memOnly {
			pts = db.memPast[h][id]
		} else {
			fi, err := db.openIndex(db.rawPath(h))
			if err != nil {
				continue
			}
			pts, _ = fi.readChunk(id)
		}
		for _, p := range pts {
			if p.t >= from && p.t <= to {
				out = append(out, Point{T: p.t, V: float64(p.v), Min: float64(p.v), Max: float64(p.v)})
			}
		}
	}
	for _, p := range headPts {
		if p.t >= from && p.t <= to {
			out = append(out, Point{T: p.t, V: float64(p.v), Min: float64(p.v), Max: float64(p.v)})
		}
	}
	return out
}

// Last returns the most recent sample of a series, if any.
func (db *DB) Last(series string) (Point, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	id, ok := db.ids[series]
	if !ok {
		return Point{}, false
	}
	pts := db.head[id]
	if len(pts) == 0 {
		return Point{}, false
	}
	p := pts[len(pts)-1]
	return Point{T: p.t, V: float64(p.v)}, true
}

// SeriesCount reports how many series are known.
func (db *DB) SeriesCount() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.ids)
}

// ---- compaction & retention ----

// Compact rolls up hours older than RawDays into daily files and deletes
// expired rollups. It is safe to call at any time.
func (db *DB) Compact() error {
	if db.memOnly {
		return nil
	}
	now := db.nowFunc()
	rawCut := now.Add(-time.Duration(db.opts.RawDays)*24*time.Hour).Unix() / 86400 * 86400
	files, _ := filepath.Glob(filepath.Join(db.dir, "raw", "*.tlb"))
	byDay := map[int64][]string{}
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".tlb")
		t, err := time.Parse("2006010215", base)
		if err != nil {
			continue
		}
		if t.Unix() < rawCut {
			day := t.Unix() / 86400 * 86400
			byDay[day] = append(byDay[day], f)
		}
	}
	for day, hourFiles := range byDay {
		if err := db.rollupDay(day, hourFiles); err != nil {
			return err
		}
		for _, f := range hourFiles {
			os.Remove(f)
			db.invalidate(f)
		}
	}
	retCut := now.Add(-time.Duration(db.opts.RetentionDays)*24*time.Hour).Unix() / 86400 * 86400
	rfiles, _ := filepath.Glob(filepath.Join(db.dir, "rollup", "*.tlb"))
	for _, f := range rfiles {
		base := strings.TrimSuffix(filepath.Base(f), ".tlb")
		t, err := time.Parse("20060102", base)
		if err != nil {
			continue
		}
		if t.Unix() < retCut {
			os.Remove(f)
			db.invalidate(f)
		}
	}
	return nil
}

func (db *DB) rollupDay(day int64, hourFiles []string) error {
	type agg struct {
		sum      float64
		n        int
		min, max float32
	}
	// series -> bucket(t) -> agg
	acc := map[uint32]map[int64]*agg{}
	for _, f := range hourFiles {
		fi, err := db.openIndex(f)
		if err != nil {
			continue
		}
		for id := range fi.entries {
			pts, err := fi.readChunk(id)
			if err != nil {
				continue
			}
			m := acc[id]
			if m == nil {
				m = map[int64]*agg{}
				acc[id] = m
			}
			for _, p := range pts {
				b := p.t / rollupStep * rollupStep
				a := m[b]
				if a == nil {
					a = &agg{min: p.v, max: p.v}
					m[b] = a
				}
				a.sum += float64(p.v)
				a.n++
				if p.v < a.min {
					a.min = p.v
				}
				if p.v > a.max {
					a.max = p.v
				}
			}
		}
	}
	// merge with an existing rollup for the day (compaction can run in parts)
	path := db.rollupPath(day)
	if fi, err := db.openIndex(path); err == nil {
		for id := range fi.entries {
			pts, err := fi.readChunk(id)
			if err != nil {
				continue
			}
			m := acc[id]
			if m == nil {
				m = map[int64]*agg{}
				acc[id] = m
			}
			for i := 0; i+2 < len(pts); i += 3 {
				if _, exists := m[pts[i].t]; !exists {
					m[pts[i].t] = &agg{sum: float64(pts[i].v), n: 1, min: pts[i+1].v, max: pts[i+2].v}
				}
			}
		}
	}
	// write
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	w.WriteString(magicRollup)
	offset := int64(4)
	ids := make([]uint32, 0, len(acc))
	for id := range acc {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	type ent struct {
		id      uint32
		off, ln int64
	}
	var ents []ent
	buf := make([]byte, binary.MaxVarintLen64)
	var chunk bytes.Buffer
	for _, id := range ids {
		m := acc[id]
		ts := make([]int64, 0, len(m))
		for t := range m {
			ts = append(ts, t)
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
		chunk.Reset()
		n := binary.PutUvarint(buf, uint64(len(ts)))
		chunk.Write(buf[:n])
		var prev int64
		for i, t := range ts {
			d := t
			if i > 0 {
				d = t - prev
			}
			prev = t
			n = binary.PutVarint(buf, d)
			chunk.Write(buf[:n])
			a := m[t]
			var vb [12]byte
			binary.LittleEndian.PutUint32(vb[0:], f32(float32(a.sum/float64(a.n))))
			binary.LittleEndian.PutUint32(vb[4:], f32(a.min))
			binary.LittleEndian.PutUint32(vb[8:], f32(a.max))
			chunk.Write(vb[:])
		}
		packed := deflateChunk(chunk.Bytes())
		w.Write(packed)
		ents = append(ents, ent{id, offset, int64(len(packed))})
		offset += int64(len(packed))
	}
	idxStart := offset
	for _, e := range ents {
		var b [20]byte
		binary.LittleEndian.PutUint32(b[0:], e.id)
		binary.LittleEndian.PutUint64(b[4:], uint64(e.off))
		binary.LittleEndian.PutUint64(b[12:], uint64(e.ln))
		w.Write(b[:])
	}
	var trailer [12]byte
	binary.LittleEndian.PutUint64(trailer[0:], uint64(idxStart))
	binary.LittleEndian.PutUint32(trailer[8:], uint32(len(ents)))
	w.Write(trailer[:])
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	db.invalidate(path)
	return os.Rename(tmp, path)
}

// DiskUsage returns bytes used under the data directory.
func (db *DB) DiskUsage() int64 {
	if db.memOnly {
		return 0
	}
	var total int64
	filepath.Walk(db.dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
