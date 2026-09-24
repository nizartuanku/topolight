package webui

// AI Assist — the optional "✨ Explain" button on an alert, backed by the
// hexward-ai sidecar (github.com/nizartuanku/hexward-ai).
//
// The rules this file exists to enforce:
//
//   - TopoLight's own state engine stays the ONLY source of alerts and
//     severity. Nothing here opens, edits, re-scores, acknowledges or
//     resolves an alert; it only asks a local model to narrate one the
//     store already holds.
//   - AI Assist is off unless the operator starts the binary with
//     -ai-assist-url. Off, unreachable, slow, or answering garbage all look
//     the same to the console: {"available": false} with HTTP 200, and the
//     alert list is untouched.
//   - Only a bounded, sanitised copy of one alert leaves the process
//     (aiclient.NewFindingPacket drops secret-like evidence keys and caps
//     strings, lists and nesting). The evidence is built here from safe
//     facts only: device name/IP/vendor/model/role, interface name and
//     rates, probe type and target, rule thresholds, durations and
//     occurrence counts. SNMP communities, SSH/SNMP credentials,
//     configuration backups and raw syslog/trap streams are never read.
//   - Edition gating: the free edition talks to a sidecar without an API
//     key (same host or same Docker network). An endpoint that needs a key
//     (a dedicated AI host serving several products, or your own
//     OpenAI-compatible endpoint) is a Pro/Team capability. The check runs
//     on every request, so a licence added at runtime takes effect at once.

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nizartuanku/topolight/internal/aiclient"
	"github.com/nizartuanku/topolight/internal/auth"
	"github.com/nizartuanku/topolight/internal/license"
	"github.com/nizartuanku/topolight/internal/model"
)

// AIConfig is what cmd/ passes in from its flags.
type AIConfig struct {
	URL        string // sidecar or endpoint base URL; "" = AI Assist off
	KeyFile    string // file holding an API key; set = keyed endpoint (Pro/Team)
	Language   string // "en" (default) or "id"
	NoThinking bool   // Qwen3 enterprise profiles: skip reasoning mode
}

// AIAssist is the resolved, ready-to-use AI configuration of a Server.
type AIAssist struct {
	Client   *aiclient.Client
	Endpoint string
	Keyed    bool
	Language string
}

// NewAIAssist validates cfg and builds the client. It returns (nil, nil)
// when cfg.URL is empty — AI Assist off is the default, not an error. It
// never dials: an absent sidecar is discovered per request and degrades
// quietly.
func NewAIAssist(cfg AIConfig) (*AIAssist, error) {
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("ai-assist-url must be an http(s) base URL such as http://127.0.0.1:8435, got %q", raw)
	}
	opts := []aiclient.Option{
		// CPU inference of a 3-8B model takes tens of seconds; measured
		// 13-53 s per explanation on a CPU-only VM (hexward-ai docs/TIERS.md).
		aiclient.WithTimeout(120 * time.Second),
		// No retries: a timed-out narration retried is another two minutes
		// of a person waiting, not a better answer.
		aiclient.WithMaxRetries(0),
		aiclient.WithMaxTokens(300),
	}
	a := &AIAssist{Endpoint: strings.TrimRight(raw, "/"), Language: aiclient.NormalizeLanguage(cfg.Language)}
	if cfg.KeyFile != "" {
		b, err := os.ReadFile(cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("read ai-assist-key-file: %w", err)
		}
		key := strings.TrimSpace(string(b))
		if key == "" {
			return nil, fmt.Errorf("ai-assist-key-file %s is empty", cfg.KeyFile)
		}
		opts = append(opts, aiclient.WithAPIKey(key))
		a.Keyed = true
	}
	if cfg.NoThinking {
		opts = append(opts, aiclient.WithDisableThinking())
	}
	a.Client = aiclient.New(a.Endpoint, opts...)
	return a, nil
}

func (s *Server) registerAI() {
	s.mux.HandleFunc("GET /api/ai", s.require("viewer", s.handleAIStatus))
	s.mux.HandleFunc("POST /api/alerts/{id}/explain", s.require("viewer", s.handleExplainAlert))
}

// aiAllowed reports whether AI Assist may be used right now, and if not,
// the sentence the console shows instead of the button. The licence tier
// is read on every call because Admin → Licence can change it at runtime.
func (s *Server) aiAllowed() (bool, string) {
	if s.d.AI == nil || s.d.AI.Client == nil {
		return false, "AI Assist is off. Start with -ai-assist-url to enable it."
	}
	if t := s.d.License().Tier; s.d.AI.Keyed && t != license.TierPro && t != license.TierTeam {
		return false, "A dedicated AI host or your own endpoint (API key) needs a Pro or Team licence. The free edition works with a hexward-ai sidecar on the same host."
	}
	return true, ""
}

type aiStatusResponse struct {
	Enabled  bool   `json:"enabled"`
	Reason   string `json:"reason,omitempty"`
	Language string `json:"language,omitempty"`
	Keyed    bool   `json:"keyed,omitempty"`
}

func (s *Server) handleAIStatus(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	ok, reason := s.aiAllowed()
	resp := aiStatusResponse{Enabled: ok, Reason: reason}
	if s.d.AI != nil {
		resp.Language, resp.Keyed = s.d.AI.Language, s.d.AI.Keyed
	}
	writeJSON(w, 200, resp)
}

type explainResponse struct {
	Available    bool     `json:"available"`
	Reason       string   `json:"reason,omitempty"`
	Explanation  string   `json:"explanation,omitempty"`
	WhatToVerify []string `json:"what_to_verify,omitempty"`
	Disclaimer   string   `json:"disclaimer,omitempty"`
}

// handleExplainAlert narrates one stored alert. Every failure after the
// request itself is validated answers {"available": false} with HTTP 200 —
// the console shows a quiet note and the alert is never affected.
func (s *Server) handleExplainAlert(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		fail(w, http.StatusBadRequest, "alert id is required")
		return
	}
	a, err := s.d.Store.Alert(id)
	if err != nil {
		fail(w, http.StatusNotFound, "alert not found")
		return
	}
	if ok, reason := s.aiAllowed(); !ok {
		writeJSON(w, 200, explainResponse{Available: false, Reason: reason})
		return
	}
	packet, err := aiclient.NewFindingPacket("topolight", s.d.AI.Language, s.alertFinding(a))
	if err != nil {
		writeJSON(w, 200, explainResponse{Available: false, Reason: "This alert has no title or severity to explain."})
		return
	}
	exp, err := s.d.AI.Client.Explain(r.Context(), packet)
	if err != nil {
		writeJSON(w, 200, explainResponse{Available: false, Reason: "The AI Assist sidecar did not answer. The alert is unaffected."})
		return
	}
	writeJSON(w, 200, explainResponse{
		Available:    true,
		Explanation:  exp.ExplanationText,
		WhatToVerify: exp.WhatToVerify,
		Disclaimer:   exp.Disclaimer,
	})
}

// alertFinding maps one alert onto the generic hexward finding shape. Only
// facts an operator already sees on the alert page go in: identities,
// thresholds, rates, durations, counts. It deliberately never reads
// credentials, SNMP communities, configuration backups or log lines.
func (s *Server) alertFinding(a model.Alert) aiclient.CoreFinding {
	st := s.d.Store
	ev := map[string]any{
		"rule":        a.Rule,
		"state":       string(a.State),
		"occurrences": a.Occurrences,
		"opened_at":   a.OpenedAt.UTC().Format(time.RFC3339),
	}
	now := time.Now()
	if a.State == model.AlertResolved && !a.ResolvedAt.IsZero() {
		ev["resolved_at"] = a.ResolvedAt.UTC().Format(time.RFC3339)
		ev["duration"] = a.ResolvedAt.Sub(a.OpenedAt).Round(time.Second).String()
	} else if !a.OpenedAt.IsZero() {
		ev["open_for"] = now.Sub(a.OpenedAt).Round(time.Second).String()
	}
	if a.Impact != "" {
		ev["impact"] = a.Impact
	}
	if a.Children > 0 {
		ev["downstream_alerts_folded"] = a.Children
	}
	if a.RootCause != "" {
		if p, err := st.Alert(a.RootCause); err == nil {
			ev["root_cause"] = p.Title
		}
	}
	// Alert.Evidence holds "source HH:MM:SS" markers (snmp, state, probe,
	// syslog, trap): when and from where the engine saw the condition.
	// The markers are safe; the underlying log/trap payloads are not read.
	if len(a.Evidence) > 0 {
		ev["seen"] = append([]string(nil), a.Evidence...)
	}
	// Detail is the engine's own sentence for metric/state alerts. For
	// alerts raised from a syslog or trap message it is that message, so
	// it stays out: the alert title already carries what the user sees.
	if a.Detail != "" && !fromLogOrTrap(a) {
		ev["detail"] = a.Detail
	}

	target := a.Object
	if d, err := st.Device(a.DeviceID); err == nil {
		target = d.Name
		if d.IP != "" && d.IP != d.Name {
			target = d.Name + " (" + d.IP + ")"
		}
		dev := map[string]any{"name": d.Name, "ip": d.IP, "role": string(d.Role), "status": string(d.Status)}
		if d.Vendor != "" {
			dev["vendor"] = d.Vendor
		}
		if d.Model != "" {
			dev["model"] = d.Model
		}
		if d.PingOnly {
			dev["ping_only"] = true
		}
		if !d.LastSeen.IsZero() {
			dev["last_seen"] = d.LastSeen.UTC().Format(time.RFC3339)
		}
		if len(d.Metrics) > 0 {
			m := map[string]any{}
			for k, v := range d.Metrics {
				m[k] = v
			}
			dev["metrics"] = m
		}
		ev["device"] = dev
	}
	if site, err := st.Site(a.SiteID); err == nil {
		ev["site"] = site.Name
		if a.DeviceID == "" {
			target = site.Name
		}
	}
	if a.Object != "" {
		if i, err := st.Interface(a.Object); err == nil {
			ifc := map[string]any{"name": i.Name, "kind": i.Kind, "important": i.Important, "admin_up": i.AdminUp, "oper_up": i.OperUp,
				"speed_mbps": i.SpeedMbps, "in_util_pct": i.InUtil, "out_util_pct": i.OutUtil, "in_err_rate": i.InErrRate, "out_err_rate": i.OutErrRate}
			if i.Alias != "" {
				ifc["alias"] = i.Alias
			}
			ev["interface"] = ifc
			target += " " + i.Name
		} else if p, err := st.Probe(a.Object); err == nil {
			pr := map[string]any{"name": p.Name, "type": p.Type, "target": p.Target, "every_s": p.Every, "timeout_s": p.Timeout}
			if p.Expect != "" {
				pr["expect"] = p.Expect
			}
			ev["probe"] = pr
			if a.DeviceID == "" {
				target = p.Name + " (" + p.Type + " " + p.Target + ")"
			}
		}
	}

	remediation := ""
	if r, ok := st.Rule(a.Rule); ok {
		remediation = r.Description
		rule := map[string]any{"for_cycles": r.ForCycles}
		if r.Metric != "" {
			rule["metric"] = r.Metric
		}
		if r.Enter != 0 {
			rule["enter_threshold"] = r.Enter
		}
		if r.Exit != 0 {
			rule["exit_threshold"] = r.Exit
		}
		if r.Escalate != 0 {
			rule["escalate_threshold"] = r.Escalate
		}
		if r.Runbook != "" {
			rule["runbook"] = r.Runbook
		}
		ev["rule_thresholds"] = rule
	}

	return aiclient.CoreFinding{
		Fingerprint: a.ID,
		Module:      "topolight",
		Check:       a.Rule,
		Title:       a.Title,
		Target:      strings.TrimSpace(target),
		Severity:    string(a.Severity),
		Status:      string(a.State),
		Remediation: remediation,
		Evidence:    ev,
	}
}

// fromLogOrTrap reports whether an alert was raised from a syslog or SNMP
// trap message rather than from polling or the state engine.
func fromLogOrTrap(a model.Alert) bool {
	for _, e := range a.Evidence {
		if strings.HasPrefix(e, "syslog ") || strings.HasPrefix(e, "trap ") {
			return true
		}
	}
	return false
}
