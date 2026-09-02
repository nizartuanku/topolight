package tsdb

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendQueryRolloverCheckpoint(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	now := base
	db, err := Open(dir, Options{RawDays: 30, RetentionDays: 60})
	if err != nil {
		t.Fatal(err)
	}
	db.nowFunc = func() time.Time { return now }
	db.hour = now.Unix() / 3600 * 3600

	// two hours of 60-s samples for two series
	for i := 0; i < 120; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		now = ts
		db.Append("if_in_bps|d1|10", ts.Unix(), float64(i)*1000)
		db.Append("icmp_rtt_ms|d1", ts.Unix(), 2.5)
	}
	// first hour must now be on disk
	if _, err := os.Stat(filepath.Join(dir, "raw", "2026090210.tlb")); err != nil {
		t.Fatalf("hour file missing: %v", err)
	}
	pts := db.Query("if_in_bps|d1|10", base.Unix(), base.Add(2*time.Hour).Unix())
	if len(pts) != 120 {
		t.Fatalf("got %d points", len(pts))
	}
	if pts[0].V != 0 || pts[119].V != 119000 || pts[60].T != base.Add(60*time.Minute).Unix() {
		t.Fatalf("bad points %+v %+v", pts[0], pts[119])
	}
	// out-of-order sample is ignored, duplicate updates
	db.Append("if_in_bps|d1|10", base.Add(119*time.Minute).Unix(), 5)
	if p, _ := db.Last("if_in_bps|d1|10"); p.V != 5 {
		t.Fatalf("dup update failed: %+v", p)
	}
	db.Append("if_in_bps|d1|10", base.Unix(), 42) // older than head hour: dropped
	if p, _ := db.Last("if_in_bps|d1|10"); p.V != 5 {
		t.Fatalf("old sample must not become last")
	}
	// checkpoint + reopen restores the head hour
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(dir, Options{RawDays: 30, RetentionDays: 60})
	if err != nil {
		t.Fatal(err)
	}
	db2.nowFunc = func() time.Time { return now }
	db2.hour = now.Unix() / 3600 * 3600
	// reload head manually since Open used real time
	db2.head = map[uint32][]sample{}
	if err := db2.loadHead(); err != nil {
		t.Fatal(err)
	}
	pts = db2.Query("if_in_bps|d1|10", base.Unix(), base.Add(2*time.Hour).Unix())
	if len(pts) != 120 {
		t.Fatalf("after reopen got %d points", len(pts))
	}
	if db2.SeriesCount() != 2 {
		t.Fatalf("series count %d", db2.SeriesCount())
	}
	db2.Close()
}

func TestRollupAndRetention(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	db, err := Open(dir, Options{RawDays: 2, RetentionDays: 10})
	if err != nil {
		t.Fatal(err)
	}
	now := base
	db.nowFunc = func() time.Time { return now }
	db.hour = now.Unix() / 3600 * 3600
	// one day of samples: value = minute of day
	for i := 0; i < 1440; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		now = ts
		db.Append("cpu|d1", ts.Unix(), float64(i%300))
	}
	// jump 5 days ahead and compact
	now = base.Add(5 * 24 * time.Hour)
	db.rollover(now)
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "rollup", "20260801.tlb")); err != nil {
		t.Fatalf("rollup missing: %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "raw", "20260801*.tlb")); len(files) != 0 {
		t.Fatalf("raw hours not removed: %d", len(files))
	}
	pts := db.Query("cpu|d1", base.Unix(), base.Add(24*time.Hour).Unix())
	if len(pts) != 288 {
		t.Fatalf("rollup points %d", len(pts))
	}
	// bucket 0 covers minutes 0..4 → avg 2, min 0, max 4
	if math.Abs(pts[0].V-2) > 0.01 || pts[0].Min != 0 || pts[0].Max != 4 {
		t.Fatalf("bucket0 %+v", pts[0])
	}
	// jump 20 days: rollup expires
	now = base.Add(20 * 24 * time.Hour)
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "rollup", "20260801.tlb")); !os.IsNotExist(err) {
		t.Fatalf("rollup should be deleted")
	}
	db.Close()
}

func TestMemoryOnly(t *testing.T) {
	db, _ := Open("", Options{})
	defer db.Close()
	n := time.Now().Unix()
	db.Append("x", n, 1)
	if p := db.Query("x", n-1, n+1); len(p) != 1 || p[0].V != 1 {
		t.Fatalf("%+v", p)
	}
}
