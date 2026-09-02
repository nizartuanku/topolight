package tsdb

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSizeReport prints bytes per point for a realistic day so the README
// sizing numbers are measured, not guessed. Run with TOPOLIGHT_SIZE_REPORT=1.
func TestSizeReport(t *testing.T) {
	if os.Getenv("TOPOLIGHT_SIZE_REPORT") == "" {
		t.Skip("set TOPOLIGHT_SIZE_REPORT=1")
	}
	dir := t.TempDir()
	db, err := Open(dir, Options{RawDays: 7, RetentionDays: 365})
	if err != nil {
		t.Fatal(err)
	}
	rnd := rand.New(rand.NewSource(1))
	series := 2000
	base := time.Now().UTC().Truncate(time.Hour).Add(time.Hour)
	points := 0
	for m := 0; m < 1440; m++ {
		ts := base.Add(time.Duration(m) * time.Minute).Unix()
		for s := 0; s < series; s++ {
			var v float64
			switch s % 4 {
			case 0: // traffic bps: noisy around a level with a daily wave
				v = float64(1e6*(s%40+1)) * (0.6 + 0.4*math.Sin(float64(m)/229)) * (0.9 + 0.2*rnd.Float64())
			case 1: // cpu %
				v = 30 + 15*math.Sin(float64(m)/97) + rnd.Float64()*3
			case 2: // rtt ms
				v = 0.8 + rnd.Float64()*0.3
			case 3: // flat gauge
				v = 41
			}
			db.Append(fmt.Sprintf("s|%d", s), ts, v)
			points++
		}
	}
	db.rollover(base.Add(48 * time.Hour))
	var raw0 int64
	filepath.Walk(filepath.Join(dir, "raw"), func(_ string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() {
			raw0 += fi.Size()
		}
		return nil
	})
	t.Logf("raw before compaction: %d B (%.2f B/point)", raw0, float64(raw0)/float64(points))
	db.nowFunc = func() time.Time { return base.Add(10 * 24 * time.Hour) }
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	var raw, roll int64
	filepath.Walk(filepath.Join(dir, "raw"), func(_ string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() {
			raw += fi.Size()
		}
		return nil
	})
	filepath.Walk(filepath.Join(dir, "rollup"), func(_ string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() {
			roll += fi.Size()
		}
		return nil
	})
	t.Logf("series=%d points=%d raw=%d B (%.2f B/point, %.1f KB/series/day) rollup=%d B (%.1f KB/series/day)", series, points, raw, float64(raw)/float64(points), float64(raw)/float64(series)/1024, roll, float64(roll)/float64(series)/1024)
	db.Close()
}
