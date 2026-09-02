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
	st := resolveWith("SNTL1-anything", "", time.Now())
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
		st := resolveWith(key, pubB64, time.Now())
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
	if st := resolveWith(forged, pubB64, now); st.Valid || !strings.Contains(st.Notice, "invalid signature") {
		t.Fatalf("forged accepted: %+v", st)
	}
	expired, _ := Encode(Claims{Product: Product, Tier: TierPro, IssuedAt: now.Unix(), Expires: now.Add(-time.Hour).Unix()}, priv)
	if st := resolveWith(expired, pubB64, now); st.Valid || !strings.Contains(st.Notice, "expired") {
		t.Fatalf("expired accepted: %+v", st)
	}
	wrong, _ := Encode(Claims{Product: "loglight", Tier: TierPro, IssuedAt: now.Unix()}, priv)
	if st := resolveWith(wrong, pubB64, now); st.Valid || !strings.Contains(st.Notice, "loglight") {
		t.Fatalf("wrong product accepted: %+v", st)
	}
	// Garbage never panics.
	for _, k := range []string{"", "SNTL1-", "SNTL1-a.b", "SNTL1-" + strings.Repeat("A", 200), "not-a-key"} {
		if st := resolveWith(k, pubB64, now); st.Valid {
			t.Fatalf("garbage accepted: %q", k)
		}
	}
	if st := resolveWith("SNTL1-x.y", "!!notbase64", now); st.Valid || st.Tier != TierFree {
		t.Fatalf("broken issuer must degrade to free: %+v", st)
	}
}
