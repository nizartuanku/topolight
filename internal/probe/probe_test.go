package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
)

func newRunner(t *testing.T) *Runner {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(st, nil, nil)
}

func TestTCPAndHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("hello topolight"))
	}))
	defer srv.Close()
	r := newRunner(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	res := r.RunOnce(context.Background(), model.Probe{ID: "p1", Type: "tcp", Target: host})
	if !res.OK || res.Ms <= 0 {
		t.Fatalf("tcp: %+v", res)
	}
	res = r.RunOnce(context.Background(), model.Probe{ID: "p2", Type: "http", Target: srv.URL, Expect: "200 body:topolight"})
	if !res.OK || res.Attrs["status"] != "200" {
		t.Fatalf("http: %+v", res)
	}
	res = r.RunOnce(context.Background(), model.Probe{ID: "p3", Type: "http", Target: srv.URL + "/down"})
	if res.OK || !strings.Contains(res.Detail, "503") {
		t.Fatalf("http down: %+v", res)
	}
	res = r.RunOnce(context.Background(), model.Probe{ID: "p4", Type: "http", Target: srv.URL, Expect: "body:missing-text"})
	if res.OK {
		t.Fatalf("body check: %+v", res)
	}
	// closed port fails, and after FailAfter failures an event is emitted, then cleared
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	p := model.Probe{ID: "p5", Name: "closed", Type: "tcp", Target: closed, Timeout: 1}
	r.RunOnce(context.Background(), p)
	r.RunOnce(context.Background(), p)
	select {
	case ev := <-r.Events:
		if ev.Kind != "probe_failed" || ev.DedupKey != "probe_failed:p5" {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no probe_failed event")
	}
	p.Target = host
	r.RunOnce(context.Background(), p)
	select {
	case ev := <-r.Events:
		if ev.Kind != "probe_ok" {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no probe_ok event")
	}
	s := r.Summaries()["p5"]
	if s.Fails != 0 || s.Uptime < 30 || s.Uptime > 40 {
		t.Fatalf("summary: %+v", s)
	}
}

func TestTLSExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	r := newRunner(t)
	host := strings.TrimPrefix(srv.URL, "https://")
	res := r.RunOnce(context.Background(), model.Probe{ID: "t1", Name: "tls", Type: "tls", Target: host, Expect: "30000"})
	if res.Attrs["cert_days"] == "" || res.Attrs["not_after"] == "" {
		t.Fatalf("tls: %+v", res)
	}
	// httptest certs are valid for decades, so a 30000-day threshold warns
	select {
	case ev := <-r.Events:
		if ev.Kind != "tls_expiring" {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no tls_expiring event")
	}
	// https probe also reports certificate days
	res = r.RunOnce(context.Background(), model.Probe{ID: "t2", Type: "http", Target: srv.URL, Expect: "200 insecure"})
	if !res.OK || res.Attrs["cert_days"] == "" {
		t.Fatalf("https: %+v", res)
	}
}

func TestDNS(t *testing.T) {
	r := newRunner(t)
	res := r.RunOnce(context.Background(), model.Probe{ID: "d1", Type: "dns", Target: "localhost", Expect: "127.0.0.1"})
	if !res.OK {
		t.Fatalf("dns: %+v", res)
	}
	res = r.RunOnce(context.Background(), model.Probe{ID: "d2", Type: "dns", Target: "localhost", Expect: "10.9."})
	if res.OK {
		t.Fatalf("dns expect: %+v", res)
	}
}

func TestStatusMatches(t *testing.T) {
	if !statusMatches(204, "200-299") || statusMatches(404, "200-399") || !statusMatches(404, "200,404") {
		t.Fatal("status ranges")
	}
}
