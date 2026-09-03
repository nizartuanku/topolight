package report

import (
	"strings"
	"testing"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
	"github.com/nizartuanku/topolight/internal/tsdb"
)

func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := tsdb.Open(dir+"/tsdb", tsdb.Options{RawDays: 7, RetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	st.PutSite(model.Site{ID: "s1", Name: "HQ"})
	for i, n := range []string{"core1", "acc1"} {
		st.PutDevice(model.Device{ID: "d" + n, SiteID: "s1", Name: n, IP: "10.0.0." + string(rune('1'+i)), Monitored: true, Role: model.RoleCore, Vendor: "Cisco", Status: model.StatusUp, Created: now.Add(-48 * time.Hour)})
	}
	// acc1 was down for 2 hours yesterday
	st.PutAlert(model.Alert{ID: "a1", Rule: "device_down", Severity: model.SevMajor, State: model.AlertResolved, DeviceID: "dacc1", SiteID: "s1", OpenedAt: now.Add(-26 * time.Hour), ResolvedAt: now.Add(-24 * time.Hour), DedupKey: "device_down:dacc1"})
	db.Append("cpu_pct|dcore1", now.Add(-time.Hour).Unix(), 40)
	db.Append("cpu_pct|dcore1", now.Add(-30*time.Minute).Unix(), 80)
	res := Generate(Deps{Store: st, DB: db, Instance: "test"}, model.Report{Name: "T", Period: "7d", Sections: []string{"availability", "alerts", "utilisation", "inventory"}}, now)
	var avail *Table
	for i := range res.Tables {
		if res.Tables[i].Key == "availability" {
			avail = &res.Tables[i]
		}
	}
	if avail == nil || len(avail.Rows) != 2 {
		t.Fatalf("availability table: %+v", res.Tables)
	}
	// acc1 first (lowest): created 48 h ago, so 2 h of 48 h = 95.83 %
	if avail.Rows[0][0] != "acc1" || !strings.HasPrefix(avail.Rows[0][3], "95.8") || avail.Rows[0][5] != "1" {
		t.Fatalf("acc1 row: %v", avail.Rows[0])
	}
	if avail.Rows[1][3] != "100.00" {
		t.Fatalf("core1 row: %v", avail.Rows[1])
	}
	page := HTML(res, "test")
	if !strings.Contains(page, "<table>") || !strings.Contains(page, "acc1") || !strings.Contains(page, "Device load") {
		t.Fatal("html")
	}
	csv := string(CSV(res))
	if !strings.Contains(csv, "# Availability") || !strings.Contains(csv, "acc1,HQ,core,95.8") {
		t.Fatalf("csv: %s", csv)
	}
	if !due(model.Report{Schedule: "daily", Hour: 7}, time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)) || due(model.Report{Schedule: "weekly", Hour: 7}, time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)) {
		t.Fatal("schedule")
	}
}
