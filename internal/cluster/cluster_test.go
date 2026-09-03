package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func fastTimers(t *testing.T) {
	old := []time.Duration{heartbeatEvery, leaseWindow, electionMin, electionJitter, memberDeadAfter}
	heartbeatEvery, leaseWindow, electionMin, electionJitter, memberDeadAfter = 200*time.Millisecond, 1500*time.Millisecond, 1200*time.Millisecond, 600*time.Millisecond, 2*time.Second
	t.Cleanup(func() {
		heartbeatEvery, leaseWindow, electionMin, electionJitter, memberDeadAfter = old[0], old[1], old[2], old[3], old[4]
	})
}

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type testNode struct {
	id       *Identity
	node     *Node
	dir      string
	addr     string
	cancel   context.CancelFunc
	promoted atomic.Int32
	demoted  atomic.Int32
	ingested atomic.Int32
}

func startNode(t *testing.T, id *Identity, mode string, dir string) *testNode {
	tn := &testNode{id: id, dir: dir, addr: id.Addr}
	hooks := Hooks{Version: "test", DataDir: dir,
		DataTS: func() time.Time {
			fi, err := os.Stat(filepath.Join(dir, "state.json"))
			if err != nil {
				return time.Time{}
			}
			return fi.ModTime()
		},
		Devices: func() []DeviceRef {
			var out []DeviceRef
			for i := 0; i < 30; i++ {
				out = append(out, DeviceRef{ID: fmt.Sprintf("dev%02d", i), Site: fmt.Sprintf("site%d", i%3)})
			}
			return out
		},
		Promote: func() { tn.promoted.Add(1) },
		Demote:  func() { tn.demoted.Add(1) },
		Ingest:  func(b Batch) error { tn.ingested.Add(int32(len(b.Metrics))); return nil },
	}
	n, err := New(id, mode, hooks)
	if err != nil {
		t.Fatal(err)
	}
	tn.node = n
	ctx, cancel := context.WithCancel(context.Background())
	tn.cancel = cancel
	listen := id.Addr[len("https://"):]
	go func() {
		if err := n.Serve(ctx, listen); err != nil {
			t.Logf("serve %s: %v", id.Name, err)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	return tn
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestClusterJoinElectFailoverMirror(t *testing.T) {
	fastTimers(t)
	// node 1 initialises the cluster
	d1 := t.TempDir()
	os.WriteFile(filepath.Join(d1, "state.json"), []byte(`{"version":1}`), 0o600)
	os.MkdirAll(filepath.Join(d1, "events"), 0o700)
	os.WriteFile(filepath.Join(d1, "events", "2026-09-03.jsonl"), []byte("{\"a\":1}\n"), 0o600)
	id1, _ := LoadIdentity(d1, "n1")
	id1.Addr = "https://" + freePort(t)
	id1.Console = "http://127.0.0.1:1"
	if err := id1.InitCA(); err != nil {
		t.Fatal(err)
	}
	id1.WasLeader = true
	id1.Upsert(id1.Self())
	id1.Save()
	n1 := startNode(t, id1, "leader", d1)
	waitFor(t, "n1 leader", 5*time.Second, func() bool { return n1.node.IsLeader() })

	// token + join two more full nodes
	tok, err := id1.NewToken(RoleFull, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, role, _, err := ParseToken(tok); err != nil || role != RoleFull {
		t.Fatalf("token: %v %s", err, role)
	}
	var nodes []*testNode
	for i := 2; i <= 3; i++ {
		d := t.TempDir()
		id, _ := LoadIdentity(d, fmt.Sprintf("n%d", i))
		id.Addr = "https://" + freePort(t)
		if i == 2 {
			tok2 := tok
			if err := Join(context.Background(), id, id1.Addr, tok2); err != nil {
				t.Fatal(err)
			}
			// the same token cannot be used twice
			id3, _ := LoadIdentity(t.TempDir(), "dup")
			id3.Addr = "https://127.0.0.1:1"
			if err := Join(context.Background(), id3, id1.Addr, tok2); err == nil {
				t.Fatal("token reuse accepted")
			}
		} else {
			tok3, _ := id1.NewToken(RoleFull, time.Hour)
			if err := Join(context.Background(), id, id1.Addr, tok3); err != nil {
				t.Fatal(err)
			}
		}
		if !id.Enabled || id.NodeCert == "" || id.CAKey == "" || len(id.Members) < 2 {
			t.Fatalf("join state: %+v", id)
		}
		nodes = append(nodes, startNode(t, id, "standby", d))
	}
	n2, n3 := nodes[0], nodes[1]
	// forged token with the wrong fingerprint is refused before any secret is sent
	bad := "TL-JOIN-" + "00000000000000000000000000000000" + "-full-secret"
	idx, _ := LoadIdentity(t.TempDir(), "x")
	idx.Addr = "https://127.0.0.1:1"
	if err := Join(context.Background(), idx, id1.Addr, bad); err == nil {
		t.Fatal("wrong-cluster token accepted")
	}
	// the leader sees both members alive and shards devices across three nodes
	waitFor(t, "members alive", 5*time.Second, func() bool {
		s := n1.node.Status()
		return len(s.MemberStat) == 2 && s.MemberStat[n2.id.ID].Alive && s.MemberStat[n3.id.ID].Alive
	})
	waitFor(t, "assignments", 3*time.Second, func() bool {
		s := n1.node.Status()
		return s.Assigned > 0 && s.MemberStat[n2.id.ID].Assigned > 0 && s.MemberStat[n3.id.ID].Assigned > 0 && s.Assigned+s.MemberStat[n2.id.ID].Assigned+s.MemberStat[n3.id.ID].Assigned == 30
	})
	// a pinned site goes to one node
	n1.id.SitePins["site0"] = n2.id.ID
	waitFor(t, "pin", 3*time.Second, func() bool {
		n1.node.mu.Lock()
		defer n1.node.mu.Unlock()
		c := 0
		for _, d := range n1.node.assign[n2.id.ID] {
			if d == "dev00" || d == "dev03" || d == "dev06" {
				c++
			}
		}
		return c == 3
	})
	// followers know the leader
	waitFor(t, "followers know leader", 3*time.Second, func() bool {
		l2, ok2 := n2.node.Leader()
		l3, ok3 := n3.node.Leader()
		return ok2 && ok3 && l2.ID == id1.ID && l3.ID == id1.ID
	})
	// mirror: n2 pulls n1's files
	m := NewMirror(n2.node, n2.dir)
	if _, err := m.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(n2.dir, "state.json")); err != nil || string(b) != `{"version":1}` {
		t.Fatalf("mirror state.json: %v %q", err, b)
	}
	// append-only tail sync
	f, _ := os.OpenFile(filepath.Join(d1, "events", "2026-09-03.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("{\"a\":2}\n")
	f.Close()
	if _, err := m.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(n2.dir, "events", "2026-09-03.jsonl")); string(b) != "{\"a\":1}\n{\"a\":2}\n" {
		t.Fatalf("tail sync: %q", b)
	}
	// forwarder: metrics reach the leader's ingest hook
	fw := NewForwarder(n3.node)
	fw.Append("cpu|x", 1, 2)
	fw.Append("cpu|y", 1, 3)
	fw.Flush(context.Background())
	waitFor(t, "ingest", 3*time.Second, func() bool { return n1.ingested.Load() == 2 })
	// failover: stop the leader; one of the standbys must be promoted
	n1.cancel()
	waitFor(t, "new leader", 8*time.Second, func() bool {
		return n2.node.IsLeader() || n3.node.IsLeader()
	})
	if n2.promoted.Load()+n3.promoted.Load() != 1 {
		t.Fatalf("promotions: %d %d", n2.promoted.Load(), n3.promoted.Load())
	}
	var newLeader *testNode
	if n2.node.IsLeader() {
		newLeader = n2
	} else {
		newLeader = n3
	}
	// the other follower accepts the new leader
	other := n2
	if newLeader == n2 {
		other = n3
	}
	waitFor(t, "follower follows new leader", 3*time.Second, func() bool { l, ok := other.node.Leader(); return ok && l.ID == newLeader.id.ID })
	// with the third node gone too, the new leader loses quorum (1 of 3) and steps down
	other.cancel()
	waitFor(t, "step down without quorum", 6*time.Second, func() bool { return !newLeader.node.IsLeader() })
	if newLeader.demoted.Load() == 0 && newLeader.node.Mode == "leader" {
		t.Fatal("expected demote hook")
	}
	newLeader.cancel()
	_ = json.Marshal
}
