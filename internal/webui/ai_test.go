package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nizartuanku/topolight/internal/aiclient"
	"github.com/nizartuanku/topolight/internal/license"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
)

// fakeSidecar answers like a grammar-constrained hexward-ai sidecar and
// records the last request it received.
type fakeSidecar struct {
	srv      *httptest.Server
	calls    atomic.Int32
	lastBody atomic.Value // string
	lastAuth atomic.Value // string
	status   int
}

func newFakeSidecar(t *testing.T) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		b := new(bytes.Buffer)
		b.ReadFrom(r.Body)
		f.lastBody.Store(b.String())
		f.lastAuth.Store(r.Header.Get("Authorization"))
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			return
		}
		content, _ := json.Marshal(map[string]any{
			"explanation":    "The core switch stopped answering ICMP and SNMP.",
			"what_to_verify": []string{"Confirm the device is powered and its uplink is up."},
			"disclaimer":     "model-authored text that must be overwritten",
		})
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": string(content)}}},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// aiTestServer builds a console with one site, one device, one rule and
// one open alert, signed in as a viewer, at the given licence tier. The
// tier is a pointer so a test can change it while the server runs.
type aiTestEnv struct {
	s      *Server
	api    *httptest.Server
	cookie *http.Cookie
	tier   *license.Tier
}

func aiTestServer(t *testing.T, ai *AIAssist, tier license.Tier) *aiTestEnv {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	st.PutSite(model.Site{ID: "site1", Name: "HQ"})
	st.PutDevice(model.Device{ID: "dev1", SiteID: "site1", Name: "core-sw1", IP: "10.0.0.1", Role: model.RoleCore, Status: model.StatusDown, Vendor: "Cisco",
		Metrics: map[string]float64{"rtt_ms": 0, "loss_pct": 100}})
	st.PutRule(model.Rule{ID: "device_down", Object: "device", ForCycles: 3, Severity: model.SevMajor, Enabled: true,
		Description: "Device does not answer ICMP (and SNMP) for 3 consecutive cycles.", Runbook: "device-down"})
	st.PutAlert(model.Alert{ID: "al1", Rule: "device_down", Severity: model.SevMajor, State: model.AlertOpen, SiteID: "site1", DeviceID: "dev1",
		Title: "Device down: core-sw1", Detail: "core-sw1 (10.0.0.1) stopped answering. Last seen 4m ago.",
		OpenedAt: time.Now().Add(-4 * time.Minute), UpdatedAt: time.Now(), Occurrences: 1,
		Evidence: []string{"state 07:31:06", "syslog 07:31:07"}, DedupKey: "device_down:dev1"})
	st.PutUser(model.User{ID: "u1", Name: "viewer", Role: "viewer", Created: time.Now()})
	cur := tier
	env := &aiTestEnv{tier: &cur}
	env.s = New(Deps{Store: st, AI: ai, Started: time.Now(), License: func() license.State {
		return license.State{Tier: *env.tier, Caps: license.CapsFor(*env.tier)}
	}})
	sess, err := env.s.sessions.Create("u1", "viewer", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	env.cookie = &http.Cookie{Name: "topolight_session", Value: sess.Token}
	env.api = httptest.NewServer(env.s.Handler())
	t.Cleanup(env.api.Close)
	return env
}

func (e *aiTestEnv) do(t *testing.T, method, path string, signedIn bool) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.api.URL+path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if signedIn {
		req.AddCookie(e.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := new(bytes.Buffer)
	b.ReadFrom(resp.Body)
	return resp.StatusCode, b.Bytes()
}

func (e *aiTestEnv) explain(t *testing.T, id string) (int, explainResponse) {
	t.Helper()
	code, body := e.do(t, "POST", "/api/alerts/"+id+"/explain", true)
	var out explainResponse
	json.Unmarshal(body, &out)
	return code, out
}

func (e *aiTestEnv) status(t *testing.T) aiStatusResponse {
	t.Helper()
	_, body := e.do(t, "GET", "/api/ai", true)
	var st aiStatusResponse
	json.Unmarshal(body, &st)
	return st
}

func TestAI_OffByDefault(t *testing.T) {
	ai, err := NewAIAssist(AIConfig{})
	if err != nil || ai != nil {
		t.Fatalf("empty URL must mean AI off, got %v, %v", ai, err)
	}
	env := aiTestServer(t, nil, license.TierFree)
	code, out := env.explain(t, "al1")
	if code != http.StatusOK || out.Available || out.Reason == "" {
		t.Fatalf("AI off: want 200 available=false with a reason, got %d %+v", code, out)
	}
	if st := env.status(t); st.Enabled {
		t.Error("/api/ai must report enabled=false when AI Assist is off")
	}
}

func TestAI_RequiresSignIn(t *testing.T) {
	side := newFakeSidecar(t)
	ai, _ := NewAIAssist(AIConfig{URL: side.srv.URL})
	env := aiTestServer(t, ai, license.TierFree)
	if code, _ := env.do(t, "GET", "/api/ai", false); code != http.StatusUnauthorized {
		t.Errorf("GET /api/ai without a session: got %d, want 401", code)
	}
	if code, _ := env.do(t, "POST", "/api/alerts/al1/explain", false); code != http.StatusUnauthorized {
		t.Errorf("explain without a session: got %d, want 401", code)
	}
	if side.calls.Load() != 0 {
		t.Error("anonymous requests must never reach the sidecar")
	}
}

func TestAI_RejectsBadURLAndEmptyKey(t *testing.T) {
	if _, err := NewAIAssist(AIConfig{URL: "127.0.0.1:8435"}); err == nil {
		t.Error("URL without scheme must be rejected")
	}
	empty := filepath.Join(t.TempDir(), "k")
	os.WriteFile(empty, []byte("  \n"), 0o600)
	if _, err := NewAIAssist(AIConfig{URL: "http://127.0.0.1:8435", KeyFile: empty}); err == nil {
		t.Error("empty key file must be rejected")
	}
	if _, err := NewAIAssist(AIConfig{URL: "http://127.0.0.1:8435", KeyFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("missing key file must be rejected")
	}
}

func TestAI_ExplainHappyPathSanitisesEvidence(t *testing.T) {
	side := newFakeSidecar(t)
	ai, err := NewAIAssist(AIConfig{URL: side.srv.URL, Language: "id"})
	if err != nil {
		t.Fatal(err)
	}
	env := aiTestServer(t, ai, license.TierFree)
	if st := env.status(t); !st.Enabled || st.Language != "id" || st.Keyed {
		t.Fatalf("/api/ai = %+v, want enabled, language id, unkeyed", st)
	}
	code, out := env.explain(t, "al1")
	if code != http.StatusOK || !out.Available {
		t.Fatalf("want available explanation, got %d %+v", code, out)
	}
	if out.Disclaimer != "AI-generated summary — verify against raw findings" {
		t.Errorf("disclaimer not canonical: %q", out.Disclaimer)
	}
	if out.Explanation == "" || len(out.WhatToVerify) != 1 {
		t.Errorf("explanation not passed through: %+v", out)
	}
	body, _ := side.lastBody.Load().(string)
	for _, want := range []string{"hexward.explain_finding", "Device down: core-sw1", "device_down", "core-sw1 (10.0.0.1)", "for_cycles", "Bahasa Indonesia", "3 consecutive cycles"} {
		if !strings.Contains(body, want) {
			t.Errorf("request to sidecar lacks %q", want)
		}
	}
	// The alert carries a syslog evidence marker, so its Detail (which for
	// log-derived alerts is the raw message) must stay out of the packet.
	if strings.Contains(body, "stopped answering. Last seen") {
		t.Error("detail of a log/trap-derived alert reached the sidecar")
	}
}

func TestAI_PacketNeverCarriesSecrets(t *testing.T) {
	// Even if a future alert stuffed a secret-looking key into the
	// evidence, aiclient's sanitiser must drop it before it leaves.
	side := newFakeSidecar(t)
	ai, _ := NewAIAssist(AIConfig{URL: side.srv.URL})
	env := aiTestServer(t, ai, license.TierFree)
	// A credential and a config backup exist in the store; neither is
	// read by alertFinding.
	env.s.d.Store.PutCred(model.Credential{ID: "c1", Name: "snmp", Community: "SECRET-COMMUNITY-DO-NOT-SEND"})
	env.s.d.Store.UpdateDevice("dev1", func(d *model.Device) { d.CredID = "c1"; d.SysDescr = "Cisco IOS SECRET-SYSDESCR" })
	f := env.s.alertFinding(mustAlert(t, env.s, "al1"))
	// aiclient's marker list has no "community": the evidence must simply
	// never contain such a key, at any nesting level.
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "community") || strings.Contains(lk, "cred") || strings.Contains(lk, "descr") || strings.Contains(lk, "config") {
				t.Errorf("evidence key %s%s must not exist", prefix, k)
			}
			if sub, ok := v.(map[string]any); ok {
				walk(prefix+k+".", sub)
			}
		}
	}
	walk("", f.Evidence)
	f.Evidence["api_token"] = "SECRET-KEY-DO-NOT-SEND"
	f.Evidence["password"] = "SECRET-PW-DO-NOT-SEND"
	pk, err := aiclient.NewFindingPacket("topolight", "en", f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ai.Client.Explain(t.Context(), pk); err != nil {
		t.Fatal(err)
	}
	body, _ := side.lastBody.Load().(string)
	for _, secret := range []string{"SECRET-COMMUNITY", "SECRET-SYSDESCR", "SECRET-KEY", "SECRET-PW", "api_token", "password"} {
		if strings.Contains(body, secret) {
			t.Errorf("%q reached the sidecar", secret)
		}
	}
}

func TestAI_KeyedEndpointNeedsPaidTier(t *testing.T) {
	side := newFakeSidecar(t)
	kf := filepath.Join(t.TempDir(), "ai_api_key")
	os.WriteFile(kf, []byte("0123456789abcdef0123456789abcdef\n"), 0o600)
	ai, err := NewAIAssist(AIConfig{URL: side.srv.URL, KeyFile: kf})
	if err != nil {
		t.Fatal(err)
	}
	env := aiTestServer(t, ai, license.TierFree)
	if st := env.status(t); st.Enabled || !st.Keyed || !strings.Contains(st.Reason, "Pro or Team") {
		t.Fatalf("free tier + keyed endpoint: /api/ai = %+v", st)
	}
	code, out := env.explain(t, "al1")
	if code != http.StatusOK || out.Available || !strings.Contains(out.Reason, "Pro or Team") {
		t.Fatalf("free tier + keyed endpoint must be refused with a reason, got %d %+v", code, out)
	}
	if side.calls.Load() != 0 {
		t.Fatal("free tier must not send anything to a keyed endpoint")
	}

	// The licence is read per request: an upgrade at runtime takes effect at once.
	for _, tier := range []license.Tier{license.TierPro, license.TierTeam} {
		*env.tier = tier
		if _, out := env.explain(t, "al1"); !out.Available {
			t.Fatalf("%s tier + keyed endpoint must work, got %+v", tier, out)
		}
		if auth, _ := side.lastAuth.Load().(string); auth != "Bearer 0123456789abcdef0123456789abcdef" {
			t.Errorf("Authorization header = %q", auth)
		}
	}
	*env.tier = license.TierFree
	if _, out := env.explain(t, "al1"); out.Available {
		t.Fatal("dropping back to free must refuse the keyed endpoint again")
	}
}

func TestAI_SidecarDownDegradesQuietly(t *testing.T) {
	side := newFakeSidecar(t)
	side.status = http.StatusServiceUnavailable
	ai, _ := NewAIAssist(AIConfig{URL: side.srv.URL})
	env := aiTestServer(t, ai, license.TierFree)
	code, out := env.explain(t, "al1")
	if code != http.StatusOK || out.Available || out.Reason == "" {
		t.Fatalf("sidecar 503 must give 200 available=false with a reason, got %d %+v", code, out)
	}
	a := mustAlert(t, env.s, "al1")
	if a.State != model.AlertOpen || a.Severity != model.SevMajor || a.Occurrences != 1 || len(a.Evidence) != 2 {
		t.Fatalf("alert must be untouched after an AI failure, got %+v", a)
	}
}

func TestAI_BadRequests(t *testing.T) {
	side := newFakeSidecar(t)
	ai, _ := NewAIAssist(AIConfig{URL: side.srv.URL})
	env := aiTestServer(t, ai, license.TierFree)
	if code, _ := env.explain(t, "nope"); code != http.StatusNotFound {
		t.Errorf("unknown alert: got %d, want 404", code)
	}
	if code, _ := env.do(t, "POST", "/api/alerts//explain", true); code != http.StatusNotFound && code != http.StatusBadRequest {
		t.Errorf("empty alert id: got %d, want 404 or 400", code)
	}
	if code, _ := env.do(t, "GET", "/api/alerts/al1/explain", true); code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
		t.Errorf("GET on explain: got %d, want 405 or 404", code)
	}
	if side.calls.Load() != 0 {
		t.Error("invalid requests must never reach the sidecar")
	}
}

func TestAI_AlertFindingShape(t *testing.T) {
	env := aiTestServer(t, nil, license.TierFree)
	st := env.s.d.Store
	st.PutInterfaces("dev1", []model.Interface{{ID: "dev1:1", DeviceID: "dev1", Index: 1, Name: "Te1/1", Alias: "uplink to core-sw2", Kind: "phys", Important: true, AdminUp: true, OperUp: false, SpeedMbps: 10000}})
	st.PutAlert(model.Alert{ID: "al2", Rule: "interface_down", Severity: model.SevMajor, State: model.AlertOpen, SiteID: "site1", DeviceID: "dev1", Object: "dev1:1",
		Title: "Interface down: core-sw1 Te1/1", Detail: "uplink to core-sw2 · 10000 Mbps", OpenedAt: time.Now(), Evidence: []string{"state 08:00:00"}})
	st.PutProbe(model.Probe{ID: "pr1", Name: "intranet", Type: "http", Target: "http://127.0.0.1:9/", Every: 60, Timeout: 5, Expect: "200-299", Enabled: true})
	st.PutAlert(model.Alert{ID: "al3", Rule: "probe_failed", Severity: model.SevMajor, State: model.AlertResolved, Object: "pr1",
		Title: "intranet (http http://127.0.0.1:9/) failing: connection refused", OpenedAt: time.Now().Add(-time.Hour), ResolvedAt: time.Now(), Evidence: []string{"probe 08:00:00"}})

	f := env.s.alertFinding(mustAlert(t, env.s, "al2"))
	if f.Check != "interface_down" || f.Target != "core-sw1 (10.0.0.1) Te1/1" || f.Severity != "major" || f.Status != "open" {
		t.Fatalf("interface alert: %+v", f)
	}
	ifc, _ := f.Evidence["interface"].(map[string]any)
	if ifc["name"] != "Te1/1" || ifc["important"] != true || ifc["oper_up"] != false || f.Evidence["detail"] != "uplink to core-sw2 · 10000 Mbps" {
		t.Fatalf("interface evidence: %+v", f.Evidence)
	}
	if f.Remediation != "" {
		t.Fatalf("no interface_down rule seeded, remediation must be empty, got %q", f.Remediation)
	}

	f = env.s.alertFinding(mustAlert(t, env.s, "al3"))
	if f.Target != "intranet (http http://127.0.0.1:9/)" || f.Status != "resolved" || f.Evidence["duration"] == nil || f.Evidence["open_for"] != nil {
		t.Fatalf("probe alert: %+v", f)
	}
	pr, _ := f.Evidence["probe"].(map[string]any)
	if pr["type"] != "http" || pr["expect"] != "200-299" {
		t.Fatalf("probe evidence: %+v", pr)
	}
	// device_down alert: rule description is TopoLight's own hint text
	f = env.s.alertFinding(mustAlert(t, env.s, "al1"))
	if !strings.Contains(f.Remediation, "3 consecutive cycles") || f.Evidence["detail"] != nil || f.Evidence["site"] != "HQ" {
		t.Fatalf("device alert: remediation=%q evidence=%+v", f.Remediation, f.Evidence)
	}
}

func mustAlert(t *testing.T, s *Server, id string) model.Alert {
	t.Helper()
	a, err := s.d.Store.Alert(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
