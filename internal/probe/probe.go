// Package probe runs synthetic checks from the TopoLight host — TCP connect,
// HTTP(S) request, DNS lookup, TLS handshake with certificate expiry, ICMP
// ping and traceroute — on a schedule, keeps recent results, writes latency
// to the time-series store and raises/clears events on state changes.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/icmp"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
	"github.com/nizartuanku/topolight/internal/tsdb"
)

// Types lists the supported probe types.
var Types = []string{"tcp", "http", "dns", "tls", "ping", "traceroute"}

// Runner schedules probes.
type Runner struct {
	st   *store.Store
	db   *tsdb.DB
	ping *icmp.Pinger
	// Events receives probe_failed / probe_ok / tls_expiring / tls_ok / path_changed.
	Events chan model.Event
	// FailAfter is how many consecutive failures open an alert (default 2).
	FailAfter int
	// TLSWarnDays is the default expiry warning threshold (default 14).
	TLSWarnDays int
	// Traceroute is the hop prober; nil when raw sockets are unavailable.
	Traceroute Tracer

	mu      sync.Mutex
	next    map[string]time.Time
	running map[string]bool
	last    map[string]model.ProbeResult
	hist    map[string][]model.ProbeResult // newest last, ≤ 120
	fails   map[string]int
	alerted map[string]bool
	tlsWarn map[string]bool
	paths   map[string][]Hop
	Runs    int64
}

// Tracer runs a traceroute; implemented in trace.go (Linux raw sockets).
type Tracer interface {
	Trace(ctx context.Context, ip string, maxHops int, perHop time.Duration) ([]Hop, error)
}

// Hop is one traceroute step; Addr "" means no answer.
type Hop struct {
	TTL  int     `json:"ttl"`
	Addr string  `json:"addr,omitempty"`
	Ms   float64 `json:"ms,omitempty"`
	Done bool    `json:"done,omitempty"` // reached the target
}

// New builds a runner. ping may be nil (ping/traceroute probes then fail with a clear message).
func New(st *store.Store, db *tsdb.DB, ping *icmp.Pinger) *Runner {
	return &Runner{st: st, db: db, ping: ping, Events: make(chan model.Event, 1024), FailAfter: 2, TLSWarnDays: 14,
		next: map[string]time.Time{}, running: map[string]bool{}, last: map[string]model.ProbeResult{}, hist: map[string][]model.ProbeResult{},
		fails: map[string]int{}, alerted: map[string]bool{}, tlsWarn: map[string]bool{}, paths: map[string][]Hop{}}
}

// Run schedules until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	sem := make(chan struct{}, 16)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, p := range r.st.Probes() {
				if !p.Enabled {
					continue
				}
				r.mu.Lock()
				nx, ok := r.next[p.ID]
				if !ok {
					nx = now.Add(time.Duration(len(r.next)%10) * time.Second) // spread
					r.next[p.ID] = nx
				}
				due := !now.Before(nx) && !r.running[p.ID]
				if due {
					r.running[p.ID] = true
				}
				r.mu.Unlock()
				if !due {
					continue
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				go func(p model.Probe) {
					defer func() { <-sem }()
					r.RunOnce(ctx, p)
					r.mu.Lock()
					r.running[p.ID] = false
					every := p.Every
					if every <= 0 {
						every = 60
					}
					r.next[p.ID] = time.Now().Add(time.Duration(every) * time.Second)
					r.mu.Unlock()
				}(p)
			}
		}
	}
}

// Now schedules an immediate run.
func (r *Runner) Now(id string) {
	r.mu.Lock()
	r.next[id] = time.Now()
	r.mu.Unlock()
}

// Forget drops state after deletion.
func (r *Runner) Forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range []map[string]bool{r.running, r.alerted, r.tlsWarn} {
		delete(m, id)
	}
	delete(r.next, id)
	delete(r.last, id)
	delete(r.hist, id)
	delete(r.fails, id)
	delete(r.paths, id)
}

// Last returns the latest result.
func (r *Runner) Last(id string) (model.ProbeResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, ok := r.last[id]
	return x, ok
}

// History returns recent results, newest last.
func (r *Runner) History(id string) []model.ProbeResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.ProbeResult(nil), r.hist[id]...)
}

// Path returns the last traceroute.
func (r *Runner) Path(id string) []Hop {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Hop(nil), r.paths[id]...)
}

// Summary is what the list page shows per probe.
type Summary struct {
	Last   model.ProbeResult `json:"last"`
	Fails  int               `json:"fails"`
	Uptime float64           `json:"uptime_pct"` // over the retained history
	AvgMs  float64           `json:"avg_ms"`
}

// Summaries for every probe.
func (r *Runner) Summaries() map[string]Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]Summary{}
	for id, l := range r.last {
		s := Summary{Last: l, Fails: r.fails[id]}
		ok, n := 0, 0
		var sum float64
		for _, h := range r.hist[id] {
			n++
			if h.OK {
				ok++
				sum += h.Ms
			}
		}
		if n > 0 {
			s.Uptime = float64(ok) * 100 / float64(n)
		}
		if ok > 0 {
			s.AvgMs = sum / float64(ok)
		}
		out[id] = s
	}
	return out
}

// RunOnce executes a probe and records the result.
func (r *Runner) RunOnce(ctx context.Context, p model.Probe) model.ProbeResult {
	timeout := time.Duration(p.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if p.Type == "traceroute" && timeout < 35*time.Second {
		timeout = 35 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res := model.ProbeResult{ProbeID: p.ID, TS: time.Now(), Attrs: map[string]string{}}
	start := time.Now()
	var err error
	switch p.Type {
	case "tcp":
		err = r.tcp(cctx, p, &res)
	case "http":
		err = r.http(cctx, p, &res)
	case "dns":
		err = r.dns(cctx, p, &res)
	case "tls":
		err = r.tlsCheck(cctx, p, &res)
	case "ping":
		err = r.icmp(cctx, p, &res)
	case "traceroute":
		err = r.trace(cctx, p, &res)
	default:
		err = fmt.Errorf("unknown probe type %q", p.Type)
	}
	if res.Ms == 0 {
		res.Ms = float64(time.Since(start).Microseconds()) / 1000
	}
	res.OK = err == nil
	if err != nil {
		res.Detail = err.Error()
	}
	r.record(p, res)
	return res
}

func (r *Runner) record(p model.Probe, res model.ProbeResult) {
	r.mu.Lock()
	r.Runs++
	r.last[p.ID] = res
	h := append(r.hist[p.ID], res)
	if len(h) > 120 {
		h = h[len(h)-120:]
	}
	r.hist[p.ID] = h
	if res.OK {
		r.fails[p.ID] = 0
	} else {
		r.fails[p.ID]++
	}
	fails, alerted := r.fails[p.ID], r.alerted[p.ID]
	failAfter := r.FailAfter
	if failAfter <= 0 {
		failAfter = 2
	}
	var ev *model.Event
	if !res.OK && fails >= failAfter && !alerted {
		r.alerted[p.ID] = true
		ev = &model.Event{Kind: "probe_failed", Object: p.ID, DeviceID: p.DeviceID, Source: "probe", Severity: model.SevMajor, Domain: model.DomainNetwork,
			Message: fmt.Sprintf("%s (%s %s) failing: %s", p.Name, p.Type, p.Target, res.Detail), DedupKey: "probe_failed:" + p.ID, Attrs: map[string]string{"probe": p.Name, "target": p.Target}}
	} else if res.OK && alerted {
		r.alerted[p.ID] = false
		ev = &model.Event{Kind: "probe_ok", Object: p.ID, DeviceID: p.DeviceID, Source: "probe", Severity: model.SevInfo, Domain: model.DomainNetwork,
			Message: fmt.Sprintf("%s (%s %s) answering again in %.0f ms", p.Name, p.Type, p.Target, res.Ms), DedupKey: "probe_failed:" + p.ID}
	}
	r.mu.Unlock()
	if r.db != nil {
		r.db.Append("probe_ms|"+p.ID, res.TS.Unix(), res.Ms)
		ok := 0.0
		if res.OK {
			ok = 1
		}
		r.db.Append("probe_ok|"+p.ID, res.TS.Unix(), ok)
	}
	if ev != nil {
		r.emit(*ev)
	}
}

func (r *Runner) emit(ev model.Event) {
	if ev.TS.IsZero() {
		ev.TS = time.Now()
	}
	select {
	case r.Events <- ev:
	default:
	}
}

// ---- probe types ----------------------------------------------------------------

func hostPort(target, defPort string) string {
	if _, _, err := net.SplitHostPort(target); err == nil {
		return target
	}
	return net.JoinHostPort(target, defPort)
}

func (r *Runner) tcp(ctx context.Context, p model.Probe, res *model.ProbeResult) error {
	var d net.Dialer
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", hostPort(p.Target, "80"))
	if err != nil {
		return err
	}
	res.Ms = float64(time.Since(start).Microseconds()) / 1000
	res.Attrs["remote"] = c.RemoteAddr().String()
	c.Close()
	return nil
}

func (r *Runner) http(ctx context.Context, p model.Probe, res *model.ProbeResult) error {
	target := p.Target
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "TopoLight-probe/1")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: strings.HasSuffix(p.Expect, " insecure")}, Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	res.Ms = float64(time.Since(start).Microseconds()) / 1000
	res.Attrs["status"] = strconv.Itoa(resp.StatusCode)
	res.Attrs["bytes"] = strconv.Itoa(len(body))
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		days := time.Until(resp.TLS.PeerCertificates[0].NotAfter).Hours() / 24
		res.Attrs["cert_days"] = strconv.Itoa(int(days))
	}
	expect := strings.TrimSuffix(p.Expect, " insecure")
	want := "200-399"
	var bodyWant string
	for _, part := range strings.Fields(expect) {
		if strings.HasPrefix(part, "body:") {
			bodyWant = strings.TrimPrefix(part, "body:")
		} else if part != "" {
			want = part
		}
	}
	if !statusMatches(resp.StatusCode, want) {
		return fmt.Errorf("HTTP %d (wanted %s)", resp.StatusCode, want)
	}
	if bodyWant != "" && !strings.Contains(string(body), bodyWant) {
		return fmt.Errorf("HTTP %d but body lacks %q", resp.StatusCode, bodyWant)
	}
	res.Detail = "HTTP " + strconv.Itoa(resp.StatusCode)
	return nil
}

func statusMatches(code int, want string) bool {
	for _, w := range strings.Split(want, ",") {
		w = strings.TrimSpace(w)
		if a, b, ok := strings.Cut(w, "-"); ok {
			lo, _ := strconv.Atoi(a)
			hi, _ := strconv.Atoi(b)
			if code >= lo && code <= hi {
				return true
			}
		} else if n, err := strconv.Atoi(w); err == nil && n == code {
			return true
		}
	}
	return false
}

func (r *Runner) dns(ctx context.Context, p model.Probe, res *model.ProbeResult) error {
	resolver := net.DefaultResolver
	if p.Resolver != "" {
		srv := hostPort(p.Resolver, "53")
		resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv)
		}}
	}
	start := time.Now()
	ips, err := resolver.LookupHost(ctx, p.Target)
	if err != nil {
		return err
	}
	res.Ms = float64(time.Since(start).Microseconds()) / 1000
	sort.Strings(ips)
	res.Attrs["answers"] = strings.Join(ips, ", ")
	res.Detail = strings.Join(ips, ", ")
	if p.Expect != "" {
		found := false
		for _, ip := range ips {
			if strings.HasPrefix(ip, p.Expect) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("resolved %s, expected %s", strings.Join(ips, ", "), p.Expect)
		}
	}
	return nil
}

func (r *Runner) tlsCheck(ctx context.Context, p model.Probe, res *model.ProbeResult) error {
	addr := hostPort(p.Target, "443")
	host, _, _ := net.SplitHostPort(addr)
	d := &tls.Dialer{Config: &tls.Config{ServerName: host, InsecureSkipVerify: true}}
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	res.Ms = float64(time.Since(start).Microseconds()) / 1000
	cs := c.(*tls.Conn).ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		return errors.New("no certificate presented")
	}
	leaf := cs.PeerCertificates[0]
	days := time.Until(leaf.NotAfter).Hours() / 24
	res.Attrs["cert_days"] = strconv.Itoa(int(days))
	res.Attrs["subject"] = leaf.Subject.CommonName
	res.Attrs["issuer"] = leaf.Issuer.CommonName
	res.Attrs["not_after"] = leaf.NotAfter.Format("2006-01-02")
	res.Attrs["version"] = tls.VersionName(cs.Version)
	// chain verification against the system roots — reported, not fatal
	pool := x509.NewCertPool()
	for _, ic := range cs.PeerCertificates[1:] {
		pool.AddCert(ic)
	}
	if _, verr := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: pool}); verr != nil {
		res.Attrs["chain"] = verr.Error()
	} else {
		res.Attrs["chain"] = "valid"
	}
	minDays := r.TLSWarnDays
	if n, err := strconv.Atoi(strings.TrimSpace(p.Expect)); err == nil && n > 0 {
		minDays = n
	}
	res.Detail = fmt.Sprintf("%s expires %s (%d days)", leaf.Subject.CommonName, leaf.NotAfter.Format("2006-01-02"), int(days))
	warn := days < float64(minDays)
	r.mu.Lock()
	was := r.tlsWarn[p.ID]
	r.tlsWarn[p.ID] = warn
	r.mu.Unlock()
	if warn && !was {
		r.emit(model.Event{Kind: "tls_expiring", Object: p.ID, DeviceID: p.DeviceID, Source: "probe", Severity: model.SevMinor, Domain: model.DomainNetwork,
			Message: fmt.Sprintf("%s: certificate for %s expires in %d days (%s)", p.Name, leaf.Subject.CommonName, int(days), leaf.NotAfter.Format("2006-01-02")), DedupKey: "tls_expiring:" + p.ID})
	} else if !warn && was {
		r.emit(model.Event{Kind: "tls_ok", Object: p.ID, DeviceID: p.DeviceID, Source: "probe", Severity: model.SevInfo, Domain: model.DomainNetwork,
			Message: fmt.Sprintf("%s: certificate renewed, %d days left", p.Name, int(days)), DedupKey: "tls_expiring:" + p.ID})
	}
	if days < 0 {
		return fmt.Errorf("certificate expired on %s", leaf.NotAfter.Format("2006-01-02"))
	}
	return nil
}

func resolveIPv4(ctx context.Context, host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return "", errors.New("IPv6 targets are not supported for ICMP probes yet")
		}
		return ip.String(), nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", errors.New("no IPv4 address")
	}
	return ips[0].String(), nil
}

func (r *Runner) icmp(ctx context.Context, p model.Probe, res *model.ProbeResult) error {
	if r.ping == nil {
		return errors.New("ICMP is unavailable on this host (see Admin → System)")
	}
	ip, err := resolveIPv4(ctx, p.Target)
	if err != nil {
		return err
	}
	pr, err := r.ping.Probe(ip, 5, 100*time.Millisecond, 2*time.Second)
	if err != nil {
		return err
	}
	res.Attrs["ip"] = ip
	res.Attrs["loss_pct"] = strconv.FormatFloat(pr.LossPct, 'f', 0, 64)
	res.Attrs["jitter_ms"] = strconv.FormatFloat(float64(pr.Jitter.Microseconds())/1000, 'f', 2, 64)
	if !pr.Reachable() {
		return errors.New("no reply to 5 echo requests")
	}
	res.Ms = float64(pr.AvgRTT.Microseconds()) / 1000
	res.Detail = fmt.Sprintf("%.1f ms, %.0f%% loss", res.Ms, pr.LossPct)
	if pr.LossPct >= 60 {
		return fmt.Errorf("%.0f%% packet loss", pr.LossPct)
	}
	return nil
}

func (r *Runner) trace(ctx context.Context, p model.Probe, res *model.ProbeResult) error {
	if r.Traceroute == nil {
		return errors.New("traceroute needs a raw ICMP socket (CAP_NET_RAW or root)")
	}
	ip, err := resolveIPv4(ctx, p.Target)
	if err != nil {
		return err
	}
	hops, err := r.Traceroute.Trace(ctx, ip, 30, time.Second)
	if err != nil {
		return err
	}
	reached := false
	last := 0
	for _, h := range hops {
		if h.Addr != "" {
			last = h.TTL
		}
		if h.Done {
			reached = true
			res.Ms = h.Ms
		}
	}
	r.mu.Lock()
	prev := r.paths[p.ID]
	r.paths[p.ID] = hops
	r.mu.Unlock()
	res.Attrs["hops"] = strconv.Itoa(last)
	res.Attrs["ip"] = ip
	res.Detail = fmt.Sprintf("%d hops", last)
	if len(prev) > 0 && pathKey(prev) != pathKey(hops) {
		r.emit(model.Event{Kind: "path_changed", Object: p.ID, DeviceID: p.DeviceID, Source: "probe", Severity: model.SevInfo, Domain: model.DomainNetwork,
			Message: fmt.Sprintf("%s: path to %s changed — now %s", p.Name, p.Target, pathText(hops)), DedupKey: "path_changed:" + p.ID})
	}
	if !reached {
		return fmt.Errorf("target not reached within 30 hops (last answer at hop %d)", last)
	}
	return nil
}

func pathKey(h []Hop) string {
	var b strings.Builder
	for _, x := range h {
		if x.Addr != "" {
			b.WriteString(x.Addr)
			b.WriteByte('>')
		}
	}
	return b.String()
}

func pathText(h []Hop) string {
	var parts []string
	for _, x := range h {
		if x.Addr != "" {
			parts = append(parts, x.Addr)
		} else {
			parts = append(parts, "*")
		}
	}
	return strings.Join(parts, " → ")
}
