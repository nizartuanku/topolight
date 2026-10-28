package siemexport

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nizartuanku/hexward-core/hfs"
	"github.com/nizartuanku/hexward-core/sink"

	"github.com/nizartuanku/topolight/internal/model"
)

var (
	t0   = time.Date(2026, 10, 28, 7, 31, 6, 0, time.UTC)
	t1   = time.Date(2026, 10, 28, 7, 45, 0, 0, time.UTC)
	meta = hfs.Meta{Product: "topolight", Version: "0.4.5", Host: "noc-01"}
)

type dir struct{}

func (dir) Device(id string) (model.Device, error) {
	if id == "d1" {
		return model.Device{ID: "d1", Name: "core-sw1", IP: "10.0.0.2", Vendor: "Cisco"}, nil
	}
	return model.Device{}, errors.New("no such device")
}

func (dir) Site(id string) (model.Site, error) {
	return model.Site{ID: id, Name: "HQ Jakarta"}, nil
}

func (dir) Interface(id string) (model.Interface, error) {
	if id == "d1:7" {
		return model.Interface{ID: id, DeviceID: "d1", Name: "Gi1/0/7", Alias: "uplink-dc"}, nil
	}
	return model.Interface{}, errors.New("no such interface")
}

func (dir) Probe(id string) (model.Probe, error) {
	if id == "prb_1" {
		return model.Probe{ID: id, Name: "portal", Type: "http", Target: "https://portal.example.com"}, nil
	}
	return model.Probe{}, errors.New("no such probe")
}

func TestInterfaceAndProbeTargets(t *testing.T) {
	x := &Exporter{Meta: meta, Dir: dir{}}
	a := alert(model.AlertOpen)
	a.Object = "d1:7"
	if got := x.Event(a, t1).Finding.Target; got != "core-sw1 (10.0.0.2) Gi1/0/7" {
		t.Errorf("interface target %q", got)
	}
	p := alert(model.AlertOpen)
	p.DeviceID, p.Object, p.Rule = "", "prb_1", "probe_failed"
	e := x.Event(p, t1)
	if e.Finding.Target != "https://portal.example.com" {
		t.Errorf("probe target %q", e.Finding.Target)
	}
	if b, _ := hfs.Marshal(e); !strings.Contains(string(b), `"probe":"portal"`) {
		t.Errorf("probe evidence: %s", b)
	}
}

func alert(state model.AlertState) model.Alert {
	a := model.Alert{
		ID: "al_1", Rule: "device_down", Severity: model.SevCritical, State: state,
		Domain: "network", SiteID: "s1", DeviceID: "d1", Title: "core-sw1 is down",
		Detail: "No reply to ICMP or SNMP for 3 polls.", OpenedAt: t0, UpdatedAt: t0,
		Occurrences: 1, Children: 14, Evidence: []string{"poll 07:31:06", "trap 07:31:06"},
		DedupKey: "device_down:d1", Impact: "14 devices behind it unreachable",
	}
	if state == model.AlertResolved {
		a.UpdatedAt, a.ResolvedAt = t1, t1
	}
	return a
}

func TestOpenedEvent(t *testing.T) {
	x := &Exporter{Meta: meta, Dir: dir{}}
	e := x.Event(alert(model.AlertOpen), time.Time{})
	f := e.Finding
	if e.Event != hfs.KindOpened || e.TS != t0.Format(time.RFC3339) {
		t.Fatalf("event %s at %s", e.Event, e.TS)
	}
	if f.Fingerprint != "device_down:d1" || f.ID != "al_1" || f.Check != "topolight.device_down" {
		t.Fatalf("identity %+v", f)
	}
	if f.Severity != "critical" || f.SeverityNum != 10 || f.Target != "core-sw1 (10.0.0.2)" {
		t.Fatalf("severity/target %+v", f)
	}
	if f.GroupID != "s1" {
		t.Fatalf("group %v", f.GroupID)
	}
	if e.SrcIP != "" {
		t.Fatalf("the affected device became src_ip %q", e.SrcIP)
	}
	b, _ := hfs.Marshal(e)
	for _, s := range []string{`"device_ip":"10.0.0.2"`, `"site":"HQ Jakarta"`, `"suppressed_children":14`, "trap 07:31:06"} {
		if !strings.Contains(string(b), s) {
			t.Errorf("missing %s in %s", s, b)
		}
	}
}

func TestResolvedAndAcked(t *testing.T) {
	x := &Exporter{Meta: meta, Dir: dir{}}
	e := x.Event(alert(model.AlertResolved), time.Time{})
	if e.Event != hfs.KindResolved || e.Finding.Status != "resolved" || e.Finding.ResolvedAt != t1.Format(time.RFC3339) {
		t.Fatalf("resolved %+v", e)
	}
	if e.TS != t1.Format(time.RFC3339) {
		t.Fatalf("ts %s", e.TS)
	}
	a := alert(model.AlertAcked)
	a.AckedBy = "nizar"
	e = x.Event(a, time.Time{})
	if e.Event != hfs.KindOpened || e.Finding.Status != "acknowledged" {
		t.Fatalf("acked %+v", e)
	}
}

func TestUnknownDeviceAndSeverities(t *testing.T) {
	a := alert(model.AlertOpen)
	a.DeviceID, a.DedupKey = "gone", ""
	e := (&Exporter{Meta: meta, Dir: dir{}}).Event(a, t1)
	if e.Finding.Target != "gone" || e.Finding.Fingerprint != "al_1" {
		t.Fatalf("fallbacks %+v", e.Finding)
	}
	for in, want := range map[model.Severity]string{model.SevCritical: "critical", model.SevMajor: "high", model.SevMinor: "medium", model.SevInfo: "info", "odd": "low"} {
		if got := Severity(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}

type slow struct {
	mu   sync.Mutex
	got  []hfs.Event
	wait chan struct{}
}

func (s *slow) Name() string { return "slow" }
func (s *slow) Send(ctx context.Context, evs []hfs.Event) error {
	<-s.wait
	s.mu.Lock()
	s.got = append(s.got, evs...)
	s.mu.Unlock()
	return nil
}

func TestTeeNeverHoldsUpNotifications(t *testing.T) {
	s := &slow{wait: make(chan struct{})}
	x := &Exporter{Sinks: []sink.Sink{s}, Meta: meta, Every: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, out := make(chan model.Alert), make(chan model.Alert, 10)
	go x.Tee(ctx, in, out)
	for i := 0; i < 3; i++ {
		select {
		case in <- alert(model.AlertOpen):
		case <-time.After(time.Second):
			t.Fatal("tee blocked while the SIEM was stalled")
		}
	}
	for i := 0; i < 3; i++ {
		select {
		case <-out:
		case <-time.After(time.Second):
			t.Fatal("notification not forwarded")
		}
	}
	close(s.wait)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.got)
		s.mu.Unlock()
		if n == 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("alerts not exported once the SIEM recovered")
}

type failing struct{}

func (failing) Name() string                            { return "broken" }
func (failing) Send(context.Context, []hfs.Event) error { return errors.New("down") }

func TestSendTriesEverySink(t *testing.T) {
	p := filepath.Join(t.TempDir(), "topolight.ndjson")
	f := &sink.File{Path: p}
	defer f.Close()
	x := &Exporter{Sinks: []sink.Sink{failing{}, f}, Meta: meta}
	err := x.Send(context.Background(), []model.Alert{alert(model.AlertOpen), alert(model.AlertResolved)})
	if err == nil || !strings.Contains(err.Error(), "broken: down") {
		t.Fatalf("err = %v", err)
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), "\n") != 2 {
		t.Fatalf("file: %s", b)
	}
}

func TestFlags(t *testing.T) {
	build := func(args ...string) (*Exporter, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		c := Register(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return c.Build(meta)
	}
	if x, err := build(); x != nil || err != nil {
		t.Fatalf("no flags: %v %v", x, err)
	}
	x, err := build("-export-file", filepath.Join(t.TempDir(), "a.ndjson"), "-export-syslog", "127.0.0.1:5514", "-export-syslog-network", "tcp")
	if err != nil || strings.Join(x.Names(), ",") != "file,syslog-json" {
		t.Fatalf("%v %v", x.Names(), err)
	}
	for _, bad := range [][]string{{"-hec-url", "https://s:8088"}, {"-export-syslog", "nohost"}, {"-export-evidence", "all"}} {
		if _, err := build(bad...); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
