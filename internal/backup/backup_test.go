package backup

import (
	"strings"
	"testing"
	"time"
)

func TestDiff(t *testing.T) {
	a := strings.Split("a\nb\nc\nd\ne", "\n")
	b := strings.Split("a\nx\nc\ne\nf", "\n")
	ops := Diff(a, b)
	add, rem := Counts(ops)
	if add != 2 || rem != 2 {
		t.Fatalf("counts %d %d: %+v", add, rem, ops)
	}
	// reconstruct both sides
	var ra, rb []string
	for _, o := range ops {
		if o.Kind != '+' {
			ra = append(ra, o.Line)
		}
		if o.Kind != '-' {
			rb = append(rb, o.Line)
		}
	}
	if strings.Join(ra, "\n") != strings.Join(a, "\n") || strings.Join(rb, "\n") != strings.Join(b, "\n") {
		t.Fatalf("diff does not reconstruct: %+v", ops)
	}
	if h := Hunks(ops, 1); len(h) == 0 || h[0].Line != "a" {
		t.Fatalf("hunks: %+v", h)
	}
	if ops := Diff(nil, nil); len(ops) != 0 {
		t.Fatal("empty")
	}
	if add, rem := Counts(Diff(a, a)); add+rem != 0 {
		t.Fatal("identical")
	}
}

func TestNormaliseAndClean(t *testing.T) {
	rc := RecipeFor("cisco-ios")
	raw := "show running-config\r\nBuilding configuration...\r\n\r\nCurrent configuration : 1234 bytes\r\n!\r\n! Last configuration change at 10:00:00 UTC Wed Sep 3 2026 by admin\r\n!\r\nhostname sw1\r\n --More-- \x08\x08\x08\x08\x08\x08\x08\x08\x08\x08         interface Gi1/0/1\r\nsw1#\r\n"
	c := clean(raw, rc)
	if strings.Contains(c, "--More--") || strings.Contains(c, "sw1#") || strings.HasPrefix(c, "show running") {
		t.Fatalf("clean: %q", c)
	}
	n := Normalise(c, rc)
	if strings.Contains(n, "Last configuration change") || !strings.Contains(n, "hostname sw1") || !strings.Contains(n, "interface Gi1/0/1") {
		t.Fatalf("normalise: %q", n)
	}
	// two captures differing only in the timestamp compare equal
	c2 := strings.Replace(c, "10:00:00", "11:00:00", 1)
	if Normalise(c, rc) != Normalise(c2, rc) {
		t.Fatal("timestamp not ignored")
	}
}

func TestStore(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Keep = 3
	now := time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)
	rc := RecipeFor("cisco-ios")
	cfg := "hostname a\ninterface Gi1\n description x\n"
	v1, changed, err := s.Put("dev1", cfg, Normalise(cfg, rc), "user", "", now, nil)
	if err != nil || !changed {
		t.Fatalf("first put: %v %v", err, changed)
	}
	_, changed, _ = s.Put("dev1", cfg, Normalise(cfg, rc), "schedule", "", now.Add(time.Hour), nil)
	if changed {
		t.Fatal("identical config stored twice")
	}
	cfg2 := cfg + "interface Gi2\n"
	v2, changed, _ := s.Put("dev1", cfg2, Normalise(cfg2, rc), "schedule", "", now.Add(2*time.Hour), nil)
	if !changed || v2.Added != 1 || v2.Removed != 0 {
		t.Fatalf("second put: %+v", v2)
	}
	for i := 0; i < 4; i++ {
		c := cfg2 + strings.Repeat("!\n", i+1)
		s.Put("dev1", c, Normalise(c, rc), "schedule", "", now.Add(time.Duration(3+i)*time.Hour), nil)
	}
	vs, st := s.Versions("dev1")
	if len(vs) != 3 || st.Versions != 3 || vs[0].TS.Before(vs[1].TS) {
		t.Fatalf("cap: %d %+v", len(vs), st)
	}
	if _, err := s.Read("dev1", v1.ID); err == nil {
		t.Fatal("evicted version still readable")
	}
	txt, err := s.Read("dev1", vs[0].ID)
	if err != nil || !strings.HasPrefix(txt, "hostname a") {
		t.Fatalf("read: %v %q", err, txt)
	}
	// reload from disk
	s2, _ := Open(s.dir)
	vs2, st2 := s2.Versions("dev1")
	if len(vs2) != 3 || st2.Latest != vs[0].ID {
		t.Fatalf("reload: %d %+v", len(vs2), st2)
	}
	s2.Fail("dev1", "ssh: timeout", now.Add(10*time.Hour))
	if _, st := s2.Versions("dev1"); st.Error == "" || st.LastOK.IsZero() {
		t.Fatalf("fail: %+v", st)
	}
	_ = v2
}
