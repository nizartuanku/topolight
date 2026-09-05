// Package webui serves the console: REST API, live stream (SSE) and the
// embedded single-page UI. No external asset is ever requested.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/auth"
	"github.com/nizartuanku/topolight/internal/backup"
	"github.com/nizartuanku/topolight/internal/discovery"
	"github.com/nizartuanku/topolight/internal/endpoint"
	"github.com/nizartuanku/topolight/internal/flow"
	"github.com/nizartuanku/topolight/internal/gnmi"
	"github.com/nizartuanku/topolight/internal/integ"
	"github.com/nizartuanku/topolight/internal/license"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/notify"
	"github.com/nizartuanku/topolight/internal/poller"
	"github.com/nizartuanku/topolight/internal/probe"
	"github.com/nizartuanku/topolight/internal/profile"
	"github.com/nizartuanku/topolight/internal/report"
	"github.com/nizartuanku/topolight/internal/state"
	"github.com/nizartuanku/topolight/internal/store"
	"github.com/nizartuanku/topolight/internal/syslog"
	"github.com/nizartuanku/topolight/internal/topology"
	"github.com/nizartuanku/topolight/internal/trap"
	"github.com/nizartuanku/topolight/internal/tsdb"
	"github.com/nizartuanku/topolight/internal/version"
)

//go:embed ui/*
var uiFS embed.FS

// Deps wires the server to the rest of the binary.
type Deps struct {
	Store                *store.Store
	DB                   *tsdb.DB
	Logs                 *syslog.LogStore
	Poller               *poller.Poller
	Discovery            *discovery.Discovery
	Topology             *topology.Builder
	Engine               *state.Engine
	Notify               *notify.Dispatcher
	Profiles             *profile.Library
	Syslog               *syslog.Receiver
	Trap                 *trap.Receiver
	Flow                 *flow.Collector
	Endpoints            *endpoint.Store
	Probes               *probe.Runner
	Backup               *backup.Runner
	Reports              *report.Runner
	Integ                *integ.Runner
	Cluster              *ClusterCtl
	FlowAddr, SFlowAddr  string
	SyslogTLSAddr        string
	License              func() license.State
	SetLicense           func(key string) license.State
	DataDir              string
	Started              time.Time
	Listen               string
	SyslogAddr, TrapAddr string
	ICMPError            string
}

// Server is the HTTP handler set.
type Server struct {
	d         Deps
	sessions  *auth.Sessions
	mux       *http.ServeMux
	mu        sync.Mutex
	loginFail map[string][]time.Time
}

// New builds the server.
func New(d Deps) *Server {
	s := &Server{d: d, sessions: auth.NewSessions(7 * 24 * time.Hour), mux: http.NewServeMux(), loginFail: map[string][]time.Time{}}
	s.routes()
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.secure(s.mux) }

func (s *Server) secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; font-src 'self' data:; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

// ---- helpers ----

type apiError struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, apiError{Error: msg, Code: code})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

func (s *Server) session(r *http.Request) (auth.Session, bool) {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return s.tokenSession(strings.TrimSpace(strings.TrimPrefix(a, "Bearer ")))
	}
	c, err := r.Cookie("topolight_session")
	if err != nil {
		return auth.Session{}, false
	}
	return s.sessions.Get(c.Value)
}

// tokenSession turns a bearer API token into a session-shaped identity.
// Tokens are not subject to the same-origin check (no browser is involved).
func (s *Server) tokenSession(secret string) (auth.Session, bool) {
	if !strings.HasPrefix(secret, "tl_") || len(secret) < 20 {
		return auth.Session{}, false
	}
	sum := sha256.Sum256([]byte(secret))
	t, ok := s.d.Store.TokenByHash(hex.EncodeToString(sum[:]))
	if !ok {
		return auth.Session{}, false
	}
	s.d.Store.TouchToken(t.ID, time.Now())
	return auth.Session{Token: "api:" + t.ID, UserID: "token:" + t.ID, Name: "token " + t.Name, Role: t.Role, Expires: time.Now().Add(time.Minute), Created: t.Created}, true
}

// require wraps a handler with authentication and a minimum role.
func (s *Server) require(role string, h func(http.ResponseWriter, *http.Request, auth.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.session(r)
		if !ok {
			if len(s.d.Store.Users()) == 0 {
				fail(w, http.StatusUnauthorized, "setup required")
				return
			}
			fail(w, http.StatusUnauthorized, "sign in required")
			return
		}
		if !roleAllows(sess.Role, role) {
			fail(w, http.StatusForbidden, "your role ("+sess.Role+") cannot do this")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && !strings.HasPrefix(sess.Token, "api:") {
			// same-origin check for state-changing requests
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := parseOrigin(o); err != nil || !strings.EqualFold(u, r.Host) {
					fail(w, http.StatusForbidden, "cross-origin request rejected")
					return
				}
			}
		}
		h(w, r, sess)
	}
}

func parseOrigin(o string) (string, error) {
	o = strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
	if o == "" {
		return "", errors.New("empty")
	}
	return o, nil
}

func roleAllows(have, need string) bool {
	rank := map[string]int{"viewer": 1, "operator": 2, "admin": 3}
	return rank[have] >= rank[need]
}

func (s *Server) caps() license.Caps { return s.d.License().Caps }

// ---- routes ----

func (s *Server) routes() {
	m := s.mux
	sub, _ := fs.Sub(uiFS, "ui")
	static := http.FileServer(http.FS(sub))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-store")
			b, _ := uiFS.ReadFile("ui/index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(b)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fail(w, 404, "no such endpoint")
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		static.ServeHTTP(w, r)
	})

	m.HandleFunc("GET /api/status", s.status)
	m.HandleFunc("POST /api/setup", s.setup)
	m.HandleFunc("POST /api/login", s.login)
	m.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("topolight_session"); err == nil {
			s.sessions.Delete(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: "topolight_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /api/me", s.require("viewer", func(w http.ResponseWriter, r *http.Request, sess auth.Session) {
		writeJSON(w, 200, map[string]string{"user": sess.Name, "role": sess.Role, "id": sess.UserID})
	}))
	m.HandleFunc("GET /api/stream", s.require("viewer", s.stream))

	// sites
	m.HandleFunc("GET /api/sites", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) { writeJSON(w, 200, s.d.Store.Sites()) }))
	m.HandleFunc("POST /api/sites", s.require("admin", s.putSite))
	m.HandleFunc("PUT /api/sites/{id}", s.require("admin", s.putSite))
	m.HandleFunc("DELETE /api/sites/{id}", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		id := r.PathValue("id")
		for _, d := range s.d.Store.Devices() {
			if d.SiteID == id {
				fail(w, 409, "site still has devices; delete or move them first")
				return
			}
		}
		s.d.Store.DeleteSite(id)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("POST /api/sites/{id}/discover", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		id := r.PathValue("id")
		if _, err := s.d.Store.Site(id); err != nil {
			fail(w, 404, "site not found")
			return
		}
		if len(s.d.Store.Creds()) == 0 {
			fail(w, 400, "add an SNMP credential first")
			return
		}
		go func() {
			p, _ := s.d.Discovery.Sweep(context.Background(), id)
			s.d.Engine.Broadcast(state.Change{Type: "discovery", Data: p})
			s.d.Topology.Collect(context.Background())
			s.d.Engine.InvalidateTopology()
			s.d.Engine.Broadcast(state.Change{Type: "topology", Data: map[string]any{"site": id}})
		}()
		writeJSON(w, 202, map[string]string{"status": "started"})
	}))
	m.HandleFunc("GET /api/sites/{id}/discovery", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		p := s.d.Discovery.Progress(r.PathValue("id"))
		if p == nil {
			writeJSON(w, 200, map[string]any{"running": false})
			return
		}
		writeJSON(w, 200, p)
	}))

	// credentials
	m.HandleFunc("GET /api/creds", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		var out []model.Credential
		for _, c := range s.d.Store.Creds() {
			out = append(out, c.Redacted())
		}
		if out == nil {
			out = []model.Credential{}
		}
		writeJSON(w, 200, out)
	}))
	m.HandleFunc("POST /api/creds", s.require("admin", s.putCred))
	m.HandleFunc("PUT /api/creds/{id}", s.require("admin", s.putCred))
	m.HandleFunc("DELETE /api/creds/{id}", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		s.d.Store.DeleteCred(r.PathValue("id"))
		if s.d.Trap != nil {
			s.d.Trap.V3.Forget()
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("POST /api/creds/{id}/test", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		var in struct {
			IP string `json:"ip"`
		}
		if err := readJSON(r, &in); err != nil || net.ParseIP(in.IP) == nil {
			fail(w, 400, "give an IPv4 address to test against")
			return
		}
		cred, err := s.d.Store.Cred(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "credential not found")
			return
		}
		start := time.Now()
		if cred.IsGNMI() {
			g := poller.GNMIClient(in.IP, cred)
			g.Timeout = 5 * time.Second
			defer g.Close()
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			caps, err := g.Capabilities(ctx)
			if err != nil {
				writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "ms": time.Since(start).Milliseconds()})
				return
			}
			host, vendor := "", ""
			if ups, err := g.Get(ctx, []string{"/system/state"}, 2, gnmi.EncJSONIETF); err == nil {
				t := gnmi.Tree(ups)
				host = gnmi.Str(gnmi.Lookup(t, "/system/state/hostname"))
			}
			models := make([]string, 0, len(caps.Models))
			for _, m := range caps.Models {
				models = append(models, m.Name)
				if vendor == "" && m.Organization != "" && !strings.Contains(strings.ToLower(m.Organization), "openconfig") && !strings.Contains(strings.ToLower(m.Organization), "ietf") {
					vendor = m.Organization
				}
			}
			writeJSON(w, 200, map[string]any{"ok": true, "sys_name": host, "sys_descr": fmt.Sprintf("gNMI %s · %d models (%s%s)", caps.Version, len(models), strings.Join(models[:min(4, len(models))], ", "), map[bool]string{true: ", …", false: ""}[len(models) > 4]), "profile": "gnmi", "vendor": vendor, "ms": time.Since(start).Milliseconds()})
			return
		}
		if cred.IsSSH() {
			writeJSON(w, 200, map[string]any{"ok": false, "error": "SSH credentials are exercised by the configuration backup, not by this test", "ms": 0})
			return
		}
		c := poller.NewClient(in.IP, cred)
		c.Timeout = 2 * time.Second
		defer c.Close()
		vbs, err := c.Get(profile.OIDSysName, profile.OIDSysDescr, profile.OIDSysObjectID)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "ms": time.Since(start).Milliseconds()})
			return
		}
		prof := s.d.Profiles.Match(vbs[2].Value.OID, vbs[1].Value.String())
		writeJSON(w, 200, map[string]any{"ok": true, "sys_name": vbs[0].Value.String(), "sys_descr": firstLine(vbs[1].Value.String()), "profile": prof.ID, "vendor": prof.Vendor, "ms": time.Since(start).Milliseconds()})
	}))

	// devices
	m.HandleFunc("GET /api/devices", s.require("viewer", s.listDevices))
	m.HandleFunc("POST /api/devices", s.require("operator", s.addDevice))
	m.HandleFunc("GET /api/devices/{id}", s.require("viewer", s.getDevice))
	m.HandleFunc("GET /api/devices/{id}/health", s.require("viewer", s.getDeviceHealth))
	m.HandleFunc("GET /api/flow", s.require("viewer", s.flowWindow))
	m.HandleFunc("GET /api/flow/exporters", s.require("viewer", s.flowExporters))
	m.HandleFunc("GET /api/flow/series", s.require("viewer", s.flowSeries))
	m.HandleFunc("GET /api/probes", s.require("viewer", s.listProbes))
	m.HandleFunc("POST /api/probes", s.require("operator", s.putProbe))
	m.HandleFunc("GET /api/probes/{id}", s.require("viewer", s.getProbe))
	m.HandleFunc("PUT /api/probes/{id}", s.require("operator", s.putProbe))
	m.HandleFunc("DELETE /api/probes/{id}", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		id := r.PathValue("id")
		s.d.Store.DeleteProbe(id)
		if s.d.Probes != nil {
			s.d.Probes.Forget(id)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	m.HandleFunc("POST /api/probes/{id}/run", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		p, err := s.d.Store.Probe(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "probe not found")
			return
		}
		writeJSON(w, 200, s.d.Probes.RunOnce(r.Context(), p))
	}))
	m.HandleFunc("GET /api/devices/{id}/routing", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		id := r.PathValue("id")
		if _, err := s.d.Store.Device(id); err != nil {
			fail(w, 404, "device not found")
			return
		}
		rt, ok := s.d.Store.Routing(id)
		writeJSON(w, 200, map[string]any{"routing": rt, "has": ok})
	}))
	m.HandleFunc("GET /api/routing", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		// estate-wide protocol summary: peers down, roots, VLAN count
		all := s.d.Store.AllRouting()
		names := map[string]string{}
		for _, d := range s.d.Store.Devices() {
			names[d.ID] = d.Name
		}
		writeJSON(w, 200, map[string]any{"routing": all, "names": names})
	}))
	m.HandleFunc("GET /api/devices/{id}/configs", s.require("viewer", s.deviceConfigs))
	m.HandleFunc("GET /api/devices/{id}/configs/{ver}", s.require("viewer", s.deviceConfig))
	m.HandleFunc("GET /api/devices/{id}/configs/{ver}/diff/{other}", s.require("viewer", s.deviceConfigDiff))
	m.HandleFunc("POST /api/devices/{id}/backup", s.require("operator", s.backupNow))
	m.HandleFunc("GET /api/configs", s.require("viewer", s.configOverview))
	m.HandleFunc("GET /api/reports", s.require("viewer", s.listReports))
	m.HandleFunc("POST /api/reports", s.require("operator", s.putReport))
	m.HandleFunc("PUT /api/reports/{id}", s.require("operator", s.putReport))
	m.HandleFunc("DELETE /api/reports/{id}", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		s.d.Store.DeleteReport(r.PathValue("id"))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	m.HandleFunc("POST /api/reports/{id}/run", s.require("operator", s.runReport))
	m.HandleFunc("GET /api/reports/preview", s.require("viewer", s.previewReport))
	m.HandleFunc("GET /api/reports/files/{file}", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		b, err := s.d.Reports.Read(r.PathValue("file"))
		if err != nil {
			fail(w, 404, "report not found")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	}))
	m.HandleFunc("GET /api/integrations", s.require("admin", s.listIntegrations))
	m.HandleFunc("POST /api/integrations", s.require("admin", s.putIntegration))
	m.HandleFunc("PUT /api/integrations/{id}", s.require("admin", s.putIntegration))
	m.HandleFunc("DELETE /api/integrations/{id}", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		s.d.Store.DeleteIntegration(r.PathValue("id"))
		if s.d.Integ != nil {
			s.d.Integ.Forget(r.PathValue("id"))
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	m.HandleFunc("POST /api/integrations/{id}/test", s.require("admin", s.testIntegration))
	m.HandleFunc("POST /api/integrations/{id}/run", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		if s.d.Integ != nil {
			s.d.Integ.Now(r.PathValue("id"))
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	m.HandleFunc("GET /api/wireless", s.require("viewer", s.wirelessOverview))
	m.HandleFunc("GET /api/sdwan", s.require("viewer", s.sdwanOverview))
	m.HandleFunc("GET /api/devices/{id}/wireless", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		ws, ok := s.d.Store.Wireless(r.PathValue("id"))
		writeJSON(w, 200, map[string]any{"wireless": ws, "has": ok, "sdwan": s.d.Store.SDWAN(r.PathValue("id"))})
	}))
	m.HandleFunc("GET /api/cluster", s.require("viewer", s.clusterStatus))
	m.HandleFunc("POST /api/cluster/enable", s.require("admin", s.clusterEnable))
	m.HandleFunc("POST /api/cluster/token", s.require("admin", s.clusterToken))
	m.HandleFunc("POST /api/cluster/members", s.require("admin", s.clusterMembers))
	m.HandleFunc("GET /api/cluster/peer/{id}", s.require("admin", s.clusterPeer))
	m.HandleFunc("GET /api/endpoints", s.require("viewer", s.listEndpoints))
	m.HandleFunc("GET /api/endpoints/{mac}", s.require("viewer", s.getEndpoint))
	m.HandleFunc("GET /api/devices/{id}/endpoints", s.require("viewer", s.deviceEndpoints))
	m.HandleFunc("PUT /api/devices/{id}", s.require("operator", s.updateDevice))
	m.HandleFunc("DELETE /api/devices/{id}", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		id := r.PathValue("id")
		s.d.Store.DeleteDevice(id)
		s.d.Poller.Forget(id)
		if s.d.Backup != nil {
			s.d.Backup.Forget(id)
		}
		if s.d.Probes != nil {
			for _, p := range s.d.Store.Probes() {
				if p.DeviceID == id {
					p.DeviceID = ""
					s.d.Store.PutProbe(p)
				}
			}
		}
		s.d.Engine.InvalidateTopology()
		s.d.Topology.Rebuild()
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("POST /api/devices/{id}/poll", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		s.d.Poller.PollNow(r.PathValue("id"))
		writeJSON(w, 202, map[string]string{"status": "queued"})
	}))
	m.HandleFunc("PUT /api/interfaces/{id}", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		var in struct {
			Important *bool `json:"important"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, 400, "bad json")
			return
		}
		id := r.PathValue("id")
		ok := s.d.Store.UpdateInterface(id, func(x *model.Interface) {
			if in.Important != nil {
				x.Important = *in.Important
			}
		})
		if !ok {
			fail(w, 404, "interface not found")
			return
		}
		i, _ := s.d.Store.Interface(id)
		writeJSON(w, 200, i)
	}))
	m.HandleFunc("GET /api/snippets", s.require("viewer", s.snippets))

	// topology
	m.HandleFunc("GET /api/topology", s.require("viewer", s.topology))
	m.HandleFunc("POST /api/topology/rebuild", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		go func() {
			s.d.Topology.Collect(context.Background())
			s.d.Engine.InvalidateTopology()
			s.d.Engine.Broadcast(state.Change{Type: "topology", Data: map[string]any{}})
		}()
		writeJSON(w, 202, map[string]string{"status": "started"})
	}))
	m.HandleFunc("POST /api/links", s.require("operator", s.addLink))
	m.HandleFunc("DELETE /api/links/{id}", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		s.d.Store.DeleteLink(r.PathValue("id"))
		s.d.Engine.InvalidateTopology()
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// metrics
	m.HandleFunc("GET /api/metrics", s.require("viewer", s.metrics))

	// alerts & events
	m.HandleFunc("GET /api/alerts", s.require("viewer", s.listAlerts))
	m.HandleFunc("POST /api/alerts/{id}/ack", s.require("operator", func(w http.ResponseWriter, r *http.Request, sess auth.Session) {
		var in struct {
			Note string `json:"note"`
		}
		_ = readJSON(r, &in)
		if !s.d.Engine.Ack(r.PathValue("id"), sess.Name, in.Note) {
			fail(w, 404, "alert not found")
			return
		}
		a, _ := s.d.Store.Alert(r.PathValue("id"))
		writeJSON(w, 200, a)
	}))
	m.HandleFunc("POST /api/alerts/{id}/resolve", s.require("operator", func(w http.ResponseWriter, r *http.Request, sess auth.Session) {
		if !s.d.Engine.Resolve(r.PathValue("id"), sess.Name) {
			fail(w, 404, "alert not found")
			return
		}
		a, _ := s.d.Store.Alert(r.PathValue("id"))
		writeJSON(w, 200, a)
	}))
	m.HandleFunc("GET /api/events", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 2000 {
			limit = 200
		}
		writeJSON(w, 200, s.d.Store.RecentEvents(r.URL.Query().Get("device"), limit))
	}))

	// logs
	m.HandleFunc("GET /api/logs", s.require("viewer", s.logs))
	m.HandleFunc("GET /api/logs/histogram", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		q := s.logQuery(r)
		writeJSON(w, 200, map[string]any{"from": q.From, "to": q.To, "buckets": s.d.Logs.Histogram(q, 48)})
	}))

	// maintenance
	m.HandleFunc("GET /api/maintenance", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		out := s.d.Store.Maintenances()
		if out == nil {
			out = []model.Maintenance{}
		}
		writeJSON(w, 200, out)
	}))
	m.HandleFunc("POST /api/maintenance", s.require("operator", func(w http.ResponseWriter, r *http.Request, sess auth.Session) {
		if !s.caps().Maintenance {
			fail(w, 402, "maintenance windows are a Pro/Team feature")
			return
		}
		var mw model.Maintenance
		if err := readJSON(r, &mw); err != nil || mw.To.Before(mw.From) {
			fail(w, 400, "bad window")
			return
		}
		mw.ID = model.NewID("mnt")
		mw.Creator = sess.Name
		mw.Created = time.Now()
		s.d.Store.PutMaintenance(mw)
		writeJSON(w, 201, mw)
	}))
	m.HandleFunc("DELETE /api/maintenance/{id}", s.require("operator", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		s.d.Store.DeleteMaintenance(r.PathValue("id"))
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// rules
	m.HandleFunc("GET /api/rules", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) { writeJSON(w, 200, s.d.Store.Rules()) }))
	m.HandleFunc("PUT /api/rules/{id}", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		old, ok := s.d.Store.Rule(r.PathValue("id"))
		if !ok {
			fail(w, 404, "rule not found")
			return
		}
		var in model.Rule
		if err := readJSON(r, &in); err != nil {
			fail(w, 400, "bad json")
			return
		}
		old.Enter, old.Exit, old.ForCycles, old.Severity, old.Escalate, old.OnlyImport, old.Enabled = in.Enter, in.Exit, in.ForCycles, in.Severity, in.Escalate, in.OnlyImport, in.Enabled
		if old.Severity.Rank() == 0 {
			old.Severity = model.SevMinor
		}
		s.d.Store.PutRule(old)
		writeJSON(w, 200, old)
	}))

	// notifications
	m.HandleFunc("GET /api/notify", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		writeJSON(w, 200, map[string]any{"config": s.d.Store.Notify(), "smtp_configured": s.d.Notify.SMTP.Host != "", "telegram_configured": s.d.Notify.Telegram.Token != "", "webhook_signed": s.d.Notify.WebhookSecret != ""})
	}))
	m.HandleFunc("PUT /api/notify", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		var n model.Notify
		if err := readJSON(r, &n); err != nil {
			fail(w, 400, "bad json")
			return
		}
		caps := s.caps()
		if n.TelegramChat != "" && !caps.Telegram {
			fail(w, 402, "Telegram notifications are a Pro/Team feature")
			return
		}
		if n.WebhookURL != "" && !caps.Webhook {
			fail(w, 402, "webhook notifications are a Pro/Team feature")
			return
		}
		if n.MinSeverity == "" {
			n.MinSeverity = model.SevMinor
		}
		s.d.Store.SetNotify(n)
		writeJSON(w, 200, n)
	}))
	m.HandleFunc("POST /api/notify/test", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		writeJSON(w, 200, map[string]any{"results": s.d.Notify.Test()})
	}))

	// users
	m.HandleFunc("GET /api/users", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		var out []map[string]any
		for _, u := range s.d.Store.Users() {
			out = append(out, map[string]any{"id": u.ID, "name": u.Name, "role": u.Role, "created": u.Created, "disabled": u.Disabled})
		}
		if out == nil {
			out = []map[string]any{}
		}
		writeJSON(w, 200, out)
	}))
	m.HandleFunc("POST /api/users", s.require("admin", s.addUser))
	m.HandleFunc("GET /api/tokens", s.require("admin", s.listTokens))
	m.HandleFunc("POST /api/tokens", s.require("admin", s.addToken))
	m.HandleFunc("DELETE /api/tokens/{id}", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		s.d.Store.DeleteToken(r.PathValue("id"))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	m.HandleFunc("DELETE /api/users/{id}", s.require("admin", func(w http.ResponseWriter, r *http.Request, sess auth.Session) {
		id := r.PathValue("id")
		if id == sess.UserID {
			fail(w, 400, "you cannot delete yourself")
			return
		}
		s.d.Store.DeleteUser(id)
		s.sessions.DeleteUser(id)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("PUT /api/users/{id}/password", s.require("viewer", func(w http.ResponseWriter, r *http.Request, sess auth.Session) {
		id := r.PathValue("id")
		if id != sess.UserID && sess.Role != "admin" {
			fail(w, 403, "only admins change other users' passwords")
			return
		}
		var in struct {
			Password string `json:"password"`
		}
		if err := readJSON(r, &in); err != nil || len(in.Password) < 10 {
			fail(w, 400, "password must be at least 10 characters")
			return
		}
		h, salt, err := auth.Hash(in.Password)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		var found bool
		for _, u := range s.d.Store.Users() {
			if u.ID == id {
				u.Hash, u.Salt = h, salt
				s.d.Store.PutUser(u)
				found = true
			}
		}
		if !found {
			fail(w, 404, "user not found")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// settings, licence, profiles, export
	m.HandleFunc("GET /api/settings", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) { writeJSON(w, 200, s.d.Store.Settings()) }))
	m.HandleFunc("PUT /api/settings", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		var in model.Settings
		if err := readJSON(r, &in); err != nil {
			fail(w, 400, "bad json")
			return
		}
		cur := s.d.Store.Settings()
		if in.InstanceName != "" {
			cur.InstanceName = in.InstanceName
		}
		cur.ConsoleURL = in.ConsoleURL
		if in.DefaultPoll >= 15 && in.DefaultPoll <= 3600 {
			cur.DefaultPoll = in.DefaultPoll
		}
		cur.DiscoveryEvery = in.DiscoveryEvery
		if in.BackupEveryHours >= -1 && in.BackupEveryHours <= 24*30 {
			cur.BackupEveryHours = in.BackupEveryHours
		}
		if in.TopologyEvery >= 5 {
			cur.TopologyEvery = in.TopologyEvery
		}
		cur.Timezone = in.Timezone
		s.d.Store.SetSettings(cur)
		s.d.Notify.ConsoleURL = cur.ConsoleURL
		writeJSON(w, 200, cur)
	}))
	m.HandleFunc("GET /api/license", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) { writeJSON(w, 200, s.d.License()) }))
	m.HandleFunc("PUT /api/license", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		var in struct {
			Key string `json:"key"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, 400, "bad json")
			return
		}
		st := s.d.SetLicense(strings.TrimSpace(in.Key))
		mon, un := discovery.ApplyCap(s.d.Store, st.Caps)
		writeJSON(w, 200, map[string]any{"license": st, "monitored": mon, "unmonitored": un})
	}))
	m.HandleFunc("GET /api/profiles", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) { writeJSON(w, 200, s.d.Profiles.All()) }))
	m.HandleFunc("GET /api/export.json", s.require("viewer", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		if !s.caps().Export {
			fail(w, 402, "export is a Pro/Team feature")
			return
		}
		writeJSON(w, 200, map[string]any{"exported_at": time.Now(), "version": version.Version, "sites": s.d.Store.Sites(), "devices": s.d.Store.Devices(),
			"interfaces": s.d.Store.Interfaces(""), "links": s.d.Store.Links(), "alerts": s.d.Store.Alerts()})
	}))
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

// ---- status / setup / login ----

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	total, monitored := s.d.Store.DeviceCount()
	lic := s.d.License()
	setupDone := len(s.d.Store.Users()) > 0
	out := map[string]any{
		"product": version.Product, "version": version.Version, "setup_done": setupDone,
		"license": map[string]any{"tier": lic.Tier, "notice": lic.Notice, "valid": lic.Valid, "caps": lic.Caps, "instance": lic.Instance},
		"devices": map[string]int{"total": total, "monitored": monitored},
		"started": s.d.Started, "uptime_s": int(time.Since(s.d.Started).Seconds()),
	}
	if sess, ok := s.session(r); ok {
		var counts = map[string]int{}
		for _, a := range s.d.Store.Alerts() {
			if a.State != model.AlertResolved && a.RootCause == "" {
				counts[string(a.Severity)]++
			}
		}
		up, down, degraded, unknown := 0, 0, 0, 0
		for _, d := range s.d.Store.Devices() {
			if !d.Monitored {
				continue
			}
			switch d.Status {
			case model.StatusUp:
				up++
			case model.StatusDown, model.StatusUnreachable, model.StatusFlapping:
				down++
			case model.StatusDegraded:
				degraded++
			default:
				unknown++
			}
		}
		out["user"] = map[string]string{"name": sess.Name, "role": sess.Role, "id": sess.UserID}
		out["alerts"] = counts
		out["health"] = map[string]int{"up": up, "down": down, "degraded": degraded, "unknown": unknown}
		out["collectors"] = map[string]any{
			"poll_cycles": s.d.Poller.Cycles, "poll_failures": s.d.Poller.Failures,
			"syslog_received": s.d.Syslog.Received, "syslog_dropped": s.d.Syslog.Dropped, "syslog_unknown_hosts": s.d.Syslog.UnknownHosts(),
			"trap_received": s.d.Trap.Received, "trap_rejected": s.d.Trap.Rejected, "trap_v3_received": s.d.Trap.V3Received, "trap_v3_rejected": s.d.Trap.V3Rejected, "trap_v3_last_error": s.d.Trap.V3LastErr, "trap_engine_id": s.d.Store.Settings().EngineID,
			"series": s.d.DB.SeriesCount(), "tsdb_bytes": s.d.DB.DiskUsage(), "logs_count": s.d.Logs.Count,
			"syslog_addr": s.d.SyslogAddr, "syslog_tls_addr": s.d.SyslogTLSAddr, "syslog_tls_failed": s.d.Syslog.TLSFailed, "syslog_tls_last_error": s.d.Syslog.TLSLastErr, "trap_addr": s.d.TrapAddr, "icmp": s.d.ICMPError == "", "icmp_error": s.d.ICMPError,
			"notify_sent": s.d.Notify.Sent, "notify_failed": s.d.Notify.Failed,
			"flow_addr": s.d.FlowAddr, "sflow_addr": s.d.SFlowAddr,
		}
		if s.d.Flow != nil {
			out["flow"] = s.d.Flow.Stats()
		}
		if s.d.Backup != nil {
			out["backup"] = s.d.Backup.Stats()
		}
		if s.d.Endpoints != nil {
			out["endpoints"] = s.d.Endpoints.Stats(time.Now())
			out["endpoint_walks"] = s.d.Poller.EPWalks
		}
		out["settings"] = s.d.Store.Settings()
		v, at := s.d.Store.TopologyVersion()
		out["topology"] = map[string]any{"version": v, "at": at}
	}
	writeJSON(w, 200, out)
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if len(s.d.Store.Users()) > 0 {
		fail(w, 409, "setup already completed")
		return
	}
	var in struct {
		User, Password, InstanceName string
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	in.User = strings.TrimSpace(in.User)
	if len(in.User) < 2 || len(in.Password) < 10 {
		fail(w, 400, "user name (2+ chars) and password (10+ chars) required")
		return
	}
	h, salt, err := auth.Hash(in.Password)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	u := model.User{ID: model.NewID("usr"), Name: in.User, Role: "admin", Hash: h, Salt: salt, Created: time.Now()}
	s.d.Store.PutUser(u)
	cfg := s.d.Store.Settings()
	if in.InstanceName != "" {
		cfg.InstanceName = in.InstanceName
	}
	cfg.SetupDone = true
	s.d.Store.SetSettings(cfg)
	_ = s.d.Store.Save()
	sess, _ := s.sessions.Create(u.ID, u.Name, u.Role)
	s.setCookie(w, r, sess.Token)
	writeJSON(w, 201, map[string]any{"ok": true, "user": u.Name})
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{Name: "topolight_session", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 7 * 24 * 3600})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ User, Password string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	fails := s.loginFail[ip]
	cut := time.Now().Add(-10 * time.Minute)
	var keep []time.Time
	for _, t := range fails {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	s.loginFail[ip] = keep
	tooMany := len(keep) >= 10
	s.mu.Unlock()
	if tooMany {
		fail(w, 429, "too many failed sign-ins; wait 10 minutes")
		return
	}
	u, ok := s.d.Store.UserByName(in.User)
	if !ok || u.Disabled || !auth.Verify(in.Password, u.Hash, u.Salt) {
		s.mu.Lock()
		s.loginFail[ip] = append(s.loginFail[ip], time.Now())
		s.mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		fail(w, 401, "wrong user name or password")
		return
	}
	sess, err := s.sessions.Create(u.ID, u.Name, u.Role)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.setCookie(w, r, sess.Token)
	writeJSON(w, 200, map[string]string{"user": u.Name, "role": u.Role, "id": u.ID})
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var in struct{ Name, Password, Role string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	caps := s.caps()
	if !license.Unlimited(caps.MaxUsers) && len(s.d.Store.Users()) >= caps.MaxUsers {
		fail(w, 402, fmt.Sprintf("the %s tier allows %d user(s)", caps.Tier.Title(), caps.MaxUsers))
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) < 2 || len(in.Password) < 10 {
		fail(w, 400, "name (2+ chars) and password (10+ chars) required")
		return
	}
	if _, exists := s.d.Store.UserByName(in.Name); exists {
		fail(w, 409, "user already exists")
		return
	}
	if in.Role == "" {
		in.Role = "admin"
	}
	if in.Role != "admin" && !caps.Roles {
		fail(w, 402, "operator/viewer roles are a Team feature; all users are admins on this tier")
		return
	}
	if !roleAllows(in.Role, "viewer") {
		fail(w, 400, "role must be admin, operator or viewer")
		return
	}
	h, salt, err := auth.Hash(in.Password)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	u := model.User{ID: model.NewID("usr"), Name: in.Name, Role: in.Role, Hash: h, Salt: salt, Created: time.Now()}
	s.d.Store.PutUser(u)
	writeJSON(w, 201, map[string]any{"id": u.ID, "name": u.Name, "role": u.Role})
}

// ---- sites & creds ----

func (s *Server) putSite(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var in model.Site
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		fail(w, 400, "site needs a name")
		return
	}
	var subnets []string
	for _, sn := range in.Subnets {
		sn = strings.TrimSpace(sn)
		if sn != "" {
			subnets = append(subnets, sn)
		}
	}
	in.Subnets = subnets
	id := r.PathValue("id")
	if id == "" {
		caps := s.caps()
		if !license.Unlimited(caps.MaxSites) && len(s.d.Store.Sites()) >= caps.MaxSites {
			fail(w, 402, fmt.Sprintf("the %s tier allows %d site(s) — upgrade for more", caps.Tier.Title(), caps.MaxSites))
			return
		}
		in.ID = model.NewID("site")
		in.Created = time.Now()
		s.d.Store.PutSite(in)
		writeJSON(w, 201, in)
		return
	}
	old, err := s.d.Store.Site(id)
	if err != nil {
		fail(w, 404, "site not found")
		return
	}
	old.Name, old.Subnets, old.Lat, old.Lon, old.CredID, old.Disabled, old.AddPingOnly, old.SSHCredID = in.Name, in.Subnets, in.Lat, in.Lon, in.CredID, in.Disabled, in.AddPingOnly, in.SSHCredID
	s.d.Store.PutSite(old)
	writeJSON(w, 200, old)
}

func (s *Server) putCred(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var in model.Credential
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		fail(w, 400, "credential needs a name")
		return
	}
	if in.Kind == "ssh" || in.Kind == "gnmi" {
		in.Version = ""
	} else if in.Version != "3" {
		in.Version = "2c"
	}
	id := r.PathValue("id")
	if id != "" {
		old, err := s.d.Store.Cred(id)
		if err != nil {
			fail(w, 404, "credential not found")
			return
		}
		// keep secrets when the UI sends the redaction placeholder
		if in.Community == "••••" {
			in.Community = old.Community
		}
		if in.AuthPass == "••••" {
			in.AuthPass = old.AuthPass
		}
		if in.PrivPass == "••••" {
			in.PrivPass = old.PrivPass
		}
		if in.Password == "••••" {
			in.Password = old.Password
		}
		if in.PrivateKey == "••••" {
			in.PrivateKey = old.PrivateKey
		}
		if in.EnablePass == "••••" {
			in.EnablePass = old.EnablePass
		}
		in.ID = id
	} else {
		in.ID = model.NewID("cred")
	}
	if in.Kind == "ssh" {
		if strings.TrimSpace(in.User) == "" {
			fail(w, 400, "SSH user name required")
			return
		}
		if in.Password == "" && in.PrivateKey == "" {
			fail(w, 400, "SSH needs a password or a private key")
			return
		}
		if in.Port < 0 || in.Port > 65535 {
			in.Port = 0
		}
		in.Community, in.AuthProto, in.AuthPass, in.PrivProto, in.PrivPass = "", "", "", "", ""
		s.d.Store.PutCred(in)
		writeJSON(w, 200, in.Redacted())
		return
	}
	if in.Kind == "gnmi" {
		if strings.TrimSpace(in.User) == "" || in.Password == "" {
			fail(w, 400, "gNMI needs a user name and password")
			return
		}
		if in.Port < 0 || in.Port > 65535 {
			in.Port = 0
		}
		in.Community, in.AuthProto, in.AuthPass, in.PrivProto, in.PrivPass, in.PrivateKey, in.EnablePass = "", "", "", "", "", "", ""
		s.d.Store.PutCred(in)
		writeJSON(w, 200, in.Redacted())
		return
	}
	// SNMP credentials may name a UDP port; 0 keeps the well-known 161. The
	// SSH-only fields are cleared so a credential switched from SSH to SNMP
	// does not carry a stale key or enable password.
	if in.Port < 0 || in.Port > 65535 {
		fail(w, 400, "SNMP port must be between 1 and 65535")
		return
	}
	in.PrivateKey, in.EnablePass, in.Password = "", "", ""
	if in.Version == "2c" && in.Community == "" {
		fail(w, 400, "community string required for v2c")
		return
	}
	if in.Version == "3" {
		if in.User == "" {
			fail(w, 400, "user name required for v3")
			return
		}
		in.AuthProto = strings.ToLower(in.AuthProto)
		in.PrivProto = strings.ToLower(in.PrivProto)
		if in.AuthProto != "" && in.AuthProto != "sha" && in.AuthProto != "sha256" && in.AuthProto != "md5" {
			fail(w, 400, "auth protocol must be sha, sha256 or md5")
			return
		}
		if in.PrivProto != "" && in.PrivProto != "aes" && in.PrivProto != "des" {
			fail(w, 400, "privacy protocol must be aes or des")
			return
		}
		if in.PrivProto != "" && in.AuthProto == "" {
			fail(w, 400, "privacy requires authentication")
			return
		}
	}
	s.d.Store.PutCred(in)
	if s.d.Trap != nil {
		s.d.Trap.V3.Forget()
	}
	code := 200
	if id == "" {
		code = 201
	}
	writeJSON(w, code, in.Redacted())
}

// ---- devices ----

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	q := r.URL.Query()
	site, status, text, domain := q.Get("site"), q.Get("status"), strings.ToLower(q.Get("q")), q.Get("domain")
	var out []model.Device
	for _, d := range s.d.Store.Devices() {
		if site != "" && d.SiteID != site {
			continue
		}
		if status != "" && string(d.Status) != status {
			continue
		}
		if domain != "" && string(d.Domain) != domain {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(d.Name+" "+d.IP+" "+d.Model+" "+d.Vendor+" "+d.Location), text) {
			continue
		}
		out = append(out, d)
	}
	if out == nil {
		out = []model.Device{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) addDevice(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var in struct {
		IP, SiteID, CredID, Name string
		PingOnly                 bool `json:"ping_only"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	ip := net.ParseIP(strings.TrimSpace(in.IP))
	if ip == nil || ip.To4() == nil {
		fail(w, 400, "an IPv4 address is required")
		return
	}
	if _, err := s.d.Store.Site(in.SiteID); err != nil {
		fail(w, 400, "choose a site")
		return
	}
	if _, exists := s.d.Store.DeviceByIP(ip.String()); exists {
		fail(w, 409, "a device with this IP already exists")
		return
	}
	if in.PingOnly {
		dev, added := s.d.Discovery.RegisterPingOnly(in.SiteID, ip.String(), strings.TrimSpace(in.Name), "user", nil)
		if !added {
			fail(w, 409, "device already exists")
			return
		}
		s.d.Poller.PollNow(dev.ID)
		s.d.Engine.Broadcast(state.Change{Type: "device", Data: dev})
		writeJSON(w, 201, map[string]any{"device": dev})
		return
	}
	// identify now so the user sees what was added
	name, descr, oid := strings.TrimSpace(in.Name), "", ""
	creds := s.d.Store.Creds()
	if in.CredID != "" {
		if c, err := s.d.Store.Cred(in.CredID); err == nil {
			creds = append([]model.Credential{c}, creds...)
		}
	}
	credUsed := in.CredID
	for _, c := range creds {
		if c.IsGNMI() {
			if c.ID != in.CredID {
				continue // gNMI is only tried when chosen explicitly
			}
			g := poller.GNMIClient(ip.String(), c)
			g.Timeout = 5 * time.Second
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			ups, err := g.Get(ctx, []string{"/system/state"}, 2, gnmi.EncJSONIETF)
			cancel()
			g.Close()
			if err != nil {
				fail(w, 502, "gNMI: "+err.Error())
				return
			}
			t := gnmi.Tree(ups)
			if name == "" {
				name = gnmi.Str(gnmi.Lookup(t, "/system/state/hostname"))
			}
			descr, credUsed = "gNMI / OpenConfig", c.ID
			break
		}
		if !c.IsSNMP() {
			continue
		}
		cl := poller.NewClient(ip.String(), c)
		cl.Timeout = 1500 * time.Millisecond
		vbs, err := cl.Get(profile.OIDSysName, profile.OIDSysDescr, profile.OIDSysObjectID)
		cl.Close()
		if err == nil && len(vbs) == 3 {
			if name == "" {
				name = vbs[0].Value.String()
			}
			descr, oid, credUsed = vbs[1].Value.String(), vbs[2].Value.OID, c.ID
			break
		}
	}
	dev, added := s.d.Discovery.Register(in.SiteID, ip.String(), name, descr, oid, credUsed, "user", nil)
	if !added {
		fail(w, 409, "device already exists")
		return
	}
	s.d.Poller.PollNow(dev.ID)
	s.d.Engine.Broadcast(state.Change{Type: "device", Data: dev})
	if descr == "" {
		writeJSON(w, 201, map[string]any{"device": dev, "warning": "added, but SNMP did not answer with any credential — check community/user and ACLs"})
		return
	}
	writeJSON(w, 201, map[string]any{"device": dev})
}

// getDeviceHealth answers the topology hover card: one small JSON built
// from the last samples already in the store (no SNMP, no history read).
func (s *Server) getDeviceHealth(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id := r.PathValue("id")
	d, err := s.d.Store.Device(id)
	if err != nil {
		fail(w, 404, "device not found")
		return
	}
	cause := ""
	if d.Cause != "" {
		if cd, err := s.d.Store.Device(d.Cause); err == nil {
			cause = cd.Name
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, deviceHealth(d, s.d.Store.Interfaces(id), s.d.Store.Alerts(), cause))
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id := r.PathValue("id")
	d, err := s.d.Store.Device(id)
	if err != nil {
		fail(w, 404, "device not found")
		return
	}
	ifs := s.d.Store.Interfaces(id)
	var links []model.Link
	for _, l := range s.d.Store.Links() {
		if l.ADevice == id || l.BDevice == id {
			links = append(links, l)
		}
	}
	var alerts []model.Alert
	for _, a := range s.d.Store.Alerts() {
		if a.DeviceID == id && a.State != model.AlertResolved {
			alerts = append(alerts, a)
		}
	}
	if links == nil {
		links = []model.Link{}
	}
	if alerts == nil {
		alerts = []model.Alert{}
	}
	if ifs == nil {
		ifs = []model.Interface{}
	}
	names := map[string]string{}
	for _, l := range links {
		for _, x := range []string{l.ADevice, l.BDevice} {
			if od, err := s.d.Store.Device(x); err == nil {
				names[x] = od.Name
			}
		}
	}
	cause := ""
	if d.Cause != "" {
		if cd, err := s.d.Store.Device(d.Cause); err == nil {
			cause = cd.Name
		}
	}
	_, hasW := s.d.Store.Wireless(id)
	writeJSON(w, 200, map[string]any{"device": d, "interfaces": ifs, "links": links, "names": names, "neighbors": s.d.Store.Neighbors(id),
		"alerts": alerts, "events": s.d.Store.RecentEvents(id, 50), "cause_name": cause, "has_wireless": hasW || len(s.d.Store.SDWAN(id)) > 0})
}

func (s *Server) updateDevice(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id := r.PathValue("id")
	var in struct {
		Name        *string `json:"name"`
		Role        *string `json:"role"`
		Domain      *string `json:"domain"`
		PollEvery   *int    `json:"poll_every"`
		Monitored   *bool   `json:"monitored"`
		Notes       *string `json:"notes"`
		CredID      *string `json:"cred_id"`
		SiteID      *string `json:"site_id"`
		Location    *string `json:"location"`
		PingOnly    *bool   `json:"ping_only"`
		SSHCredID   *string `json:"ssh_cred_id"`
		BackupEvery *int    `json:"backup_every"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	caps := s.caps()
	_, monitored := s.d.Store.DeviceCount()
	ok := s.d.Store.UpdateDevice(id, func(d *model.Device) {
		if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
			d.Name = strings.TrimSpace(*in.Name)
		}
		if in.Role != nil {
			d.Role = model.Role(*in.Role)
			d.RoleLocked = true
		}
		if in.Domain != nil {
			d.Domain = model.Domain(*in.Domain)
		}
		if in.PollEvery != nil && *in.PollEvery >= 15 && *in.PollEvery <= 3600 {
			d.PollEvery = *in.PollEvery
		}
		if in.Monitored != nil {
			if *in.Monitored && !d.Monitored {
				if license.Unlimited(caps.MaxDevices) || monitored < caps.MaxDevices {
					d.Monitored = true
					d.Notes = ""
				}
			} else if !*in.Monitored {
				d.Monitored = false
				d.Notes = "Disabled by an operator."
				d.Status = model.StatusUnknown
			}
		}
		if in.Notes != nil {
			d.Notes = *in.Notes
		}
		if in.PingOnly != nil {
			d.PingOnly = *in.PingOnly // switching to SNMP: the next poll runs inventory
		}
		if in.SSHCredID != nil {
			d.SSHCredID = *in.SSHCredID
		}
		if in.BackupEvery != nil && *in.BackupEvery >= -1 && *in.BackupEvery <= 24*30 {
			d.BackupEvery = *in.BackupEvery
		}
		if in.CredID != nil {
			d.CredID = *in.CredID
		}
		if in.SiteID != nil {
			if _, err := s.d.Store.Site(*in.SiteID); err == nil {
				d.SiteID = *in.SiteID
			}
		}
		if in.Location != nil {
			d.Location = *in.Location
		}
	})
	if !ok {
		fail(w, 404, "device not found")
		return
	}
	d, _ := s.d.Store.Device(id)
	if in.Monitored != nil && *in.Monitored && !d.Monitored {
		writeJSON(w, 402, map[string]any{"error": fmt.Sprintf("the %s tier monitors up to %d devices", caps.Tier.Title(), caps.MaxDevices), "device": d})
		return
	}
	s.d.Engine.Broadcast(state.Change{Type: "device", Data: d})
	writeJSON(w, 200, d)
}

func (s *Server) addLink(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var in struct{ ADevice, AIf, BDevice, BIf string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	if _, err := s.d.Store.Device(in.ADevice); err != nil {
		fail(w, 400, "device A not found")
		return
	}
	if _, err := s.d.Store.Device(in.BDevice); err != nil {
		fail(w, 400, "device B not found")
		return
	}
	l := model.Link{ID: model.NewID("lnk"), ADevice: in.ADevice, AIf: in.AIf, BDevice: in.BDevice, BIf: in.BIf, Layer: "L2", Confidence: 1, Sources: []string{"manual"},
		Status: model.StatusUp, FirstSeen: time.Now(), LastSeen: time.Now(), Manual: true}
	s.d.Store.PutLink(l)
	s.d.Engine.InvalidateTopology()
	s.d.Topology.Layout()
	writeJSON(w, 201, l)
}

// ---- topology ----

func (s *Server) topology(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	site := r.URL.Query().Get("site")
	minConf, _ := strconv.ParseFloat(r.URL.Query().Get("min_confidence"), 64)
	if minConf == 0 {
		minConf = 0.5
	}
	layout := s.d.Store.Layout()
	type node struct {
		ID        string       `json:"id"`
		Name      string       `json:"name"`
		SiteID    string       `json:"site_id"`
		Role      model.Role   `json:"role"`
		Domain    model.Domain `json:"domain"`
		Status    model.Status `json:"status"`
		Vendor    string       `json:"vendor,omitempty"`
		Model     string       `json:"model,omitempty"`
		IP        string       `json:"ip"`
		X, Y, Z   float64
		Cause     string  `json:"cause,omitempty"`
		Alerts    int     `json:"alerts"`
		CPU       float64 `json:"cpu,omitempty"`
		External  bool    `json:"external,omitempty"`
		Monitored bool    `json:"monitored"`
	}
	alertCount := map[string]int{}
	for _, a := range s.d.Store.Alerts() {
		if a.State != model.AlertResolved {
			alertCount[a.DeviceID]++
		}
	}
	var nodes []node
	inSite := map[string]bool{}
	for _, d := range s.d.Store.Devices() {
		if site != "" && d.SiteID != site {
			continue
		}
		inSite[d.ID] = true
		p := layout[d.ID]
		nodes = append(nodes, node{ID: d.ID, Name: d.Name, SiteID: d.SiteID, Role: d.Role, Domain: d.Domain, Status: d.Status, Vendor: d.Vendor, Model: d.Model, IP: d.IP,
			X: p[0], Y: p[1], Z: p[2], Cause: d.Cause, Alerts: alertCount[d.ID], CPU: d.Metrics["cpu_pct"], Monitored: d.Monitored})
	}
	var links []model.Link
	ext := map[string]bool{}
	for _, l := range s.d.Store.Links() {
		if l.Confidence < minConf && !l.Manual {
			continue
		}
		if !inSite[l.ADevice] && !(l.External && inSite[l.ADevice]) {
			if !inSite[l.BDevice] {
				continue
			}
		}
		if l.External {
			key := "ext:" + strings.ToLower(l.ExternalName)
			if !ext[key] {
				ext[key] = true
				var p [3]float64
				if a := layout[l.ADevice]; true {
					p = [3]float64{a[0] * 1.25, a[1] * 1.25, 0}
				}
				nodes = append(nodes, node{ID: l.BDevice, Name: l.ExternalName, SiteID: site, Role: model.RoleOther, Status: model.StatusUnknown, X: p[0], Y: p[1], Z: p[2], External: true})
			}
		}
		links = append(links, l)
	}
	if nodes == nil {
		nodes = []node{}
	}
	if links == nil {
		links = []model.Link{}
	}
	v, at := s.d.Store.TopologyVersion()
	writeJSON(w, 200, map[string]any{"version": v, "at": at, "nodes": nodes, "links": links})
}

// ---- metrics ----

func (s *Server) metrics(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	q := r.URL.Query()
	series := q["series"]
	if len(series) == 0 || len(series) > 40 {
		fail(w, 400, "give 1..40 series parameters")
		return
	}
	to := time.Now()
	from := to.Add(-24 * time.Hour)
	if v := q.Get("from"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			from = to.Add(-d)
		} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			from = time.Unix(n, 0)
		}
	}
	if v := q.Get("to"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			to = time.Unix(n, 0)
		}
	}
	// retention gate: free tier sees its retention window only
	caps := s.caps()
	if min := to.Add(-time.Duration(caps.RetentionDays) * 24 * time.Hour); from.Before(min) {
		from = min
	}
	out := map[string][]tsdb.Point{}
	for _, sname := range series {
		pts := s.d.DB.Query(sname, from.Unix(), to.Unix())
		if pts == nil {
			pts = []tsdb.Point{}
		}
		out[sname] = pts
	}
	writeJSON(w, 200, map[string]any{"from": from.Unix(), "to": to.Unix(), "series": out})
}

// ---- alerts ----

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	q := r.URL.Query()
	st, sev, site, rootOnly := q.Get("state"), q.Get("severity"), q.Get("site"), q.Get("root_only") == "true"
	var out []model.Alert
	names := map[string]string{}
	for _, a := range s.d.Store.Alerts() {
		switch st {
		case "", "active":
			if a.State == model.AlertResolved {
				continue
			}
		case "all":
		default:
			if string(a.State) != st {
				continue
			}
		}
		if sev != "" && string(a.Severity) != sev {
			continue
		}
		if site != "" && a.SiteID != site {
			continue
		}
		if rootOnly && a.RootCause != "" {
			continue
		}
		if d, err := s.d.Store.Device(a.DeviceID); err == nil {
			names[a.DeviceID] = d.Name
		}
		if sn, err := s.d.Store.Site(a.SiteID); err == nil {
			names[a.SiteID] = sn.Name
		}
		out = append(out, a)
	}
	if out == nil {
		out = []model.Alert{}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == model.AlertOpen
		}
		if out[i].Severity != out[j].Severity {
			return out[i].Severity.Rank() > out[j].Severity.Rank()
		}
		return out[i].OpenedAt.After(out[j].OpenedAt)
	})
	writeJSON(w, 200, map[string]any{"alerts": out, "names": names})
}

// ---- logs ----

func (s *Server) logQuery(r *http.Request) syslog.Query {
	q := r.URL.Query()
	qq := syslog.Query{DeviceID: q.Get("device"), Text: q.Get("q"), Source: q.Get("source"), MaxSev: -1}
	if v := q.Get("sev"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			qq.MaxSev = n
		}
	}
	qq.To = time.Now()
	qq.From = qq.To.Add(-24 * time.Hour)
	if v := q.Get("from"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			qq.From = qq.To.Add(-d)
		} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			qq.From = time.Unix(n, 0)
		}
	}
	if v := q.Get("to"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			qq.To = time.Unix(n, 0)
		}
	}
	caps := s.caps()
	if min := time.Now().Add(-time.Duration(caps.RetentionDays) * 24 * time.Hour); qq.From.Before(min) {
		qq.From = min
	}
	qq.Limit, _ = strconv.Atoi(q.Get("limit"))
	if qq.Limit <= 0 || qq.Limit > 5000 {
		qq.Limit = 500
	}
	return qq
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	q := s.logQuery(r)
	entries := s.d.Logs.Search(q)
	if entries == nil {
		entries = []model.LogEntry{}
	}
	names := map[string]string{}
	for _, e := range entries {
		if e.DeviceID != "" && names[e.DeviceID] == "" {
			if d, err := s.d.Store.Device(e.DeviceID); err == nil {
				names[e.DeviceID] = d.Name
			}
		}
	}
	writeJSON(w, 200, map[string]any{"entries": entries, "names": names, "from": q.From, "to": q.To})
}

// ---- SSE ----

func (s *Server) stream(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := s.d.Engine.Subscribe()
	defer s.d.Engine.Unsubscribe(ch)
	fmt.Fprintf(w, "event: hello\ndata: {\"version\":%q}\n\n", version.Version)
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	seq := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case c := <-ch:
			seq++
			b, _ := json.Marshal(c.Data)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", seq, c.Type, b)
			fl.Flush()
		}
	}
}

// ---- config snippets ----

func (s *Server) snippets(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	collector := r.URL.Query().Get("collector")
	if collector == "" {
		collector = "<collector-ip>"
	}
	credID := r.URL.Query().Get("cred")
	cred := model.Credential{Version: "3", User: "topolight", AuthProto: "sha", PrivProto: "aes"}
	if c, err := s.d.Store.Cred(credID); err == nil {
		cred = c
	}
	out := map[string]string{}
	trapPort := "162"
	if _, p, err := net.SplitHostPort(s.d.TrapAddr); err == nil && p != "" {
		trapPort = p
	}
	syslogPort := "514"
	if _, p, err := net.SplitHostPort(s.d.SyslogAddr); err == nil && p != "" {
		syslogPort = p
	}
	tlsPort := "6514"
	if _, p, err := net.SplitHostPort(s.d.SyslogTLSAddr); err == nil && p != "" {
		tlsPort = p
	}
	var cisco, nxos, fgt, junos, mikrotik, aruba strings.Builder
	if cred.Version == "3" {
		authP := strings.ToUpper(cred.AuthProto)
		if authP == "SHA256" {
			authP = "sha256"
		}
		privP := strings.ToUpper(cred.PrivProto)
		if privP == "AES" {
			privP = "aes 128"
		}
		fmt.Fprintf(&cisco, "! Cisco IOS / IOS-XE — generated by TopoLight for collector %s\nsnmp-server view TOPOLIGHT iso included\nsnmp-server group TOPOLIGHT-RO v3 priv read TOPOLIGHT\nsnmp-server group TOPOLIGHT-RO v3 priv context vlan- match prefix read TOPOLIGHT\nsnmp-server user %s TOPOLIGHT-RO v3 auth %s <auth-password> priv %s <priv-password>\nsnmp-server host %s version 3 priv %s\n! informs instead of traps (acknowledged): the remote engine id is TopoLight's, from Admin → System\n!  snmp-server engineID remote %s %s\n!  snmp-server user %s TOPOLIGHT-RO remote %s v3 auth %s <auth-password> priv %s <priv-password>\n!  snmp-server host %s informs version 3 priv %s\n", collector, cred.User, strings.ToLower(authP), privP, collector, cred.User, collector, s.d.Store.Settings().EngineID, cred.User, collector, strings.ToLower(authP), privP, collector, cred.User)
		fmt.Fprintf(&nxos, "! Cisco NX-OS — generated by TopoLight\nsnmp-server user %s network-operator auth %s <auth-password> priv %s <priv-password>\nsnmp-server host %s traps version 3 priv %s\n", cred.User, strings.ToLower(authP), strings.ToLower(strings.Fields(privP)[0]), collector, cred.User)
		fmt.Fprintf(&fgt, "# FortiGate — generated by TopoLight\nconfig system snmp sysinfo\n    set status enable\nend\nconfig system snmp user\n    edit \"%s\"\n        set security-level auth-priv\n        set auth-proto %s\n        set auth-pwd <auth-password>\n        set priv-proto %s\n        set priv-pwd <priv-password>\n        set notify-hosts %s\n        set events cpu-high mem-low log-full intf-ip ha-switch ha-hb-failure ips-signature ips-anomaly av-virus av-oversize fm-if-change\n    next\nend\n", cred.User, strings.ToLower(authP), strings.ToLower(strings.Fields(privP)[0]), collector)
		fmt.Fprintf(&junos, "# Junos — generated by TopoLight\nset snmp v3 usm local-engine user %s authentication-%s authentication-password <auth-password>\nset snmp v3 usm local-engine user %s privacy-%s privacy-password <priv-password>\nset snmp v3 vacm security-to-group security-model usm security-name %s group TOPOLIGHT\nset snmp v3 vacm access group TOPOLIGHT default-context-prefix security-model usm security-level privacy read-view all\nset snmp view all oid .1 include\n", cred.User, strings.ToLower(authP), cred.User, strings.ToLower(strings.Fields(privP)[0]), cred.User)
		fmt.Fprintf(&mikrotik, "# MikroTik RouterOS — generated by TopoLight\n/snmp community add name=%s security=private authentication-protocol=%s authentication-password=<auth-password> encryption-protocol=%s encryption-password=<priv-password> read-access=yes\n/snmp set enabled=yes trap-community=%s trap-target=%s trap-version=3\n", cred.User, authP, strings.ToUpper(strings.Fields(privP)[0]), cred.User, collector)
		fmt.Fprintf(&aruba, "# Aruba AOS-S — generated by TopoLight\nsnmpv3 enable\nsnmpv3 user %s auth %s <auth-password> priv %s <priv-password>\nsnmpv3 group operatorauth user %s sec-model ver3\nsnmpv3 targetaddress \"topolight\" params \"tl\" %s\n", cred.User, strings.ToLower(authP), strings.ToLower(strings.Fields(privP)[0]), cred.User, collector)
	} else {
		fmt.Fprintf(&cisco, "! Cisco IOS / IOS-XE — generated by TopoLight (v2c: prefer v3 in production)\nsnmp-server community <community> RO\nsnmp-server host %s version 2c <community>\n", collector)
		fmt.Fprintf(&nxos, "! Cisco NX-OS\nsnmp-server community <community> group network-operator\nsnmp-server host %s traps version 2c <community>\n", collector)
		fmt.Fprintf(&fgt, "# FortiGate\nconfig system snmp community\n    edit 1\n        set name \"<community>\"\n        config hosts\n            edit 1\n                set ip %s 255.255.255.255\n            next\n        end\n    next\nend\n", collector)
		fmt.Fprintf(&junos, "# Junos\nset snmp community <community> authorization read-only\nset snmp trap-group topolight targets %s\n", collector)
		fmt.Fprintf(&mikrotik, "# MikroTik\n/snmp community set [find default=yes] name=<community>\n/snmp set enabled=yes trap-target=%s trap-community=<community> trap-version=2\n", collector)
		fmt.Fprintf(&aruba, "# Aruba AOS-S\nsnmp-server community \"<community>\" operator\nsnmp-server host %s community \"<community>\"\n", collector)
	}
	fmt.Fprintf(&cisco, "snmp-server enable traps snmp linkdown linkup coldstart warmstart authentication\nsnmp-server enable traps envmon\nsnmp-server ifindex persist\nlogging host %s transport udp port %s\n! or over TLS (IOS-XE, needs a trustpoint that trusts the collector certificate):\n!  logging host %s transport tls port %s\nlogging trap informational\nservice timestamps log datetime msec localtime show-timezone year\nlldp run\ncdp run\n", collector, syslogPort, collector, tlsPort)
	fmt.Fprintf(&nxos, "snmp-server enable traps link\nlogging server %s 6 port %s\nfeature lldp\n", collector, syslogPort)
	fmt.Fprintf(&fgt, "config log syslogd setting\n    set status enable\n    set server \"%s\"\n    set port %s\n    set facility local7\n    # for TLS instead: set mode reliable / set port %s / set enc-algorithm high (import the collector certificate first)\nend\nconfig system interface\n    edit \"<mgmt-interface>\"\n        set allowaccess ping snmp\n        set lldp-transmission enable\n    next\nend\n", collector, syslogPort, tlsPort)
	fmt.Fprintf(&junos, "set system syslog host %s any notice\nset system syslog host %s port %s\nset protocols lldp interface all\n", collector, collector, syslogPort)
	fmt.Fprintf(&mikrotik, "/system logging action add name=topolight target=remote remote=%s remote-port=%s\n/system logging add action=topolight topics=info,!debug\n/ip neighbor discovery-settings set discover-interface-list=all protocol=lldp,cdp\n", collector, syslogPort)
	fmt.Fprintf(&aruba, "logging %s\nlogging facility local7\nlldp run\n", collector)
	// flow export (optional — comment out if the device should not export flows)
	flowPort, sflowPort := "", ""
	if _, p, err := net.SplitHostPort(s.d.FlowAddr); err == nil && p != "" {
		flowPort = p
	}
	if _, p, err := net.SplitHostPort(s.d.SFlowAddr); err == nil && p != "" {
		sflowPort = p
	}
	if flowPort != "" {
		fmt.Fprintf(&cisco, "! NetFlow v9 (optional)\nflow exporter TOPOLIGHT\n destination %s\n transport udp %s\n template data timeout 60\nflow record TOPOLIGHT-REC\n match ipv4 protocol\n match ipv4 source address\n match ipv4 destination address\n match transport source-port\n match transport destination-port\n match interface input\n collect interface output\n collect counter bytes\n collect counter packets\nflow monitor TOPOLIGHT-MON\n exporter TOPOLIGHT\n record TOPOLIGHT-REC\n cache timeout active 60\n! apply on the WAN / uplink interface(s):\n!  interface <uplink>\n!   ip flow monitor TOPOLIGHT-MON input\n!   ip flow monitor TOPOLIGHT-MON output\n", collector, flowPort)
		fmt.Fprintf(&nxos, "! NetFlow v9 (optional)\nfeature netflow\nflow exporter TOPOLIGHT\n  destination %s\n  transport udp %s\n  version 9\nflow record TOPOLIGHT-REC\n  match ipv4 source address\n  match ipv4 destination address\n  match ip protocol\n  match transport source-port\n  match transport destination-port\n  collect counter bytes\n  collect counter packets\nflow monitor TOPOLIGHT-MON\n  record TOPOLIGHT-REC\n  exporter TOPOLIGHT\n! interface <uplink>: ip flow monitor TOPOLIGHT-MON input\n", collector, flowPort)
		fmt.Fprintf(&fgt, "# NetFlow v9 (optional)\nconfig system netflow\n    set collector-ip %s\n    set collector-port %s\n    set active-flow-timeout 1\nend\nconfig system interface\n    edit \"<wan-interface>\"\n        set netflow-sampler both\n    next\nend\n", collector, flowPort)
		fmt.Fprintf(&junos, "# IPFIX (optional, MX/SRX)\nset services flow-monitoring version-ipfix template TOPOLIGHT ipv4-template\nset forwarding-options sampling instance TOPOLIGHT input rate 1\nset forwarding-options sampling instance TOPOLIGHT family inet output flow-server %s port %s version-ipfix template TOPOLIGHT\nset forwarding-options sampling instance TOPOLIGHT family inet output inline-jflow source-address <router-ip>\n# interface <uplink> unit 0 family inet sampling input output\n", collector, flowPort)
		fmt.Fprintf(&mikrotik, "# Traffic Flow — NetFlow v9 (optional)\n/ip traffic-flow set enabled=yes interfaces=all active-flow-timeout=1m\n/ip traffic-flow target add dst-address=%s port=%s version=9\n", collector, flowPort)
	}
	if sflowPort != "" {
		fmt.Fprintf(&aruba, "# sFlow (optional)\nsflow 1 destination %s %s\nsflow 1 sampling all 512\nsflow 1 polling all 60\n", collector, sflowPort)
	}
	out["cisco-ios"] = cisco.String()
	out["cisco-nxos"] = nxos.String()
	out["fortinet"] = fgt.String()
	out["juniper"] = junos.String()
	out["mikrotik"] = mikrotik.String()
	out["aruba"] = aruba.String()
	ports := fmt.Sprintf("SNMP polling from %s → devices UDP/161 · traps → collector UDP/%s · syslog → collector UDP or TCP/%s · console TCP/%s", collector, trapPort, syslogPort, portOf(s.d.Listen))
	if flowPort != "" {
		ports += " · syslog TLS → collector TCP/" + tlsPort + " · NetFlow/IPFIX → collector UDP/" + flowPort
	}
	if sflowPort != "" {
		ports += " · sFlow → collector UDP/" + sflowPort
	}
	out["_ports"] = ports
	writeJSON(w, 200, out)
}

func portOf(listen string) string {
	if _, p, err := net.SplitHostPort(listen); err == nil {
		return p
	}
	return strconv.Itoa(version.Port)
}

// ReadLicenseKey loads the licence key file from the data dir.
func ReadLicenseKey(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "license.key"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WriteLicenseKey stores the key in the data dir.
func WriteLicenseKey(dir, key string) error {
	if dir == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, "license.key"), []byte(key+"\n"), 0o600)
}

// ---- flow -------------------------------------------------------------------

func flowWindowDuration(s string) time.Duration {
	switch s {
	case "5m":
		return 5 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "1h", "":
		return time.Hour
	case "6h":
		return 6 * time.Hour
	case "24h":
		return 24 * time.Hour
	}
	return time.Hour
}

// flowExporter resolves ?device=<id> or ?exporter=<ip> to the exporter address.
func (s *Server) flowExporter(r *http.Request) (string, model.Device, bool) {
	if id := r.URL.Query().Get("device"); id != "" {
		d, err := s.d.Store.Device(id)
		if err != nil {
			return "", model.Device{}, false
		}
		return d.IP, d, true
	}
	return r.URL.Query().Get("exporter"), model.Device{}, true
}

// flowWindow answers GET /api/flow?device=|exporter=&window=5m|15m|1h|6h|24h
// with a merged Summary; interface rows carry names when the exporter is a device.
func (s *Server) flowWindow(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if s.d.Flow == nil {
		fail(w, 503, "flow collector disabled")
		return
	}
	exp, dev, ok := s.flowExporter(r)
	if !ok {
		fail(w, 404, "device not found")
		return
	}
	win := r.URL.Query().Get("window")
	sum := s.d.Flow.Agg.Window(exp, flowWindowDuration(win), time.Now())
	type ifRow struct {
		flow.IfStat
		Name  string `json:"name,omitempty"`
		Alias string `json:"alias,omitempty"`
		IfID  string `json:"if_id,omitempty"`
	}
	names := map[string]string{}
	rows := make([]ifRow, 0, len(sum.Ifaces))
	if dev.ID != "" {
		byIdx := map[int]model.Interface{}
		for _, i := range s.d.Store.Interfaces(dev.ID) {
			byIdx[i.Index] = i
		}
		for _, st := range sum.Ifaces {
			row := ifRow{IfStat: st}
			if i, ok := byIdx[int(st.IfIndex)]; ok {
				row.Name, row.Alias, row.IfID = i.Name, i.Alias, i.ID
			}
			rows = append(rows, row)
		}
	}
	// device names for addresses that are monitored devices (nice in the tables)
	for _, e := range append(append([]flow.Entry{}, sum.Talkers...), sum.Targets...) {
		if d, ok := s.d.Store.DeviceByIP(e.Key); ok {
			names[e.Key] = d.Name
		}
	}
	writeJSON(w, 200, map[string]any{"summary": sum, "window": win, "ifaces": rows, "names": names, "device": dev.ID, "device_name": dev.Name})
}

func (s *Server) flowExporters(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if s.d.Flow == nil {
		fail(w, 503, "flow collector disabled")
		return
	}
	type row struct {
		flow.ExporterInfo
		DeviceID string `json:"device_id,omitempty"`
		Name     string `json:"name,omitempty"`
	}
	var out []row
	for _, e := range s.d.Flow.Agg.Exporters() {
		x := row{ExporterInfo: e}
		if d, ok := s.d.Store.DeviceByIP(e.Exporter); ok {
			x.DeviceID, x.Name = d.ID, d.Name
		}
		out = append(out, x)
	}
	if out == nil {
		out = []row{}
	}
	writeJSON(w, 200, map[string]any{"exporters": out, "stats": s.d.Flow.Stats()})
}

func (s *Server) flowSeries(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if s.d.Flow == nil {
		fail(w, 503, "flow collector disabled")
		return
	}
	exp, _, ok := s.flowExporter(r)
	if !ok {
		fail(w, 404, "device not found")
		return
	}
	writeJSON(w, 200, map[string]any{"points": s.d.Flow.Agg.Series(exp, flowWindowDuration(r.URL.Query().Get("window")), time.Now())})
}

// ---- endpoints ----------------------------------------------------------------

// listEndpoints answers GET /api/endpoints?q=&device=&if=&limit= — newest first,
// with device names resolved for the tables.
func (s *Server) listEndpoints(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if s.d.Endpoints == nil {
		fail(w, 503, "endpoint tracking disabled")
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	ifIndex, _ := strconv.Atoi(q.Get("if"))
	list := s.d.Endpoints.Query(q.Get("q"), q.Get("device"), ifIndex, limit)
	names := map[string]string{}
	for _, e := range list {
		for _, id := range []string{e.DeviceID, e.ARPDevice} {
			if id != "" && names[id] == "" {
				if d, err := s.d.Store.Device(id); err == nil {
					names[id] = d.Name
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"endpoints": list, "names": names, "stats": s.d.Endpoints.Stats(time.Now())})
}

func (s *Server) getEndpoint(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if s.d.Endpoints == nil {
		fail(w, 503, "endpoint tracking disabled")
		return
	}
	e, ok := s.d.Endpoints.Get(r.PathValue("mac"))
	if !ok {
		fail(w, 404, "unknown MAC")
		return
	}
	writeJSON(w, 200, e)
}

// deviceEndpoints answers GET /api/devices/{id}/endpoints: everything placed on
// this device grouped per port, plus per-port counts for the interface table.
func (s *Server) deviceEndpoints(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if s.d.Endpoints == nil {
		fail(w, 503, "endpoint tracking disabled")
		return
	}
	id := r.PathValue("id")
	if _, err := s.d.Store.Device(id); err != nil {
		fail(w, 404, "device not found")
		return
	}
	list := s.d.Endpoints.Query("", id, 0, 0)
	placed := make([]endpoint.Endpoint, 0, len(list))
	resolved := make([]endpoint.Endpoint, 0)
	for _, e := range list {
		if e.DeviceID == id {
			placed = append(placed, e)
		} else {
			resolved = append(resolved, e) // ARP-only: this device resolved the IP but did not learn the MAC on an access port
		}
	}
	counts := map[string]int{}
	for k, v := range s.d.Endpoints.PortCounts(id) {
		counts[strconv.Itoa(k)] = v
	}
	names := map[string]string{}
	for _, e := range resolved {
		if e.DeviceID != "" && names[e.DeviceID] == "" {
			if d, err := s.d.Store.Device(e.DeviceID); err == nil {
				names[e.DeviceID] = d.Name
			}
		}
	}
	writeJSON(w, 200, map[string]any{"placed": placed, "resolved": resolved, "port_counts": counts, "names": names})
}

// ---- API tokens ----------------------------------------------------------------

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	type row struct {
		ID, Name, Role, Prefix, Creator string
		Created, LastUsed               time.Time
	}
	out := []row{}
	for _, t := range s.d.Store.Tokens() {
		out = append(out, row{t.ID, t.Name, t.Role, t.Prefix, t.Creator, t.Created, t.LastUsed})
	}
	writeJSON(w, 200, out)
}

// addToken creates a bearer token; the secret is returned exactly once.
func (s *Server) addToken(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	var in struct{ Name, Role string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 60 {
		fail(w, 400, "a short name is required")
		return
	}
	switch in.Role {
	case "viewer", "operator", "admin":
	default:
		in.Role = "viewer"
	}
	if !roleAllows(sess.Role, in.Role) {
		fail(w, 403, "a token cannot outrank its creator")
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		fail(w, 500, "entropy")
		return
	}
	secret := "tl_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	t := model.APIToken{ID: model.NewID("tok"), Name: in.Name, Role: in.Role, Hash: hex.EncodeToString(sum[:]), Prefix: secret[:8], Created: time.Now(), Creator: sess.Name}
	s.d.Store.PutToken(t)
	writeJSON(w, 201, map[string]any{"id": t.ID, "name": t.Name, "role": t.Role, "prefix": t.Prefix, "secret": secret,
		"example": "curl -H 'Authorization: Bearer " + secret + "' " + s.baseURL(r) + "/api/status"})
}

func (s *Server) baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ---- probes -----------------------------------------------------------------------

func (s *Server) listProbes(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	names := map[string]string{}
	list := s.d.Store.Probes()
	for _, p := range list {
		if p.DeviceID != "" {
			if d, err := s.d.Store.Device(p.DeviceID); err == nil {
				names[p.DeviceID] = d.Name
			}
		}
	}
	var sums map[string]probe.Summary
	if s.d.Probes != nil {
		sums = s.d.Probes.Summaries()
	}
	writeJSON(w, 200, map[string]any{"probes": list, "summaries": sums, "names": names, "types": probe.Types, "traceroute": s.d.Probes != nil && s.d.Probes.Traceroute != nil})
}

func (s *Server) getProbe(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	p, err := s.d.Store.Probe(r.PathValue("id"))
	if err != nil {
		fail(w, 404, "probe not found")
		return
	}
	out := map[string]any{"probe": p}
	if s.d.Probes != nil {
		out["history"] = s.d.Probes.History(p.ID)
		out["path"] = s.d.Probes.Path(p.ID)
		if l, ok := s.d.Probes.Last(p.ID); ok {
			out["last"] = l
		}
	}
	writeJSON(w, 200, out)
}

// putProbe creates (POST) or updates (PUT) a probe.
func (s *Server) putProbe(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var in model.Probe
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	in.Name, in.Target, in.Type, in.Expect, in.Resolver = strings.TrimSpace(in.Name), strings.TrimSpace(in.Target), strings.TrimSpace(in.Type), strings.TrimSpace(in.Expect), strings.TrimSpace(in.Resolver)
	valid := false
	for _, t := range probe.Types {
		if t == in.Type {
			valid = true
		}
	}
	if !valid {
		fail(w, 400, "type must be one of "+strings.Join(probe.Types, ", "))
		return
	}
	if in.Target == "" {
		fail(w, 400, "a target is required")
		return
	}
	if in.Name == "" {
		in.Name = in.Type + " " + in.Target
	}
	if in.Every < 10 {
		in.Every = 60
	}
	if in.Type == "traceroute" && in.Every < 300 {
		in.Every = 300
	}
	if in.Timeout <= 0 || in.Timeout > 60 {
		in.Timeout = 5
	}
	if in.DeviceID != "" {
		if _, err := s.d.Store.Device(in.DeviceID); err != nil {
			in.DeviceID = ""
		}
	}
	if id := r.PathValue("id"); id != "" {
		old, err := s.d.Store.Probe(id)
		if err != nil {
			fail(w, 404, "probe not found")
			return
		}
		in.ID, in.Created = old.ID, old.Created
	} else {
		in.ID, in.Created, in.Enabled = model.NewID("prb"), time.Now(), true
	}
	s.d.Store.PutProbe(in)
	if s.d.Probes != nil {
		s.d.Probes.Now(in.ID)
	}
	writeJSON(w, 200, in)
}

// ---- configuration backups --------------------------------------------------------

func (s *Server) deviceConfigs(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id := r.PathValue("id")
	d, err := s.d.Store.Device(id)
	if err != nil {
		fail(w, 404, "device not found")
		return
	}
	vs, st := s.d.Backup.Cfg.Versions(id)
	_, hasCred := s.d.Backup.CredFor(d)
	rc := backup.RecipeFor(d.ProfileID)
	writeJSON(w, 200, map[string]any{"versions": vs, "status": st, "has_cred": hasCred, "recipe": map[string]any{"show": rc.Show, "exec": rc.Exec}, "every_hours": func() int {
		if d.BackupEvery != 0 {
			return d.BackupEvery
		}
		if h := s.d.Store.Settings().BackupEveryHours; h != 0 {
			return h
		}
		return 24
	}()})
}

func (s *Server) deviceConfig(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	txt, err := s.d.Backup.Cfg.Read(r.PathValue("id"), r.PathValue("ver"))
	if err != nil {
		fail(w, 404, "version not found")
		return
	}
	if r.URL.Query().Get("raw") != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+r.PathValue("id")+"-"+r.PathValue("ver")+".txt\"")
		w.Write([]byte(txt))
		return
	}
	writeJSON(w, 200, map[string]any{"text": txt})
}

// deviceConfigDiff answers ?context=N (default 3; 0 = full) with hunks between two versions.
func (s *Server) deviceConfigDiff(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id := r.PathValue("id")
	a, err := s.d.Backup.Cfg.Read(id, r.PathValue("ver"))
	if err != nil {
		fail(w, 404, "version not found")
		return
	}
	b, err := s.d.Backup.Cfg.Read(id, r.PathValue("other"))
	if err != nil {
		fail(w, 404, "version not found")
		return
	}
	volatile := r.URL.Query().Get("volatile") == "1"
	if !volatile {
		d, err := s.d.Store.Device(id)
		if err == nil {
			rc := backup.RecipeFor(d.ProfileID)
			a, b = backup.Normalise(a, rc), backup.Normalise(b, rc)
		}
	}
	ops := backup.Diff(strings.Split(strings.TrimRight(a, "\n"), "\n"), strings.Split(strings.TrimRight(b, "\n"), "\n"))
	added, removed := backup.Counts(ops)
	ctxLines := 3
	if c := r.URL.Query().Get("context"); c != "" {
		ctxLines, _ = strconv.Atoi(c)
	}
	if ctxLines > 0 {
		ops = backup.Hunks(ops, ctxLines)
	}
	writeJSON(w, 200, map[string]any{"ops": ops, "added": added, "removed": removed})
}

func (s *Server) backupNow(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	d, err := s.d.Store.Device(r.PathValue("id"))
	if err != nil {
		fail(w, 404, "device not found")
		return
	}
	v, changed, err := s.d.Backup.Backup(r.Context(), d, "user")
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "version": v, "changed": changed})
}

// configOverview lists every device with its backup status (Devices page column / report).
func (s *Server) configOverview(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	st := s.d.Backup.Cfg.Statuses()
	type row struct {
		DeviceID string         `json:"device_id"`
		Name     string         `json:"name"`
		HasCred  bool           `json:"has_cred"`
		Status   *backup.Status `json:"status,omitempty"`
	}
	out := []row{}
	for _, d := range s.d.Store.Devices() {
		if d.PingOnly {
			continue
		}
		_, ok := s.d.Backup.CredFor(d)
		x := row{DeviceID: d.ID, Name: d.Name, HasCred: ok}
		if v, has := st[d.ID]; has {
			vv := v
			x.Status = &vv
		}
		out = append(out, x)
	}
	writeJSON(w, 200, map[string]any{"devices": out, "stats": s.d.Backup.Stats()})
}

// ---- reports ------------------------------------------------------------------------

func (s *Server) listReports(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	writeJSON(w, 200, map[string]any{"reports": s.d.Store.Reports(), "sections": report.Sections, "files": s.d.Reports.List(""), "smtp": s.d.Notify != nil && s.d.Notify.SMTP.Host != ""})
}

func (s *Server) putReport(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var in model.Report
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = "Network report"
	}
	var secs []string
	for _, x := range in.Sections {
		for _, ok := range report.Sections {
			if x == ok {
				secs = append(secs, x)
			}
		}
	}
	in.Sections = secs
	switch in.Period {
	case "24h", "7d", "30d":
	default:
		in.Period = "7d"
	}
	switch in.Schedule {
	case "", "daily", "weekly", "monthly":
	default:
		in.Schedule = ""
	}
	if in.Hour < 0 || in.Hour > 23 {
		in.Hour = 7
	}
	var to []string
	for _, e := range in.EmailTo {
		if e = strings.TrimSpace(e); e != "" {
			to = append(to, e)
		}
	}
	in.EmailTo = to
	if id := r.PathValue("id"); id != "" {
		old, err := s.d.Store.Report(id)
		if err != nil {
			fail(w, 404, "report not found")
			return
		}
		in.ID, in.Created, in.LastRun, in.LastErr = old.ID, old.Created, old.LastRun, old.LastErr
	} else {
		in.ID, in.Created, in.Enabled = model.NewID("rpt"), time.Now(), true
	}
	s.d.Store.PutReport(in)
	writeJSON(w, 200, in)
}

// runReport generates now; ?mail=1 also sends it to the recipients.
func (s *Server) runReport(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	rep, err := s.d.Store.Report(r.PathValue("id"))
	if err != nil {
		fail(w, 404, "report not found")
		return
	}
	_, file, err := s.d.Reports.RunReport(rep, time.Now(), r.URL.Query().Get("mail") == "1")
	out := map[string]any{"ok": err == nil, "file": file}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, 200, out)
}

// previewReport renders an ad-hoc report: ?sections=a,b&period=7d&site=&format=html|csv|json
func (s *Server) previewReport(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	q := r.URL.Query()
	rep := model.Report{Name: q.Get("name"), Period: q.Get("period"), SiteID: q.Get("site")}
	if sec := q.Get("sections"); sec != "" {
		rep.Sections = strings.Split(sec, ",")
	}
	if rep.Name == "" {
		rep.Name = "Network report"
	}
	res := report.Generate(s.d.Reports.Deps, rep, time.Now())
	switch q.Get("format") {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=\"topolight-report.csv\"")
		w.Write(report.CSV(res))
	case "json":
		writeJSON(w, 200, res)
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(report.HTML(res, s.d.Reports.Deps.Instance)))
	}
}
