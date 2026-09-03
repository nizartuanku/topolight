package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Timing (seconds) — small enough that failover completes in ~20 s, large
// enough that a busy leader is not deposed by one slow heartbeat.
var (
	heartbeatEvery  = 2 * time.Second
	leaseWindow     = 10 * time.Second
	electionMin     = 10 * time.Second
	electionJitter  = 5 * time.Second
	memberDeadAfter = 15 * time.Second
)

// State of the in-process election machine.
const (
	StateFollower  = "follower"
	StateCandidate = "candidate"
	StateLeader    = "leader"
)

// Status is what the UI and other nodes see about this node.
type Status struct {
	NodeID     string                   `json:"node_id"`
	Name       string                   `json:"name"`
	Role       string                   `json:"role"`
	State      string                   `json:"state"` // follower|candidate|leader
	Mode       string                   `json:"mode"`  // leader|standby|collector (process mode)
	Term       uint64                   `json:"term"`
	LeaderID   string                   `json:"leader_id"`
	Version    string                   `json:"version"`
	DataTS     time.Time                `json:"data_ts"`    // mtime of state.json (freshness of the copy)
	MirrorAge  float64                  `json:"mirror_age"` // seconds since last successful sync (standby)
	Assigned   int                      `json:"assigned"`   // devices polled here
	Queue      int                      `json:"queue"`      // forwarder backlog
	Load1      float64                  `json:"load1,omitempty"`
	Uptime     float64                  `json:"uptime_s"`
	Hostname   string                   `json:"hostname"`
	Members    []Member                 `json:"members,omitempty"`
	MemberStat map[string]*MemberStatus `json:"member_status,omitempty"`
}

// MemberStatus is the leader's view of one member.
type MemberStatus struct {
	Alive     bool      `json:"alive"`
	LastSeen  time.Time `json:"last_seen"`
	State     string    `json:"state,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Version   string    `json:"version,omitempty"`
	MirrorAge float64   `json:"mirror_age"`
	DataTS    time.Time `json:"data_ts"`
	Assigned  int       `json:"assigned"`
	Queue     int       `json:"queue"`
	RTTms     float64   `json:"rtt_ms"`
}

// Hooks connect the node to the rest of the process.
type Hooks struct {
	// Version string reported to peers.
	Version string
	// DataTS reports the freshness of the local data copy (state.json mtime).
	DataTS func() time.Time
	// Assigned reports how many devices this node polls; Queue the forwarder backlog.
	Assigned func() int
	Queue    func() int
	// OnAssign receives the device ids this node must poll (nil = all, when standalone).
	OnAssign func(ids []string, pins map[string]string)
	// Promote / Demote are called once when the role must change; the callee
	// persists nothing (the node already did) and restarts the process.
	Promote func()
	Demote  func()
	// Ingest handles forwarded data on the leader.
	Ingest func(b Batch) error
	// Devices lists monitored devices (id, site) for sharding on the leader.
	Devices func() []DeviceRef
	// Manifest lists the data directory on the leader; Open reads a file.
	DataDir string
}

// DeviceRef is what sharding needs.
type DeviceRef struct {
	ID   string
	Site string
}

// Node is the cluster participant.
type Node struct {
	ID    *Identity
	Hooks Hooks
	Mode  string // leader|standby|collector

	mu       sync.Mutex
	state    string
	term     uint64
	leaderID string
	votedFor string
	lastHB   time.Time // last heartbeat from a leader (followers)
	acks     map[string]time.Time
	status   map[string]*MemberStatus
	assign   map[string][]string // leader: node → device ids
	started  time.Time
	client   *http.Client
	server   *http.Server
	mirrorAt time.Time
	stepping bool
	fw       *Forwarder
	syncErr  string
	assigned []string
}

// New builds a node in the given process mode.
func New(id *Identity, mode string, hooks Hooks) (*Node, error) {
	n := &Node{ID: id, Hooks: hooks, Mode: mode, state: StateFollower, term: id.Term, votedFor: id.VotedFor, leaderID: id.LeaderID,
		acks: map[string]time.Time{}, status: map[string]*MemberStatus{}, assign: map[string][]string{}, started: time.Now()}
	tcfg, err := id.TLSClient()
	if err != nil {
		return nil, err
	}
	n.client = &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{TLSClientConfig: tcfg, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second}}
	return n, nil
}

// Client returns the mTLS HTTP client (used by the forwarder and the sync loop).
func (n *Node) Client() *http.Client { return n.client }

// Serve binds the cluster port and runs the election / heartbeat loops until ctx ends.
func (n *Node) Serve(ctx context.Context, listen string) error {
	tcfg, err := n.ID.TLSServer()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	n.routes(mux)
	n.server = &http.Server{Addr: listen, Handler: mux, TLSConfig: tcfg, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		n.server.Close()
	}()
	go n.loop(ctx)
	if err := n.server.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Leader returns the current leader member, if known and alive.
func (n *Node) Leader() (Member, bool) {
	n.mu.Lock()
	id := n.leaderID
	n.mu.Unlock()
	if id == "" {
		return Member{}, false
	}
	for _, m := range n.ID.MemberList() {
		if m.ID == id {
			return m, true
		}
	}
	return Member{}, false
}

// IsLeader reports whether this node currently holds the lease.
func (n *Node) IsLeader() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state == StateLeader
}

// Status snapshot for the API.
func (n *Node) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := Status{NodeID: n.ID.ID, Name: n.ID.Name, Role: n.ID.Role, State: n.state, Mode: n.Mode, Term: n.term, LeaderID: n.leaderID,
		Version: n.Hooks.Version, Uptime: time.Since(n.started).Seconds(), Assigned: len(n.assigned)}
	s.Hostname, _ = os.Hostname()
	if n.Hooks.DataTS != nil {
		s.DataTS = n.Hooks.DataTS()
	}
	if n.Hooks.Queue != nil {
		s.Queue = n.Hooks.Queue()
	}
	if !n.mirrorAt.IsZero() {
		s.MirrorAge = time.Since(n.mirrorAt).Seconds()
	}
	if n.state == StateLeader {
		s.Members = n.ID.MemberList()
		s.MemberStat = map[string]*MemberStatus{}
		for k, v := range n.status {
			c := *v
			c.Alive = time.Since(v.LastSeen) < memberDeadAfter
			c.Assigned = len(n.assign[k])
			s.MemberStat[k] = &c
		}
	}
	return s
}

// ---- main loop -----------------------------------------------------------------------

func (n *Node) loop(ctx context.Context) {
	// a node that was leader when it stopped tries to reclaim leadership
	// immediately; everyone else waits a full election timeout first
	timeout := electionMin + time.Duration(rand.Int63n(int64(electionJitter)))
	if n.Mode == "leader" && n.ID.Role == RoleFull {
		// a returning leader must not steal the lease from a leader elected
		// while it was away: ask the members first
		if other, term := n.findLeader(ctx); other != "" && other != n.ID.ID {
			n.observeTerm(term, other)
			n.mu.Lock()
			n.stepDownLocked("another leader (" + other + ") is active")
			n.mu.Unlock()
			return
		}
		timeout = 500 * time.Millisecond
	}
	n.mu.Lock()
	n.lastHB = time.Now()
	n.mu.Unlock()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	lastBeat := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n.mu.Lock()
			st := n.state
			since := now.Sub(n.lastHB)
			n.mu.Unlock()
			switch st {
			case StateLeader:
				if now.Sub(lastBeat) >= heartbeatEvery {
					lastBeat = now
					n.beat(ctx)
				}
			default:
				if n.ID.Role != RoleFull {
					continue // collectors never run for office
				}
				if since >= timeout {
					n.mu.Lock()
					n.lastHB = now
					n.mu.Unlock()
					timeout = electionMin + time.Duration(rand.Int63n(int64(electionJitter)))
					n.campaign(ctx)
				}
			}
		}
	}
}

// findLeader asks every member who leads; returns the leader id and term.
func (n *Node) findLeader(ctx context.Context) (string, uint64) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, m := range n.ID.MemberList() {
		if m.ID == n.ID.ID {
			continue
		}
		req, err := http.NewRequestWithContext(cctx, "GET", strings.TrimRight(m.Addr, "/")+"/cluster/status", nil)
		if err != nil {
			continue
		}
		resp, err := n.client.Do(req)
		if err != nil {
			continue
		}
		var st Status
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if st.State == StateLeader {
			return st.NodeID, st.Term
		}
	}
	return "", 0
}

// campaign runs one election round.
func (n *Node) campaign(ctx context.Context) {
	voters := 0
	for _, m := range n.ID.MemberList() {
		if m.Role == RoleFull {
			voters++
		}
	}
	if voters == 0 {
		voters = 1
	}
	n.mu.Lock()
	n.state = StateCandidate
	n.term++
	n.votedFor = n.ID.ID
	term := n.term
	n.mu.Unlock()
	n.ID.Term, n.ID.VotedFor = term, n.ID.ID
	_ = n.ID.Save()
	req := voteReq{Term: term, Candidate: n.ID.ID, DataTS: n.dataTS()}
	votes := 1
	var wg sync.WaitGroup
	var vm sync.Mutex
	for _, m := range n.ID.MemberList() {
		if m.ID == n.ID.ID || m.Role != RoleFull {
			continue
		}
		wg.Add(1)
		go func(m Member) {
			defer wg.Done()
			var resp voteResp
			if err := n.post(ctx, m.Addr, "/cluster/vote", req, &resp); err != nil {
				return
			}
			vm.Lock()
			defer vm.Unlock()
			if resp.Granted {
				votes++
			}
			if resp.Term > term {
				n.observeTerm(resp.Term, "")
			}
		}(m)
	}
	wg.Wait()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state != StateCandidate || n.term != term {
		return
	}
	if votes*2 > voters {
		n.state = StateLeader
		n.leaderID = n.ID.ID
		n.acks = map[string]time.Time{n.ID.ID: time.Now()}
		log.Printf("cluster: elected leader for term %d with %d/%d votes", term, votes, voters)
		if n.Mode != "leader" && n.Hooks.Promote != nil && !n.stepping {
			n.stepping = true
			n.ID.LeaderID, n.ID.WasLeader = n.ID.ID, true
			_ = n.ID.Save()
			go n.Hooks.Promote()
		}
	} else {
		n.state = StateFollower
		log.Printf("cluster: election for term %d lost (%d/%d votes)", term, votes, voters)
	}
}

// observeTerm steps down when a higher term (or another leader) shows up.
func (n *Node) observeTerm(term uint64, leader string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if term > n.term {
		n.term, n.votedFor = term, ""
		n.ID.Term, n.ID.VotedFor = term, ""
		_ = n.ID.Save()
		if n.state == StateLeader {
			n.stepDownLocked("a newer term exists")
		} else {
			n.state = StateFollower
		}
	}
	if leader != "" {
		n.leaderID = leader
		n.ID.LeaderID = leader
	}
}

func (n *Node) stepDownLocked(why string) {
	n.state = StateFollower
	log.Printf("cluster: stepping down: %s", why)
	if n.Mode == "leader" && n.Hooks.Demote != nil && !n.stepping {
		n.stepping = true
		n.ID.WasLeader = false
		_ = n.ID.Save()
		go n.Hooks.Demote()
	}
}

// beat sends a heartbeat to every member and checks the lease.
func (n *Node) beat(ctx context.Context) {
	n.reshard()
	n.mu.Lock()
	term := n.term
	members := n.ID.MemberList()
	pins := n.ID.SitePins
	n.mu.Unlock()
	var wg sync.WaitGroup
	for _, m := range members {
		if m.ID == n.ID.ID {
			continue
		}
		wg.Add(1)
		go func(m Member) {
			defer wg.Done()
			n.mu.Lock()
			req := hbReq{Term: term, LeaderID: n.ID.ID, Members: members, Assign: n.assign[m.ID], Pins: pins}
			n.mu.Unlock()
			var resp hbResp
			t0 := time.Now()
			err := n.post(ctx, m.Addr, "/cluster/heartbeat", req, &resp)
			n.mu.Lock()
			defer n.mu.Unlock()
			st := n.status[m.ID]
			if st == nil {
				st = &MemberStatus{}
				n.status[m.ID] = st
			}
			if err != nil {
				return
			}
			st.LastSeen, st.State, st.Mode, st.Version, st.MirrorAge, st.DataTS, st.Queue, st.RTTms = time.Now(), resp.State, resp.Mode, resp.Version, resp.MirrorAge, resp.DataTS, resp.Queue, float64(time.Since(t0).Microseconds())/1000
			if resp.Term > term {
				go n.observeTerm(resp.Term, "")
				return
			}
			if m.Role == RoleFull {
				n.acks[m.ID] = time.Now()
			}
		}(m)
	}
	wg.Wait()
	// lease check: a majority of full members must have acked within the window
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state != StateLeader {
		return
	}
	voters, ok := 0, 0
	for _, m := range members {
		if m.Role != RoleFull {
			continue
		}
		voters++
		if m.ID == n.ID.ID || time.Since(n.acks[m.ID]) < leaseWindow {
			ok++
		}
	}
	if ok*2 <= voters {
		n.stepDownLocked(fmt.Sprintf("lost quorum (%d of %d full nodes answering)", ok, voters))
	}
}

func (n *Node) dataTS() time.Time {
	if n.Hooks.DataTS != nil {
		return n.Hooks.DataTS()
	}
	return time.Time{}
}

// ---- sharding ---------------------------------------------------------------------------

// reshard assigns every monitored device to a live node: a pinned site goes
// to its node when alive, everything else by rendezvous hashing over the
// live nodes (stable: adding a node moves only ~1/N of the devices).
func (n *Node) reshard() {
	if n.Hooks.Devices == nil {
		return
	}
	n.mu.Lock()
	members := n.ID.MemberList()
	var live []string
	for _, m := range members {
		if m.ID == n.ID.ID || time.Since(n.status[m.ID].lastSeen()) < memberDeadAfter {
			live = append(live, m.ID)
		}
	}
	pins := map[string]string{}
	for k, v := range n.ID.SitePins {
		pins[k] = v
	}
	n.mu.Unlock()
	sort.Strings(live)
	assign := map[string][]string{}
	alive := map[string]bool{}
	for _, id := range live {
		alive[id] = true
	}
	for _, d := range n.Hooks.Devices() {
		target := ""
		if p := pins[d.Site]; p != "" && alive[p] {
			target = p
		} else {
			var best uint64
			for _, id := range live {
				h := fnv64(id + "|" + d.ID)
				if target == "" || h > best {
					target, best = id, h
				}
			}
		}
		assign[target] = append(assign[target], d.ID)
	}
	n.mu.Lock()
	n.assign = assign
	mine := assign[n.ID.ID]
	n.assigned = mine
	n.mu.Unlock()
	if n.Hooks.OnAssign != nil {
		n.Hooks.OnAssign(mine, pins)
	}
}

func (ms *MemberStatus) lastSeen() time.Time {
	if ms == nil {
		return time.Time{}
	}
	return ms.LastSeen
}

func fnv64(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	// FNV alone leaves the high bits dominated by the first bytes; finish with
	// a murmur3 fmix so rendezvous comparisons are fair
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// ---- wire types ------------------------------------------------------------------------

type voteReq struct {
	Term      uint64    `json:"term"`
	Candidate string    `json:"candidate"`
	DataTS    time.Time `json:"data_ts"`
}

type voteResp struct {
	Term    uint64 `json:"term"`
	Granted bool   `json:"granted"`
}

type hbReq struct {
	Term     uint64            `json:"term"`
	LeaderID string            `json:"leader_id"`
	Members  []Member          `json:"members"`
	Assign   []string          `json:"assign"`
	Pins     map[string]string `json:"pins,omitempty"`
}

type hbResp struct {
	Term      uint64    `json:"term"`
	State     string    `json:"state"`
	Mode      string    `json:"mode"`
	Version   string    `json:"version"`
	MirrorAge float64   `json:"mirror_age"`
	DataTS    time.Time `json:"data_ts"`
	Queue     int       `json:"queue"`
}

// post sends JSON to another node and decodes the reply.
func (n *Node) post(ctx context.Context, addr, path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(addr, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s: %s", addr, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// TLSDialer helper for tests.
func (n *Node) tlsConfig() *tls.Config { return n.client.Transport.(*http.Transport).TLSClientConfig }
