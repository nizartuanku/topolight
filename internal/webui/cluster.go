package webui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nizartuanku/topolight/internal/auth"
	"github.com/nizartuanku/topolight/internal/cluster"
)

// ClusterCtl is what the console needs to show and manage the cluster.
type ClusterCtl struct {
	Ident  *cluster.Identity
	Node   func() *cluster.Node
	Enable func() error
}

func (s *Server) clusterStatus(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	c := s.d.Cluster
	if c == nil || c.Ident == nil {
		writeJSON(w, 200, map[string]any{"available": false, "reason": "no data directory (memory mode)"})
		return
	}
	out := map[string]any{"available": true, "enabled": c.Ident.Enabled, "node_id": c.Ident.ID, "name": c.Ident.Name, "addr": c.Ident.Addr, "console": c.Ident.Console, "role": c.Ident.Role, "ca_fp": c.Ident.CAFingerprint(), "site_pins": c.Ident.SitePins}
	if n := c.Node(); n != nil {
		st := n.Status()
		out["status"] = st
		// devices per node for the table
		out["members"] = c.Ident.MemberList()
	}
	sites := map[string]string{}
	for _, x := range s.d.Store.Sites() {
		sites[x.ID] = x.Name
	}
	out["sites"] = sites
	writeJSON(w, 200, out)
}

func (s *Server) clusterEnable(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	c := s.d.Cluster
	if c == nil || c.Enable == nil {
		fail(w, 400, "clustering is not available in memory mode")
		return
	}
	if err := c.Enable(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "addr": c.Ident.Addr})
}

func (s *Server) clusterToken(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	c := s.d.Cluster
	if c == nil || !c.Ident.Enabled {
		fail(w, 400, "enable clustering first")
		return
	}
	var in struct{ Role string }
	_ = readJSON(r, &in)
	tok, err := c.Ident.NewToken(in.Role, 24*time.Hour)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	cmd := "curl -fsSL https://raw.githubusercontent.com/nizartuanku/topolight/main/install.sh | sudo sh -s -- --join " + c.Ident.Addr + " --token " + tok
	if in.Role == cluster.RoleCollector {
		cmd += " --role collector"
	}
	writeJSON(w, 200, map[string]any{"token": tok, "expires": time.Now().Add(24 * time.Hour), "command": cmd, "manual": "topolight -join " + c.Ident.Addr + " -join-token " + tok + " -data /var/lib/topolight"})
}

func (s *Server) clusterMembers(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	c := s.d.Cluster
	if c == nil || !c.Ident.Enabled {
		fail(w, 400, "enable clustering first")
		return
	}
	var in struct {
		Remove string            `json:"remove"`
		Pins   map[string]string `json:"pins"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	n := c.Node()
	if in.Remove != "" {
		if in.Remove == c.Ident.ID {
			fail(w, 400, "a node cannot remove itself")
			return
		}
		if n != nil {
			n.RemoveMember(in.Remove)
		}
	}
	if in.Pins != nil {
		clean := map[string]string{}
		for site, nodeID := range in.Pins {
			if strings.TrimSpace(nodeID) != "" {
				clean[site] = nodeID
			}
		}
		c.Ident.SitePins = clean
		_ = c.Ident.Save()
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// clusterPeer proxies a status request to a member (for the per-node detail).
func (s *Server) clusterPeer(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	c := s.d.Cluster
	n := c.Node()
	if c == nil || n == nil {
		fail(w, 400, "cluster not running")
		return
	}
	id := r.PathValue("id")
	for _, m := range c.Ident.MemberList() {
		if m.ID != id {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(m.Addr, "/")+"/cluster/status", nil)
		resp, err := n.Client().Do(req)
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 64<<10)
		for {
			k, err := resp.Body.Read(buf)
			if k > 0 {
				w.Write(buf[:k])
			}
			if err != nil {
				return
			}
		}
	}
	fail(w, 404, "member not found")
}
