package cluster

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Batch is forwarded data from a standby/collector to the leader. Payloads
// are opaque JSON so the cluster package does not depend on the model.
type Batch struct {
	From    string            `json:"from"`
	Devices []json.RawMessage `json:"devices,omitempty"`
	Ifaces  []json.RawMessage `json:"ifaces,omitempty"`
	Events  []json.RawMessage `json:"events,omitempty"`
	Metrics []Metric          `json:"metrics,omitempty"`
	Logs    []LogLine         `json:"logs,omitempty"`
	Traps   []Datagram        `json:"traps,omitempty"`
	Flows   []Datagram        `json:"flows,omitempty"`
}

// Metric is one sample.
type Metric struct {
	S string  `json:"s"`
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// LogLine is one syslog line with its source.
type LogLine struct {
	Host string `json:"h"`
	Raw  string `json:"r"`
}

// Datagram is a raw UDP payload with its source (traps, flows).
type Datagram struct {
	From  string `json:"f"`
	SFlow bool   `json:"s,omitempty"`
	Data  []byte `json:"d"`
}

func (n *Node) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /cluster/join", n.handleJoin)
	mux.HandleFunc("POST /cluster/heartbeat", n.auth(n.handleHeartbeat))
	mux.HandleFunc("POST /cluster/vote", n.auth(n.handleVote))
	mux.HandleFunc("GET /cluster/status", n.auth(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, n.Status()) }))
	mux.HandleFunc("GET /cluster/manifest", n.auth(n.handleManifest))
	mux.HandleFunc("GET /cluster/file", n.auth(n.handleFile))
	mux.HandleFunc("POST /cluster/ingest", n.auth(n.handleIngest))
	mux.HandleFunc("POST /cluster/members", n.auth(n.handleMembers))
}

// auth requires a client certificate from the cluster CA.
func (n *Node) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(n.ID.CACert))
		if _, err := r.TLS.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			http.Error(w, "certificate not issued by the cluster CA", http.StatusForbidden)
			return
		}
		// the certificate's CN is the node id; removed nodes are shut out
		peer := r.TLS.PeerCertificates[0].Subject.CommonName
		n.ID.mu.Lock()
		for _, m := range n.ID.Members {
			if m.ID == peer && m.Removed {
				n.ID.mu.Unlock()
				http.Error(w, "this node was removed from the cluster", http.StatusForbidden)
				return
			}
		}
		n.ID.mu.Unlock()
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---- join ------------------------------------------------------------------------------

type joinReq struct {
	Secret  string `json:"secret"`
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	Addr    string `json:"addr"`
	Console string `json:"console"`
}

type joinResp struct {
	CACert   string            `json:"ca_cert"`
	CAKey    string            `json:"ca_key,omitempty"`
	NodeCert string            `json:"node_cert"`
	NodeKey  string            `json:"node_key"`
	Role     string            `json:"role"`
	Members  []Member          `json:"members"`
	LeaderID string            `json:"leader_id"`
	Pins     map[string]string `json:"pins,omitempty"`
}

func (n *Node) handleJoin(w http.ResponseWriter, r *http.Request) {
	var in joinReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if n.ID.CAKey == "" {
		http.Error(w, "this node cannot admit members (no CA key) — join through a full node", 403)
		return
	}
	role, ok := n.ID.RedeemToken(in.Secret, in.NodeID)
	if !ok {
		http.Error(w, "join token invalid, expired or already used", 403)
		return
	}
	cert, key, err := n.ID.IssueNodeCert(in.NodeID, in.Addr)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	m := Member{ID: in.NodeID, Name: in.Name, Addr: in.Addr, Console: in.Console, Role: role, Joined: time.Now(), CertFP: fingerprint(cert)}
	n.ID.Upsert(m)
	_ = n.ID.Save()
	n.mu.Lock()
	leader := n.leaderID
	n.mu.Unlock()
	resp := joinResp{CACert: n.ID.CACert, NodeCert: cert, NodeKey: key, Role: role, Members: n.ID.MemberList(), LeaderID: leader, Pins: n.ID.SitePins}
	if role == RoleFull {
		resp.CAKey = n.ID.CAKey // every full node can admit others after a failover
	}
	log.Printf("cluster: admitted %s (%s, %s) as %s", in.Name, in.NodeID, in.Addr, role)
	writeJSON(w, resp)
}

func fingerprint(certPEM string) string {
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil {
		return ""
	}
	sum := sha256.Sum256(b.Bytes)
	return hex.EncodeToString(sum[:8])
}

// Join contacts an existing node with a token and fills in the identity.
// The CA is pinned by the fingerprint inside the token, so the first
// connection needs no prior trust.
func Join(ctx context.Context, id *Identity, target, token string) error {
	caFP, role, secret, err := ParseToken(token)
	if err != nil {
		return err
	}
	tcfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
		// nodes send [leaf, CA]; pin the CA by the fingerprint in the token, then
		// check that the leaf really chains to it
		if len(raw) < 2 {
			return errors.New("cluster: server did not present the cluster CA")
		}
		pool := x509.NewCertPool()
		found := false
		for _, r := range raw[1:] {
			sum := sha256.Sum256(r)
			if hex.EncodeToString(sum[:16]) == caFP {
				if c, err := x509.ParseCertificate(r); err == nil {
					pool.AddCert(c)
					found = true
				}
			}
		}
		if !found {
			return errors.New("cluster: the CA presented does not match the join token — wrong cluster or forged token")
		}
		leaf, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		return err
	}}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: tcfg}}
	id.Role = role
	body, _ := json.Marshal(joinReq{Secret: secret, NodeID: id.ID, Name: id.Name, Addr: id.Addr, Console: id.Console})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(target, "/")+"/cluster/join", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("join refused: %s", strings.TrimSpace(string(msg)))
	}
	var jr joinResp
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		return err
	}
	id.CACert, id.CAKey, id.NodeCert, id.NodeKey, id.Role = jr.CACert, jr.CAKey, jr.NodeCert, jr.NodeKey, jr.Role
	id.Members = jr.Members
	id.LeaderID = jr.LeaderID
	id.SitePins = jr.Pins
	if id.SitePins == nil {
		id.SitePins = map[string]string{}
	}
	id.Enabled, id.WasLeader = true, false
	id.Upsert(id.Self())
	return id.Save()
}

// ---- heartbeat / vote ----------------------------------------------------------------------

func (n *Node) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var in hbReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&in); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	n.mu.Lock()
	if in.Term < n.term {
		resp := hbResp{Term: n.term, State: n.state, Mode: n.Mode, Version: n.Hooks.Version}
		n.mu.Unlock()
		writeJSON(w, resp)
		return
	}
	if in.Term > n.term || n.state != StateFollower {
		if n.state == StateLeader && in.LeaderID != n.ID.ID {
			n.stepDownLocked("another leader (" + in.LeaderID + ") is sending heartbeats")
		}
		n.term, n.votedFor, n.state = in.Term, "", StateFollower
		n.ID.Term = in.Term
	}
	n.leaderID = in.LeaderID
	n.ID.LeaderID = in.LeaderID
	n.lastHB = time.Now()
	n.assigned = in.Assign
	resp := hbResp{Term: n.term, State: n.state, Mode: n.Mode, Version: n.Hooks.Version, DataTS: n.dataTS()}
	if !n.mirrorAt.IsZero() {
		resp.MirrorAge = time.Since(n.mirrorAt).Seconds()
	}
	if n.Hooks.Queue != nil {
		resp.Queue = n.Hooks.Queue()
	}
	n.mu.Unlock()
	// membership and pins follow the leader
	for _, m := range in.Members {
		n.ID.Upsert(m)
	}
	if in.Pins != nil {
		n.ID.SitePins = in.Pins
	}
	_ = n.ID.Save()
	if n.Hooks.OnAssign != nil {
		n.Hooks.OnAssign(in.Assign, in.Pins)
	}
	writeJSON(w, resp)
}

func (n *Node) handleVote(w http.ResponseWriter, r *http.Request) {
	var in voteReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	resp := voteResp{Term: n.term}
	if in.Term < n.term {
		writeJSON(w, resp)
		return
	}
	if in.Term > n.term {
		n.term, n.votedFor = in.Term, ""
		n.ID.Term, n.ID.VotedFor = in.Term, ""
		if n.state == StateLeader {
			n.stepDownLocked("vote request for a newer term")
		}
		n.state = StateFollower
	}
	// grant when we have not voted this term and the candidate's copy is not
	// clearly staler than ours (30 s of slack for mirror timing)
	mine := n.dataTS()
	fresh := in.DataTS.Add(30*time.Second).After(mine) || mine.IsZero()
	if (n.votedFor == "" || n.votedFor == in.Candidate) && fresh && n.ID.Role == RoleFull {
		n.votedFor = in.Candidate
		n.ID.VotedFor = in.Candidate
		n.lastHB = time.Now() // do not start our own election right after voting
		resp.Granted = true
	}
	resp.Term = n.term
	_ = n.ID.Save()
	writeJSON(w, resp)
}

// ---- members (admin actions relayed to the leader) ------------------------------------

type membersReq struct {
	Remove string            `json:"remove,omitempty"`
	Pins   map[string]string `json:"pins,omitempty"`
}

func (n *Node) handleMembers(w http.ResponseWriter, r *http.Request) {
	var in membersReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if in.Remove != "" {
		n.RemoveMember(in.Remove)
	}
	if in.Pins != nil {
		n.ID.SitePins = in.Pins
		_ = n.ID.Save()
	}
	writeJSON(w, map[string]any{"ok": true})
}

// RemoveMember marks a node removed (it stops receiving heartbeats and
// assignments; its certificate is not revoked — reinstall the node to re-join).
func (n *Node) RemoveMember(id string) {
	n.ID.mu.Lock()
	for i := range n.ID.Members {
		if n.ID.Members[i].ID == id {
			n.ID.Members[i].Removed = true
		}
	}
	n.ID.mu.Unlock()
	_ = n.ID.Save()
}

// ---- manifest / file ---------------------------------------------------------------------

// FileInfo is one entry of the data manifest.
type FileInfo struct {
	Path  string `json:"p"`
	Size  int64  `json:"s"`
	MTime int64  `json:"m"` // unix nanoseconds
}

// excluded files are per-node and never mirrored.
func mirrored(rel string) bool {
	if strings.HasPrefix(rel, "cluster/") || strings.HasSuffix(rel, ".tmp") || strings.HasPrefix(rel, "syslog-tls.") || strings.HasSuffix(rel, ".corrupt") {
		return false
	}
	if strings.Contains(rel, ".corrupt-") {
		return false
	}
	return true
}

// Manifest walks the data directory.
func Manifest(dir string) ([]FileInfo, error) {
	var out []FileInfo
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !mirrored(rel) {
			return nil
		}
		out = append(out, FileInfo{Path: rel, Size: info.Size(), MTime: info.ModTime().UnixNano()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

func (n *Node) handleManifest(w http.ResponseWriter, r *http.Request) {
	if n.Hooks.DataDir == "" {
		http.Error(w, "no data directory", 404)
		return
	}
	m, err := Manifest(n.Hooks.DataDir)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, m)
}

func (n *Node) handleFile(w http.ResponseWriter, r *http.Request) {
	rel := filepath.ToSlash(filepath.Clean("/" + r.URL.Query().Get("path")))[1:]
	if rel == "" || !mirrored(rel) || strings.Contains(rel, "..") {
		http.Error(w, "bad path", 400)
		return
	}
	off, _ := strconv.ParseInt(r.URL.Query().Get("off"), 10, 64)
	f, err := os.Open(filepath.Join(n.Hooks.DataDir, filepath.FromSlash(rel)))
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	defer f.Close()
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, f)
}

// ---- ingest --------------------------------------------------------------------------------

func (n *Node) handleIngest(w http.ResponseWriter, r *http.Request) {
	if !n.IsLeader() || n.Hooks.Ingest == nil {
		http.Error(w, "not the leader", http.StatusConflict)
		return
	}
	var b Batch
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&b); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if err := n.Hooks.Ingest(b); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
