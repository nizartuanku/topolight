package webui

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nizartuanku/topolight/internal/auth"
	"github.com/nizartuanku/topolight/internal/integ"
	"github.com/nizartuanku/topolight/internal/model"
)

// ---- integrations (controller / cloud APIs) --------------------------------------------

func (s *Server) listIntegrations(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	all := s.d.Store.Integrations()
	out := make([]model.Integration, 0, len(all))
	for _, i := range all {
		out = append(out, i.Redacted())
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	writeJSON(w, 200, map[string]any{"integrations": out, "kinds": integ.Kinds})
}

// putIntegration creates (POST) or updates (PUT /{id}); blank secrets keep the stored ones.
func (s *Server) putIntegration(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var in model.Integration
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad json")
		return
	}
	in.Kind, in.Name, in.URL, in.Site = strings.TrimSpace(in.Kind), strings.TrimSpace(in.Name), strings.TrimRight(strings.TrimSpace(in.URL), "/"), strings.TrimSpace(in.Site)
	in.Username, in.Password, in.APIKey = strings.TrimSpace(in.Username), strings.TrimSpace(in.Password), strings.TrimSpace(in.APIKey)
	valid := false
	for _, k := range integ.Kinds {
		if k == in.Kind {
			valid = true
		}
	}
	if !valid {
		fail(w, 400, "kind must be one of "+strings.Join(integ.Kinds, ", "))
		return
	}
	if in.Kind == "unifi" && in.URL == "" {
		fail(w, 400, "the controller URL is required (https://controller:8443)")
		return
	}
	if in.Every < 30 {
		in.Every = 60
	}
	if in.SiteID != "" {
		if _, err := s.d.Store.Site(in.SiteID); err != nil {
			in.SiteID = ""
		}
	}
	if id := r.PathValue("id"); id != "" {
		old, err := s.d.Store.Integration(id)
		if err != nil {
			fail(w, 404, "integration not found")
			return
		}
		in.ID, in.Created, in.LastRun, in.LastErr, in.Devices, in.Clients = old.ID, old.Created, old.LastRun, old.LastErr, old.Devices, old.Clients
		if in.Password == "" || in.Password == "••••" {
			in.Password = old.Password
		}
		if in.APIKey == "" || in.APIKey == "••••" {
			in.APIKey = old.APIKey
		}
	} else {
		in.ID, in.Created, in.Enabled = model.NewID("int"), time.Now(), true
	}
	if in.Name == "" {
		in.Name = in.Kind
	}
	if in.Kind == "meraki" && in.APIKey == "" {
		fail(w, 400, "a Meraki API key is required")
		return
	}
	if in.Kind == "unifi" && (in.Username == "" || in.Password == "") {
		fail(w, 400, "a UniFi username and password are required")
		return
	}
	s.d.Store.PutIntegration(in)
	if s.d.Integ != nil {
		s.d.Integ.Forget(in.ID)
		s.d.Integ.Now(in.ID)
	}
	writeJSON(w, 200, in.Redacted())
}

// testIntegration tries the saved credentials (or the posted ones) without importing.
func (s *Server) testIntegration(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	in, err := s.d.Store.Integration(r.PathValue("id"))
	if err != nil {
		fail(w, 404, "integration not found")
		return
	}
	if s.d.Integ == nil {
		fail(w, 503, "integrations are not running on this node")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	msg, err := s.d.Integ.Test(ctx, in)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": msg})
}

// wirelessOverview lists every AP / controller with its wireless state.
func (s *Server) wirelessOverview(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	type row struct {
		model.Wireless
		Name   string `json:"name"`
		Status string `json:"status"`
		Site   string `json:"site_id"`
		Vendor string `json:"vendor"`
	}
	names := map[string]model.Device{}
	for _, d := range s.d.Store.Devices() {
		names[d.ID] = d
	}
	var out []row
	clients := 0
	for id, ws := range s.d.Store.AllWireless() {
		d, ok := names[id]
		if !ok {
			continue
		}
		clients += ws.Clients
		out = append(out, row{Wireless: ws, Name: d.Name, Status: string(d.Status), Site: d.SiteID, Vendor: d.Vendor})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	writeJSON(w, 200, map[string]any{"aps": out, "clients": clients})
}

// sdwanOverview lists every WAN path known from SD-WAN integrations or SNMP.
func (s *Server) sdwanOverview(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	type row struct {
		model.SDWANLink
		Device string `json:"device"`
		Site   string `json:"site_id"`
	}
	names := map[string]model.Device{}
	for _, d := range s.d.Store.Devices() {
		names[d.ID] = d
	}
	var out []row
	down := 0
	for id, links := range s.d.Store.AllSDWAN() {
		d, ok := names[id]
		if !ok {
			continue
		}
		for _, l := range links {
			if !l.Up {
				down++
			}
			out = append(out, row{SDWANLink: l, Device: d.Name, Site: d.SiteID})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Device != out[b].Device {
			return out[a].Device < out[b].Device
		}
		return out[a].Name < out[b].Name
	})
	writeJSON(w, 200, map[string]any{"links": out, "down": down})
}
