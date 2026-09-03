package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// StandbyDeps is what a standby/collector console needs.
type StandbyDeps struct {
	Leader func() (console string, ok bool)
	Status func() any
}

// StandbyHandler proxies the whole console to the leader and answers
// /cluster-status locally, so an operator who opens any node lands on the
// working console without knowing which node leads.
func StandbyHandler(d StandbyDeps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cluster-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(d.Status())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, ok := d.Leader()
		if !ok {
			http.Error(w, "standby: no leader", 503)
			return
		}
		w.Write([]byte("ok standby\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		console, ok := d.Leader()
		target, err := url.Parse(console)
		if !ok || err != nil || target.Host == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(503)
			w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>TopoLight standby</title><style>body{font:15px/1.5 system-ui;margin:60px auto;max-width:560px;color:#111}code{background:#eef1f5;padding:1px 5px;border-radius:4px}</style><h1>This node is a standby</h1><p>No leader is reachable right now. If the leader is down for good and this cluster has fewer than three full nodes, promote this node from its command line: <code>topolight -promote</code> (or wait — with three or more full nodes a new leader is elected automatically within about 20 seconds).</p><p><a href="/cluster-status">Cluster status (JSON)</a></p>`))
			return
		}
		p := httputil.NewSingleHostReverseProxy(target)
		p.FlushInterval = 200 * time.Millisecond // SSE
		orig := p.Director
		p.Director = func(req *http.Request) {
			orig(req)
			req.Host = target.Host
			req.Header.Set("X-TopoLight-Via", "standby")
		}
		p.ModifyResponse = func(resp *http.Response) error {
			resp.Header.Set("X-TopoLight-Node", "standby")
			// cookies set by the leader must stay valid on this host name: drop the Domain attribute
			for i, c := range resp.Header.Values("Set-Cookie") {
				resp.Header["Set-Cookie"][i] = strings.ReplaceAll(c, "; Domain="+target.Hostname(), "")
			}
			return nil
		}
		p.ServeHTTP(w, r)
	})
	return mux
}
