// Package license implements offline Ed25519 licence validation and the tier
// caps that follow from it.
//
// Design rule: the guard never panics and never fails closed in a way that
// breaks the binary. A missing, malformed, expired, or forged key degrades to
// Free and records an accurate notice explaining why. The free build ships with
// an empty issuer key and simply runs as Free.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// KeyPrefix is the wire prefix of every Hexward licence key. It is protocol,
// not branding, and is unchanged across the line so existing keys stay valid.
const KeyPrefix = "SNTL1-"

// Product is the module id this binary validates keys for.
const Product = "topolight"

// IssuerPublicKey is set at build time via
//
//	-ldflags "-X github.com/nizartuanku/topolight/internal/license.IssuerPublicKey=<base64>"
//
// The open-source build leaves it empty and therefore always runs as Free.
var IssuerPublicKey = ""

// Tier is the entitlement level.
type Tier string

const (
	TierFree Tier = "free"
	TierPro  Tier = "pro"
	TierTeam Tier = "team"
)

// Valid reports whether t is a known tier.
func (t Tier) Valid() bool { return t == TierFree || t == TierPro || t == TierTeam }

// Title returns a display name.
func (t Tier) Title() string {
	switch t {
	case TierPro:
		return "Pro"
	case TierTeam:
		return "Team"
	default:
		return "Free"
	}
}

// Claims is the signed payload of a licence key.
type Claims struct {
	Product  string `json:"p"`
	Tier     Tier   `json:"t"`
	Licensee string `json:"l,omitempty"`
	IssuedAt int64  `json:"i"`
	Expires  int64  `json:"e,omitempty"` // unix seconds; 0 means perpetual
}

// Caps are the concrete limits a tier grants. 0 means unlimited where noted.
type Caps struct {
	Tier Tier `json:"tier"`

	MaxDevices    int `json:"max_devices"`    // monitored devices
	MaxSites      int `json:"max_sites"`      // 0 = unlimited
	RetentionDays int `json:"retention_days"` // metrics, logs, events
	MaxUsers      int `json:"max_users"`      // 0 = unlimited

	Telegram    bool `json:"telegram"`
	Webhook     bool `json:"webhook"`
	Export      bool `json:"export"`
	Maintenance bool `json:"maintenance"`
	Roles       bool `json:"roles"` // admin/operator/viewer
}

// Unlimited reports whether n means "no limit".
func Unlimited(n int) bool { return n == 0 }

// CapsFor returns the caps granted by a tier.
//
// Every feature is available in every tier — the tiers differ only in
// capacity (devices, sites, users) and history. That is deliberate: a Free
// install must be able to try everything the paid tiers do, on a small network.
func CapsFor(t Tier) Caps {
	all := Caps{Telegram: true, Webhook: true, Export: true, Maintenance: true, Roles: true}
	switch t {
	case TierTeam:
		all.Tier, all.MaxDevices, all.MaxSites, all.RetentionDays, all.MaxUsers = TierTeam, 1500, 0, 365, 0
	case TierPro:
		all.Tier, all.MaxDevices, all.MaxSites, all.RetentionDays, all.MaxUsers = TierPro, 500, 3, 183, 5
	default:
		all.Tier, all.MaxDevices, all.MaxSites, all.RetentionDays, all.MaxUsers = TierFree, 25, 1, 7, 3
	}
	return all
}

// State is the resolved licence status of the running binary.
type State struct {
	Tier     Tier   `json:"tier"`
	Caps     Caps   `json:"caps"`
	Licensee string `json:"licensee,omitempty"`
	Expires  string `json:"expires,omitempty"`
	// Notice explains the outcome in plain language and is always populated.
	Notice string `json:"notice"`
	// Valid is true only when a real signed key was accepted.
	Valid bool `json:"valid"`
}

// Encode builds a licence key string from claims and a private key.
func Encode(claims Claims, priv ed25519.PrivateKey) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("license: marshal claims: %w", err)
	}
	sig := ed25519.Sign(priv, payload)
	enc := base64.RawURLEncoding
	return KeyPrefix + enc.EncodeToString(payload) + "." + enc.EncodeToString(sig), nil
}

func parse(key string, pub ed25519.PublicKey) (Claims, error) {
	var c Claims
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, KeyPrefix) {
		return c, fmt.Errorf("unrecognised key format")
	}
	parts := strings.Split(strings.TrimPrefix(key, KeyPrefix), ".")
	if len(parts) != 2 {
		return c, fmt.Errorf("malformed key")
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(parts[0])
	if err != nil {
		return c, fmt.Errorf("malformed key payload")
	}
	sig, err := enc.DecodeString(parts[1])
	if err != nil {
		return c, fmt.Errorf("malformed key signature")
	}
	if len(sig) != ed25519.SignatureSize {
		return c, fmt.Errorf("invalid signature length")
	}
	if !ed25519.Verify(pub, payload, sig) {
		return c, fmt.Errorf("invalid signature")
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, fmt.Errorf("malformed claims")
	}
	return c, nil
}

func freeState(notice string) State {
	return State{Tier: TierFree, Caps: CapsFor(TierFree), Notice: notice}
}

// Resolve validates key against the compiled-in issuer key. It never returns
// an error: every failure path degrades to Free with an explanatory notice.
func Resolve(key string) State { return resolveWith(key, IssuerPublicKey, time.Now()) }

// ResolveWith validates key against an explicit issuer public key (used by the
// licgen tool to verify keys before they are sent out).
func ResolveWith(key, issuerB64 string) State { return resolveWith(key, issuerB64, time.Now()) }

func resolveWith(key, issuerB64 string, now time.Time) State {
	if strings.TrimSpace(issuerB64) == "" {
		return freeState("Free edition — no issuer key in this build.")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(issuerB64))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return freeState("Running as Free — the issuer key in this build is unreadable.")
	}
	pub := ed25519.PublicKey(raw)
	if strings.TrimSpace(key) == "" {
		return freeState("Free edition — no licence key configured.")
	}
	claims, err := parse(key, pub)
	if err != nil {
		return freeState("Running as Free — licence key rejected: " + err.Error() + ".")
	}
	if claims.Product != Product {
		return freeState(fmt.Sprintf("Running as Free — this key is for %q, not %q.", claims.Product, Product))
	}
	if !claims.Tier.Valid() || claims.Tier == TierFree {
		return freeState("Running as Free — the key carries no paid tier.")
	}
	if claims.Expires != 0 && now.After(time.Unix(claims.Expires, 0)) {
		return freeState(fmt.Sprintf("Running as Free — licence expired on %s.", time.Unix(claims.Expires, 0).UTC().Format("2 January 2006")))
	}
	st := State{Tier: claims.Tier, Caps: CapsFor(claims.Tier), Licensee: claims.Licensee, Valid: true}
	if claims.Expires != 0 {
		st.Expires = time.Unix(claims.Expires, 0).UTC().Format("2 January 2006")
		st.Notice = fmt.Sprintf("%s licence — valid until %s.", claims.Tier.Title(), st.Expires)
	} else {
		st.Notice = fmt.Sprintf("%s licence — active.", claims.Tier.Title())
	}
	if claims.Licensee != "" {
		st.Notice = claims.Licensee + " · " + st.Notice
	}
	return st
}
