package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func keypair(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub), priv
}

func TestFreeBuildAlwaysFree(t *testing.T) {
	st := resolveWith("SNTL1-anything", "", AnyInstance, time.Now())
	if st.Tier != TierFree || st.Valid {
		t.Fatalf("expected free: %+v", st)
	}
	if !strings.Contains(st.Notice, "no issuer key") {
		t.Fatalf("notice: %s", st.Notice)
	}
}

func TestValidProAndTeam(t *testing.T) {
	pubB64, priv := keypair(t)
	for _, tier := range []Tier{TierPro, TierTeam} {
		key, err := Encode(Claims{Product: Product, Tier: tier, Licensee: "ACME", IssuedAt: time.Now().Unix()}, priv)
		if err != nil {
			t.Fatal(err)
		}
		st := resolveWith(key, pubB64, AnyInstance, time.Now())
		if !st.Valid || st.Tier != tier || st.Licensee != "ACME" {
			t.Fatalf("tier %s: %+v", tier, st)
		}
		if st.Caps.MaxDevices != CapsFor(tier).MaxDevices {
			t.Fatalf("caps mismatch")
		}
	}
}

func TestForgedExpiredWrongProduct(t *testing.T) {
	pubB64, priv := keypair(t)
	_, other := keypair(t)
	now := time.Now()

	forged, _ := Encode(Claims{Product: Product, Tier: TierTeam, IssuedAt: now.Unix()}, other)
	if st := resolveWith(forged, pubB64, AnyInstance, now); st.Valid || !strings.Contains(st.Notice, "invalid signature") {
		t.Fatalf("forged accepted: %+v", st)
	}
	expired, _ := Encode(Claims{Product: Product, Tier: TierPro, IssuedAt: now.Unix(), Expires: now.Add(-time.Hour).Unix()}, priv)
	if st := resolveWith(expired, pubB64, AnyInstance, now); st.Valid || !strings.Contains(st.Notice, "expired") {
		t.Fatalf("expired accepted: %+v", st)
	}
	wrong, _ := Encode(Claims{Product: "loglight", Tier: TierPro, IssuedAt: now.Unix()}, priv)
	if st := resolveWith(wrong, pubB64, AnyInstance, now); st.Valid || !strings.Contains(st.Notice, "loglight") {
		t.Fatalf("wrong product accepted: %+v", st)
	}
	// Garbage never panics.
	for _, k := range []string{"", "SNTL1-", "SNTL1-a.b", "SNTL1-" + strings.Repeat("A", 200), "not-a-key"} {
		if st := resolveWith(k, pubB64, AnyInstance, now); st.Valid {
			t.Fatalf("garbage accepted: %q", k)
		}
	}
	if st := resolveWith("SNTL1-x.y", "!!notbase64", AnyInstance, now); st.Valid || st.Tier != TierFree {
		t.Fatalf("broken issuer must degrade to free: %+v", st)
	}
}

func TestInstanceBinding(t *testing.T) {
	pubB64, priv := keypair(t)
	now := time.Now()
	bound, _ := Encode(Claims{Product: Product, Tier: TierPro, IssuedAt: now.Unix(), Instance: "TL-7K2M-9QXA-B3CD"}, priv)
	free, _ := Encode(Claims{Product: Product, Tier: TierTeam, IssuedAt: now.Unix()}, priv)

	// Right instance, case/dash-insensitive.
	for _, id := range []string{"TL-7K2M-9QXA-B3CD", "tl-7k2m-9qxa-b3cd", "TL7K2M9QXAB3CD"} {
		st := resolveWith(bound, pubB64, id, now)
		if !st.Valid || st.Tier != TierPro || st.Bound != "TL-7K2M-9QXA-B3CD" || st.Instance != id {
			t.Fatalf("bound key on %s: %+v", id, st)
		}
	}
	// Wrong instance → Free with a notice naming both.
	st := resolveWith(bound, pubB64, "TL-0000-0000-0000", now)
	if st.Valid || !strings.Contains(st.Notice, "bound to instance TL-7K2M-9QXA-B3CD, not TL-0000-0000-0000") {
		t.Fatalf("wrong instance accepted: %+v", st)
	}
	// Unknown instance (no data dir) → Free.
	if st := resolveWith(bound, pubB64, "", now); st.Valid || !strings.Contains(st.Notice, "no Instance ID") {
		t.Fatalf("no-instance accepted bound key: %+v", st)
	}
	// Issuer-side verification skips the check.
	if st := ResolveWith(bound, pubB64, ""); !st.Valid || st.Instance != "" {
		t.Fatalf("issuer verify: %+v", st)
	}
	// Unbound keys still work anywhere (manual issuance / legacy).
	if st := resolveWith(free, pubB64, "TL-0000-0000-0000", now); !st.Valid || st.Bound != "" || st.Tier != TierTeam {
		t.Fatalf("unbound key: %+v", st)
	}
}

func TestInstanceIDFile(t *testing.T) {
	dir := t.TempDir()
	if LoadInstanceID("") != "" {
		t.Fatal("no dir must give no id")
	}
	a := LoadInstanceID(dir)
	if NormalizeInstance(a) != a || len(a) != 17 || !strings.HasPrefix(a, "TL-") {
		t.Fatalf("bad id %q", a)
	}
	if b := LoadInstanceID(dir); b != a {
		t.Fatalf("id not stable: %q vs %q", a, b)
	}
	if NewInstanceID() == NewInstanceID() {
		t.Fatal("ids must differ")
	}
	for _, bad := range []string{"", "TL-", "XX-7K2M", "TL-7K2M!9QXA"} {
		if NormalizeInstance(bad) != "" {
			t.Fatalf("accepted %q", bad)
		}
	}
	if !SameInstance("tl-7k2m-9qxa-b3cd", "TL7K2M9QXAB3CD") || SameInstance("", "") {
		t.Fatal("SameInstance")
	}
}
