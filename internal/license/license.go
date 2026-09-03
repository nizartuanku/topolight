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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// Instance binds the key to one installation (its Instance ID, see
	// LoadInstanceID). Empty means the key is not bound. A cluster shares one
	// Instance ID because <data>/instance.id is mirrored to every node.
	Instance string `json:"n,omitempty"`
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
	// Instance is this installation's Instance ID (always populated when the
	// data dir is known); Bound is the Instance ID the accepted key names.
	Instance string `json:"instance,omitempty"`
	Bound    string `json:"bound,omitempty"`
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
// instance is this installation's Instance ID (see LoadInstanceID); "" means
// unknown, in which case a bound key is rejected because it cannot be proven
// to belong here.
func Resolve(key, instance string) State {
	return resolveWith(key, IssuerPublicKey, instance, time.Now())
}

// ResolveWith validates key against an explicit issuer public key (used by the
// licgen tool to verify keys before they are sent out). Pass instance "" to
// skip the binding check so an issuer can verify any key it minted.
func ResolveWith(key, issuerB64, instance string) State {
	if instance == "" {
		instance = AnyInstance
	}
	return resolveWith(key, issuerB64, instance, time.Now())
}

// AnyInstance disables the instance-binding check (issuer-side verification).
const AnyInstance = "*"

func resolveWith(key, issuerB64, instance string, now time.Time) State {
	st := resolve(key, issuerB64, instance, now)
	if instance != AnyInstance {
		st.Instance = instance
	}
	return st
}

func resolve(key, issuerB64, instance string, now time.Time) State {
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
	if claims.Instance != "" && instance != AnyInstance {
		if instance == "" {
			return freeState(fmt.Sprintf("Running as Free — this key is bound to instance %s but this server has no Instance ID (no data directory).", claims.Instance))
		}
		if !SameInstance(claims.Instance, instance) {
			return freeState(fmt.Sprintf("Running as Free — this key is bound to instance %s, not %s.", claims.Instance, instance))
		}
	}
	st := State{Tier: claims.Tier, Caps: CapsFor(claims.Tier), Licensee: claims.Licensee, Bound: claims.Instance, Valid: true}
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

// ---- Instance ID --------------------------------------------------------------------------

// InstanceFile is the name of the Instance ID file inside the data directory.
// It is mirrored across a cluster (everything outside cluster/ is), so all
// nodes present the same ID and a key bound to it survives failover.
const InstanceFile = "instance.id"

// instanceAlphabet is Crockford base32: no I, L, O, U — easy to read aloud.
const instanceAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewInstanceID returns a fresh random Instance ID such as TL-7K2M-9QXA-B3CD
// (60 random bits).
func NewInstanceID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failing is a broken system; fall back to time so we still
		// produce something unique-ish rather than crash.
		t := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(t >> (uint(i) * 5))
		}
	}
	var sb strings.Builder
	sb.WriteString("TL-")
	for i, c := range b {
		if i == 4 || i == 8 {
			sb.WriteByte('-')
		}
		sb.WriteByte(instanceAlphabet[int(c)&31])
	}
	return sb.String()
}

// LoadInstanceID reads <dir>/instance.id, creating it on first use. It returns
// "" when dir is empty (no data directory) or the file cannot be written.
func LoadInstanceID(dir string) string {
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, InstanceFile)
	if b, err := os.ReadFile(p); err == nil {
		if id := NormalizeInstance(string(b)); id != "" {
			return id
		}
	}
	id := NewInstanceID()
	if err := os.WriteFile(p, []byte(id+"\n"), 0o644); err != nil {
		return ""
	}
	return id
}

// NormalizeInstance upper-cases and strips whitespace so IDs typed by hand
// (or pasted with a trailing newline) compare equal. Returns "" when the
// value is not a plausible Instance ID.
func NormalizeInstance(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	if !strings.HasPrefix(s, "TL-") || len(s) < 8 || len(s) > 40 {
		return ""
	}
	for _, r := range s[3:] {
		if !(r == '-' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z')) {
			return ""
		}
	}
	return s
}

// SameInstance compares two Instance IDs ignoring case, spaces and dashes.
func SameInstance(a, b string) bool {
	strip := func(s string) string {
		s = strings.ToUpper(strings.TrimSpace(s))
		return strings.NewReplacer("-", "", " ", "").Replace(s)
	}
	return a != "" && strip(a) == strip(b)
}
