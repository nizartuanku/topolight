// Package cluster lets 2–5 TopoLight nodes run as one system: one leader
// holds the state engine and the canonical data, every full node keeps a
// mirrored copy and polls a share of the devices, and a majority of full
// nodes elects a new leader when the current one disappears.
//
// Design in one paragraph: nodes talk over a dedicated mTLS port using a
// cluster CA created on the first node; a join token (which carries the CA
// fingerprint) lets a new node obtain its certificate. The leader publishes
// its data directory as a file manifest; standbys mirror it every few
// seconds (append-only journals by tail, everything else whole). Standbys
// and collectors poll their assigned devices and forward samples, logs,
// traps and flows to the leader. Leadership is a lease renewed by heartbeat
// acknowledgements from a majority; a node that wins an election, or a
// leader that loses its majority, re-executes itself in the new role — the
// startup path is the only path, which keeps the failure modes few.
package cluster

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Role of a node in the cluster.
const (
	RoleFull      = "full"      // keeps a data copy, votes, can become leader
	RoleCollector = "collector" // polls and forwards only
)

// Member is one node as recorded in the replicated membership list.
type Member struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Addr     string    `json:"addr"`    // https://host:8434 (cluster port)
	Console  string    `json:"console"` // http(s)://host:8433 (for the UI proxy)
	Role     string    `json:"role"`
	Joined   time.Time `json:"joined"`
	CertFP   string    `json:"cert_fp,omitempty"`
	Removed  bool      `json:"removed,omitempty"`
	LastSeen time.Time `json:"-"`
}

// Token is a join token (only the hash is stored).
type Token struct {
	Hash    string    `json:"hash"`
	Role    string    `json:"role"`
	Expires time.Time `json:"expires"`
	Created time.Time `json:"created"`
	Used    string    `json:"used,omitempty"` // node id that redeemed it
}

// Identity is the persistent per-node cluster state (<data>/cluster/node.json).
type Identity struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Role      string            `json:"role"`
	Addr      string            `json:"addr"`
	Console   string            `json:"console"`
	Enabled   bool              `json:"enabled"` // part of a cluster (even a cluster of one)
	Members   []Member          `json:"members"`
	Tokens    []Token           `json:"tokens,omitempty"`
	Term      uint64            `json:"term"`
	VotedFor  string            `json:"voted_for,omitempty"`
	LeaderID  string            `json:"leader_id,omitempty"` // last known
	WasLeader bool              `json:"was_leader"`          // role at last shutdown / re-exec
	SitePins  map[string]string `json:"site_pins,omitempty"` // site id → node id
	// PEM material
	CACert   string `json:"ca_cert,omitempty"`
	CAKey    string `json:"ca_key,omitempty"` // only the node that created the cluster keeps it… and every node that joins (so any node can admit others after failover)
	NodeCert string `json:"node_cert,omitempty"`
	NodeKey  string `json:"node_key,omitempty"`

	mu   sync.Mutex
	path string
}

// LoadIdentity reads or initialises <dir>/cluster/node.json.
func LoadIdentity(dir, name string) (*Identity, error) {
	id := &Identity{Role: RoleFull, SitePins: map[string]string{}}
	if dir == "" {
		id.ID = newID()
		id.Name = name
		return id, nil
	}
	id.path = filepath.Join(dir, "cluster", "node.json")
	b, err := os.ReadFile(id.path)
	if err == nil {
		if err := json.Unmarshal(b, id); err != nil {
			return nil, fmt.Errorf("cluster/node.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if id.ID == "" {
		id.ID = newID()
	}
	if id.Name == "" {
		id.Name = name
	}
	if id.Role == "" {
		id.Role = RoleFull
	}
	if id.SitePins == nil {
		id.SitePins = map[string]string{}
	}
	return id, id.Save()
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "node_" + hex.EncodeToString(b)
}

// Save persists the identity atomically.
func (id *Identity) Save() error {
	if id.path == "" {
		return nil
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(id.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(id, "", " ")
	if err != nil {
		return err
	}
	tmp := id.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, id.path)
}

// ---- PKI ----------------------------------------------------------------------------

// InitCA creates the cluster CA and this node's certificate (first node).
func (id *Identity) InitCA() error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "TopoLight cluster CA", Organization: []string{"TopoLight"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(20 * 365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	kb, _ := x509.MarshalECPrivateKey(caKey)
	id.CACert = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	id.CAKey = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
	cert, key, err := id.IssueNodeCert(id.ID, id.Addr)
	if err != nil {
		return err
	}
	id.NodeCert, id.NodeKey = cert, key
	id.Enabled = true
	return nil
}

// IssueNodeCert signs a certificate for a node id (needs the CA key). It
// generates the key pair too; the joining node receives both over the
// token-authenticated join call, which runs inside TLS pinned to the CA.
func (id *Identity) IssueNodeCert(nodeID, addr string) (certPEM, keyPEM string, err error) {
	caCert, caKey, err := id.ca()
	if err != nil {
		return "", "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: nodeID, Organization: []string{"TopoLight node"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames: []string{nodeID}}
	if host := hostOf(addr); host != "" {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return "", "", err
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})), nil
}

func (id *Identity) ca() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if id.CACert == "" || id.CAKey == "" {
		return nil, nil, errors.New("cluster: no CA on this node")
	}
	cb, _ := pem.Decode([]byte(id.CACert))
	kb, _ := pem.Decode([]byte(id.CAKey))
	if cb == nil || kb == nil {
		return nil, nil, errors.New("cluster: bad CA pem")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// CAFingerprint is the SHA-256 of the CA certificate (hex, first 16 bytes),
// embedded in join tokens so joiners can pin the CA before trusting anything.
func (id *Identity) CAFingerprint() string {
	cb, _ := pem.Decode([]byte(id.CACert))
	if cb == nil {
		return ""
	}
	sum := sha256.Sum256(cb.Bytes)
	return hex.EncodeToString(sum[:16])
}

// TLSServer builds the mTLS server config: our node cert, clients must
// present a certificate from the cluster CA (join is the exception and is
// handled by the handler, which checks the token instead).
func (id *Identity) TLSServer() (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(id.NodeCert), []byte(id.NodeKey))
	if err != nil {
		return nil, err
	}
	// present the CA too, so a joining node can pin it by the fingerprint in its token
	if cb, _ := pem.Decode([]byte(id.CACert)); cb != nil {
		cert.Certificate = append(cert.Certificate, cb.Bytes)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(id.CACert))
	return &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// TLSClient builds the client config for talking to other nodes.
func (id *Identity) TLSClient() (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(id.NodeCert), []byte(id.NodeKey))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(id.CACert))
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		VerifyPeerCertificate: verifyAgainst(pool)}, nil
}

// verifyAgainst checks the chain against the cluster CA only, ignoring host
// names (nodes are addressed by whatever IP the admin gave, and certificates
// are pinned to the CA, not to names).
func verifyAgainst(pool *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("cluster: no peer certificate")
		}
		cert, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		inter := x509.NewCertPool()
		for _, r := range raw[1:] {
			if c, err := x509.ParseCertificate(r); err == nil {
				inter.AddCert(c)
			}
		}
		_, err = cert.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		return err
	}
}

// ---- join tokens --------------------------------------------------------------------

// NewToken mints a join token valid for ttl: "TL-JOIN-<caFP>-<role>-<secret>".
func (id *Identity) NewToken(role string, ttl time.Duration) (string, error) {
	if role != RoleCollector {
		role = RoleFull
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(secret))
	id.mu.Lock()
	// drop expired tokens
	var keep []Token
	for _, t := range id.Tokens {
		if time.Now().Before(t.Expires) && t.Used == "" {
			keep = append(keep, t)
		}
	}
	id.Tokens = append(keep, Token{Hash: hex.EncodeToString(sum[:]), Role: role, Expires: time.Now().Add(ttl), Created: time.Now()})
	id.mu.Unlock()
	if err := id.Save(); err != nil {
		return "", err
	}
	return "TL-JOIN-" + id.CAFingerprint() + "-" + role + "-" + secret, nil
}

// ParseToken splits a token into CA fingerprint, role and secret.
func ParseToken(tok string) (caFP, role, secret string, err error) {
	tok = strings.TrimSpace(tok)
	if !strings.HasPrefix(tok, "TL-JOIN-") {
		return "", "", "", errors.New("not a join token")
	}
	parts := strings.SplitN(strings.TrimPrefix(tok, "TL-JOIN-"), "-", 3)
	if len(parts) != 3 || len(parts[0]) != 32 {
		return "", "", "", errors.New("malformed join token")
	}
	return parts[0], parts[1], parts[2], nil
}

// RedeemToken validates a secret and marks it used.
func (id *Identity) RedeemToken(secret, nodeID string) (role string, ok bool) {
	sum := sha256.Sum256([]byte(secret))
	h := hex.EncodeToString(sum[:])
	id.mu.Lock()
	defer id.mu.Unlock()
	for i := range id.Tokens {
		t := &id.Tokens[i]
		if t.Hash == h && t.Used == "" && time.Now().Before(t.Expires) {
			t.Used = nodeID
			return t.Role, true
		}
	}
	return "", false
}

// ---- membership -----------------------------------------------------------------------

// Upsert adds or updates a member.
func (id *Identity) Upsert(m Member) {
	id.mu.Lock()
	defer id.mu.Unlock()
	for i := range id.Members {
		if id.Members[i].ID == m.ID {
			ls := id.Members[i].LastSeen
			id.Members[i] = m
			if id.Members[i].LastSeen.IsZero() {
				id.Members[i].LastSeen = ls
			}
			return
		}
	}
	id.Members = append(id.Members, m)
}

// MemberList returns a copy of the membership (removed nodes excluded).
func (id *Identity) MemberList() []Member {
	id.mu.Lock()
	defer id.mu.Unlock()
	out := make([]Member, 0, len(id.Members))
	for _, m := range id.Members {
		if !m.Removed {
			out = append(out, m)
		}
	}
	return out
}

// Self returns this node's member record.
func (id *Identity) Self() Member {
	return Member{ID: id.ID, Name: id.Name, Addr: id.Addr, Console: id.Console, Role: id.Role, Joined: time.Now()}
}

// Voters counts full members (the electorate).
func (id *Identity) Voters() int {
	n := 0
	for _, m := range id.MemberList() {
		if m.Role == RoleFull {
			n++
		}
	}
	return n
}

func hostOf(addr string) string {
	addr = strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		addr = addr[:i]
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
