package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---- forwarder: standby/collector → leader --------------------------------------------

// Forwarder batches samples, events, metrics, logs, traps and flows and
// posts them to the leader; while no leader answers it keeps up to Max
// items in memory (oldest dropped beyond that).
type Forwarder struct {
	node                  *Node
	mu                    sync.Mutex
	b                     Batch
	n                     int
	Max                   int
	Sent, Dropped, Failed int64
	lastErr               string
}

// NewForwarder builds one (Max defaults to 200k items ≈ tens of MB).
func NewForwarder(n *Node) *Forwarder {
	f := &Forwarder{node: n, Max: 200000}
	n.fw = f
	return f
}

func (f *Forwarder) add(fn func(b *Batch)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n >= f.Max {
		// drop the oldest half of the cheap stuff first
		f.b.Metrics = f.b.Metrics[len(f.b.Metrics)/2:]
		f.b.Logs = f.b.Logs[len(f.b.Logs)/2:]
		f.b.Flows = f.b.Flows[len(f.b.Flows)/2:]
		f.Dropped++
		f.n = len(f.b.Devices) + len(f.b.Ifaces) + len(f.b.Events) + len(f.b.Metrics) + len(f.b.Logs) + len(f.b.Traps) + len(f.b.Flows)
	}
	fn(&f.b)
	f.n++
}

// Device / Iface / Event queue JSON-encodable samples.
func (f *Forwarder) Device(v any) {
	b, _ := json.Marshal(v)
	f.add(func(x *Batch) { x.Devices = append(x.Devices, b) })
}
func (f *Forwarder) Iface(v any) {
	b, _ := json.Marshal(v)
	f.add(func(x *Batch) { x.Ifaces = append(x.Ifaces, b) })
}
func (f *Forwarder) Event(v any) {
	b, _ := json.Marshal(v)
	f.add(func(x *Batch) { x.Events = append(x.Events, b) })
}

// Append satisfies the poller's metric sink.
func (f *Forwarder) Append(series string, t int64, v float64) {
	f.add(func(x *Batch) { x.Metrics = append(x.Metrics, Metric{S: series, T: t, V: v}) })
}

// Log queues a syslog line.
func (f *Forwarder) Log(host, raw string) {
	f.add(func(x *Batch) { x.Logs = append(x.Logs, LogLine{Host: host, Raw: raw}) })
}

// Trap / Flow queue raw datagrams.
func (f *Forwarder) Trap(from string, data []byte) {
	f.add(func(x *Batch) { x.Traps = append(x.Traps, Datagram{From: from, Data: append([]byte(nil), data...)}) })
}
func (f *Forwarder) Flow(from string, sflow bool, data []byte) {
	f.add(func(x *Batch) {
		x.Flows = append(x.Flows, Datagram{From: from, SFlow: sflow, Data: append([]byte(nil), data...)})
	})
}

// Queue length (for status).
func (f *Forwarder) Queue() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// Run flushes every second.
func (f *Forwarder) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.Flush(ctx)
		}
	}
}

// Flush posts the pending batch to the leader.
func (f *Forwarder) Flush(ctx context.Context) {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return
	}
	batch := f.b
	batch.From = f.node.ID.ID
	f.b = Batch{}
	f.n = 0
	f.mu.Unlock()
	leader, ok := f.node.Leader()
	if !ok || leader.ID == f.node.ID.ID {
		f.requeue(batch)
		return
	}
	if err := f.node.post(ctx, leader.Addr, "/cluster/ingest", batch, nil); err != nil {
		f.mu.Lock()
		f.Failed++
		f.lastErr = err.Error()
		f.mu.Unlock()
		f.requeue(batch)
		return
	}
	f.mu.Lock()
	f.Sent++
	f.lastErr = ""
	f.mu.Unlock()
}

func (f *Forwarder) requeue(b Batch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// put the failed batch back in front
	f.b.Devices = append(b.Devices, f.b.Devices...)
	f.b.Ifaces = append(b.Ifaces, f.b.Ifaces...)
	f.b.Events = append(b.Events, f.b.Events...)
	f.b.Metrics = append(b.Metrics, f.b.Metrics...)
	f.b.Logs = append(b.Logs, f.b.Logs...)
	f.b.Traps = append(b.Traps, f.b.Traps...)
	f.b.Flows = append(b.Flows, f.b.Flows...)
	f.n = len(f.b.Devices) + len(f.b.Ifaces) + len(f.b.Events) + len(f.b.Metrics) + len(f.b.Logs) + len(f.b.Traps) + len(f.b.Flows)
	if f.n > f.Max {
		f.b.Metrics = f.b.Metrics[len(f.b.Metrics)/2:]
		f.b.Logs = f.b.Logs[len(f.b.Logs)/2:]
		f.b.Flows = f.b.Flows[len(f.b.Flows)/2:]
		f.Dropped++
		f.n = len(f.b.Devices) + len(f.b.Ifaces) + len(f.b.Events) + len(f.b.Metrics) + len(f.b.Logs) + len(f.b.Traps) + len(f.b.Flows)
	}
}

// Stats for the UI.
func (f *Forwarder) Stats() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{"queue": f.n, "sent": f.Sent, "failed": f.Failed, "dropped": f.Dropped, "last_error": f.lastErr}
}

// ---- mirror: leader data directory → standby ---------------------------------------------

// Mirror keeps a standby's data directory in step with the leader's.
type Mirror struct {
	node  *Node
	dir   string
	Every time.Duration
	mu    sync.Mutex
	last  map[string]FileInfo
	Bytes int64
	Files int64
	Err   string
	// OnChange is called after a sync that changed state.json (the standby reloads its store).
	OnChange func(changed []string)
	// Only, when set, restricts the mirror to paths with these prefixes (collectors).
	Only []string
}

// NewMirror builds one for the local data directory.
func NewMirror(n *Node, dir string) *Mirror {
	return &Mirror{node: n, dir: dir, Every: 10 * time.Second, last: map[string]FileInfo{}}
}

// Run syncs until ctx ends.
func (m *Mirror) Run(ctx context.Context) {
	t := time.NewTicker(m.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if m.node.IsLeader() {
				continue
			}
			if _, err := m.SyncOnce(ctx); err != nil {
				m.mu.Lock()
				m.Err = err.Error()
				m.mu.Unlock()
			}
		}
	}
}

// SyncOnce pulls everything that differs from the leader. Returns the paths changed.
func (m *Mirror) SyncOnce(ctx context.Context) ([]string, error) {
	leader, ok := m.node.Leader()
	if !ok {
		return nil, fmt.Errorf("no leader known")
	}
	if leader.ID == m.node.ID.ID {
		return nil, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(leader.Addr, "/")+"/cluster/manifest", nil)
	resp, err := m.node.client.Do(req)
	if err != nil {
		return nil, err
	}
	var remote []FileInfo
	err = json.NewDecoder(resp.Body).Decode(&remote)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	local, _ := Manifest(m.dir)
	lmap := map[string]FileInfo{}
	for _, f := range local {
		lmap[f.Path] = f
	}
	var changed []string
	seen := map[string]bool{}
	for _, rf := range remote {
		if len(m.Only) > 0 {
			keep := false
			for _, pre := range m.Only {
				if strings.HasPrefix(rf.Path, pre) {
					keep = true
				}
			}
			if !keep {
				continue
			}
		}
		seen[rf.Path] = true
		lf, have := lmap[rf.Path]
		prev := m.last[rf.Path]
		if have && lf.Size == rf.Size && prev.MTime == rf.MTime && prev.Size == rf.Size {
			continue
		}
		// append-only journals grow: fetch the tail when our copy is a prefix
		off := int64(0)
		if have && strings.HasSuffix(rf.Path, ".jsonl") && lf.Size < rf.Size && prev.Size == lf.Size {
			off = lf.Size
		}
		if err := m.fetch(ctx, leader.Addr, rf, off); err != nil {
			return changed, fmt.Errorf("%s: %w", rf.Path, err)
		}
		m.mu.Lock()
		m.last[rf.Path] = rf
		m.mu.Unlock()
		changed = append(changed, rf.Path)
	}
	for _, lf := range local {
		if len(m.Only) > 0 {
			keep := false
			for _, pre := range m.Only {
				if strings.HasPrefix(lf.Path, pre) {
					keep = true
				}
			}
			if !keep {
				continue
			}
		}
		if !seen[lf.Path] {
			os.Remove(filepath.Join(m.dir, filepath.FromSlash(lf.Path)))
			m.mu.Lock()
			delete(m.last, lf.Path)
			m.mu.Unlock()
			changed = append(changed, lf.Path)
		}
	}
	m.node.mu.Lock()
	m.node.mirrorAt = time.Now()
	m.node.mu.Unlock()
	m.mu.Lock()
	m.Err = ""
	m.mu.Unlock()
	if len(changed) > 0 && m.OnChange != nil {
		m.OnChange(changed)
	}
	return changed, nil
}

func (m *Mirror) fetch(ctx context.Context, addr string, rf FileInfo, off int64) error {
	u := strings.TrimRight(addr, "/") + "/cluster/file?path=" + url.QueryEscape(rf.Path) + "&off=" + fmt.Sprint(off)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := m.node.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	dst := filepath.Join(m.dir, filepath.FromSlash(rf.Path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	var n int64
	if off > 0 {
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		n, err = io.Copy(f, resp.Body)
		f.Close()
		if err != nil {
			return err
		}
	} else {
		tmp := dst + ".mirror.tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		n, err = io.Copy(f, resp.Body)
		f.Close()
		if err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
	}
	os.Chtimes(dst, time.Now(), time.Unix(0, rf.MTime))
	m.mu.Lock()
	m.Bytes += n
	m.Files++
	m.mu.Unlock()
	return nil
}

// Stats for the UI.
func (m *Mirror) Stats() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{"files": m.Files, "bytes": m.Bytes, "last_error": m.Err}
}

// Bootstrap does a full initial sync with retries (used right after join).
func (m *Mirror) Bootstrap(ctx context.Context) error {
	var err error
	for i := 0; i < 5; i++ {
		if _, err = m.SyncOnce(ctx); err == nil {
			return nil
		}
		log.Printf("cluster: initial sync: %v (retrying)", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return err
}
