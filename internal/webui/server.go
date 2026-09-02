// Package webui serves the console: REST API, live stream (SSE) and the
// embedded single-page UI. No external asset is ever requested.
package webui

import (
	"context"
	"embed"
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
	"github.com/nizartuanku/topolight/internal/discovery"
	"github.com/nizartuanku/topolight/internal/license"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/notify"
	"github.com/nizartuanku/topolight/internal/poller"
	"github.com/nizartuanku/topolight/internal/profile"
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
	c, err := r.Cookie("topolight_session")
	if err != nil {
		return auth.Session{}, false
	}
	return s.sessions.Get(c.Value)
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
		if r.Method != "GET" && r.Method != "HEAD" {
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
		c := poller.NewClient(in.IP, cred)
		c.Timeout = 2 * time.Second
		defer c.Close()
		start := time.Now()
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
	m.HandleFunc("PUT /api/devices/{id}", s.require("operator", s.updateDevice))
	m.HandleFunc("DELETE /api/devices/{id}", s.require("admin", func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		id := r.PathValue("id")
		s.d.Store.DeleteDevice(id)
		s.d.Poller.Forget(id)
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
		"license": map[string]any{"tier": lic.Tier, "notice": lic.Notice, "valid": lic.Valid, "caps": lic.Caps},
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
			"trap_received": s.d.Trap.Received, "trap_rejected": s.d.Trap.Rejected,
			"series": s.d.DB.SeriesCount(), "tsdb_bytes": s.d.DB.DiskUsage(), "logs_count": s.d.Logs.Count,
			"syslog_addr": s.d.SyslogAddr, "trap_addr": s.d.TrapAddr, "icmp": s.d.ICMPError == "", "icmp_error": s.d.ICMPError,
			"notify_sent": s.d.Notify.Sent, "notify_failed": s.d.Notify.Failed,
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
	old.Name, old.Subnets, old.Lat, old.Lon, old.CredID, old.Disabled = in.Name, in.Subnets, in.Lat, in.Lon, in.CredID, in.Disabled
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
	if in.Version != "3" {
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
		in.ID = id
	} else {
		in.ID = model.NewID("cred")
	}
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
	writeJSON(w, 200, map[string]any{"device": d, "interfaces": ifs, "links": links, "names": names, "neighbors": s.d.Store.Neighbors(id),
		"alerts": alerts, "events": s.d.Store.RecentEvents(id, 50), "cause_name": cause})
}

func (s *Server) updateDevice(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id := r.PathValue("id")
	var in struct {
		Name      *string `json:"name"`
		Role      *string `json:"role"`
		Domain    *string `json:"domain"`
		PollEvery *int    `json:"poll_every"`
		Monitored *bool   `json:"monitored"`
		Notes     *string `json:"notes"`
		CredID    *string `json:"cred_id"`
		SiteID    *string `json:"site_id"`
		Location  *string `json:"location"`
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
		fmt.Fprintf(&cisco, "! Cisco IOS / IOS-XE — generated by TopoLight for collector %s\nsnmp-server view TOPOLIGHT iso included\nsnmp-server group TOPOLIGHT-RO v3 priv read TOPOLIGHT\nsnmp-server user %s TOPOLIGHT-RO v3 auth %s <auth-password> priv %s <priv-password>\nsnmp-server host %s version 3 priv %s\n", collector, cred.User, strings.ToLower(authP), privP, collector, cred.User)
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
	fmt.Fprintf(&cisco, "snmp-server enable traps snmp linkdown linkup coldstart warmstart authentication\nsnmp-server enable traps envmon\nsnmp-server ifindex persist\nlogging host %s transport udp port %s\nlogging trap informational\nservice timestamps log datetime msec localtime show-timezone year\nlldp run\ncdp run\n", collector, syslogPort)
	fmt.Fprintf(&nxos, "snmp-server enable traps link\nlogging server %s 6 port %s\nfeature lldp\n", collector, syslogPort)
	fmt.Fprintf(&fgt, "config log syslogd setting\n    set status enable\n    set server \"%s\"\n    set port %s\n    set facility local7\nend\nconfig system interface\n    edit \"<mgmt-interface>\"\n        set allowaccess ping snmp\n        set lldp-transmission enable\n    next\nend\n", collector, syslogPort)
	fmt.Fprintf(&junos, "set system syslog host %s any notice\nset system syslog host %s port %s\nset protocols lldp interface all\n", collector, collector, syslogPort)
	fmt.Fprintf(&mikrotik, "/system logging action add name=topolight target=remote remote=%s remote-port=%s\n/system logging add action=topolight topics=info,!debug\n/ip neighbor discovery-settings set discover-interface-list=all protocol=lldp,cdp\n", collector, syslogPort)
	fmt.Fprintf(&aruba, "logging %s\nlogging facility local7\nlldp run\n", collector)
	out["cisco-ios"] = cisco.String()
	out["cisco-nxos"] = nxos.String()
	out["fortinet"] = fgt.String()
	out["juniper"] = junos.String()
	out["mikrotik"] = mikrotik.String()
	out["aruba"] = aruba.String()
	out["_ports"] = fmt.Sprintf("SNMP polling from %s → devices UDP/161 · traps → collector UDP/%s · syslog → collector UDP or TCP/%s · console TCP/%s", collector, trapPort, syslogPort, portOf(s.d.Listen))
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
