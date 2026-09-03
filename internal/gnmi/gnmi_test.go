package gnmi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPathRoundTrip(t *testing.T) {
	cases := []string{
		"/interfaces/interface[name=Ethernet1]/state/counters",
		"/system/state/hostname",
		"/a/b[k1=v1][k2=v 2]/c",
		"/network-instances/network-instance[name=default]/protocols/protocol[identifier=BGP][name=BGP]/bgp",
	}
	for _, c := range cases {
		origin, elems := parsePath(c)
		if origin != "" {
			t.Fatalf("%s: unexpected origin %q", c, origin)
		}
		back, err := decodePath(encodePath(origin, elems).b)
		if err != nil || back != c {
			t.Fatalf("%s -> %s (%v)", c, back, err)
		}
	}
	origin, elems := parsePath("openconfig:/system/state")
	if origin != "openconfig" || len(elems) != 2 {
		t.Fatalf("origin parse: %q %v", origin, elems)
	}
	_, elems = parsePath("/x[name=a\\]b]/y")
	if elems[0].keys[0][1] != "a]b" {
		t.Fatalf("escaped key: %v", elems[0].keys)
	}
}

func TestTreeMergesBlobsAndLeaves(t *testing.T) {
	ups := []Update{
		{Path: "/interfaces", Val: json.RawMessage(`{"openconfig-interfaces:interfaces":{"interface":[{"name":"eth0","state":{"oper-status":"UP","counters":{"in-octets":"12345678901234"}}}]}}`)},
		{Path: "/interfaces/interface[name=eth0]/state/counters/out-octets", Val: uint64(99)},
		{Path: "/interfaces/interface[name=eth1]/state/oper-status", Val: "DOWN"},
		{Path: "/system/state/hostname", Val: "sw1"},
	}
	tr := Tree(ups)
	if Str(Lookup(tr, "/system/state/hostname")) != "sw1" {
		t.Fatal("hostname")
	}
	if Uint(Lookup(tr, "/interfaces/interface[name=eth0]/state/counters/in-octets")) != 12345678901234 {
		t.Fatalf("in-octets: %v", Lookup(tr, "/interfaces/interface[name=eth0]/state/counters/in-octets"))
	}
	if Uint(Lookup(tr, "/interfaces/interface[name=eth0]/state/counters/out-octets")) != 99 {
		t.Fatal("leaf merged into blob entry")
	}
	if Str(Lookup(tr, "/interfaces/interface[name=eth1]/state/oper-status")) != "DOWN" {
		t.Fatal("second list entry from leaf path")
	}
	if lst, _ := Lookup(tr, "/interfaces/interface").([]any); len(lst) != 2 {
		t.Fatalf("want 2 interfaces, got %d", len(lst))
	}
}

// A tiny gRPC-over-HTTP/2 server built from the same codec exercises framing,
// trailers, metadata and error mapping without a gRPC dependency.
func TestGetAgainstFakeServer(t *testing.T) {
	h := http.NewServeMux()
	h.HandleFunc("/gnmi.gNMI/Capabilities", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("username") != "u" || r.Header.Get("password") != "p" {
			w.Header().Set("Content-Type", "application/grpc")
			w.Header().Set("grpc-status", "16")
			w.Header().Set("grpc-message", "bad%20credentials")
			w.WriteHeader(200)
			return
		}
		e := &enc{}
		m := &enc{}
		m.str(1, "openconfig-interfaces")
		m.str(2, "OpenConfig working group")
		e.msg(1, m)
		e.uint(2, 4)
		e.str(3, "0.10.0")
		writeFrame(w, e.b)
	})
	h.HandleFunc("/gnmi.gNMI/Get", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fs, err := decode(body[5:])
		if err != nil {
			t.Errorf("decode request: %v", err)
		}
		var paths int
		var encoding uint64
		for _, f := range fs {
			if f.num == 2 {
				paths++
			}
			if f.num == 5 {
				encoding = f.u
			}
		}
		if paths != 2 || encoding != EncJSONIETF {
			t.Errorf("request: %d paths, encoding %d", paths, encoding)
		}
		// GetResponse{ notification{ timestamp, update{ path, val{json_ietf} }, update{ path leaf, val{uint} } } }
		n := &enc{}
		n.uint(1, uint64(time.Now().UnixNano()))
		u1 := &enc{}
		_, el := parsePath("/system/state")
		u1.msg(1, encodePath("", el))
		tv := &enc{}
		tv.bytes(11, []byte(`{"openconfig-system:hostname":"r1","boot-time":"1700000000000000000"}`))
		u1.msg(3, tv)
		n.msg(4, u1)
		u2 := &enc{}
		_, el2 := parsePath("/interfaces/interface[name=e1]/state/counters/in-octets")
		u2.msg(1, encodePath("", el2))
		tv2 := &enc{}
		tv2.uint(3, 4242)
		u2.msg(3, tv2)
		n.msg(4, u2)
		resp := &enc{}
		resp.msg(1, n)
		writeFrame(w, resp.b)
	})
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	c := &Client{Target: srv.Listener.Addr().String(), Username: "u", Password: "p", TLS: true, Insecure: true}
	defer c.Close()
	ctx := context.Background()
	caps, err := c.Capabilities(ctx)
	if err != nil || caps.Version != "0.10.0" || len(caps.Models) != 1 || !caps.Supports(EncJSONIETF) || caps.Supports(EncProto) {
		t.Fatalf("capabilities: %+v %v", caps, err)
	}
	ups, err := c.Get(ctx, []string{"/system/state", "/interfaces/interface[name=e1]/state/counters/in-octets"}, 2, EncJSONIETF)
	if err != nil || len(ups) != 2 {
		t.Fatalf("get: %v %v", ups, err)
	}
	tr := Tree(ups)
	if Str(Lookup(tr, "/system/state/hostname")) != "r1" || Uint(Lookup(tr, "/interfaces/interface[name=e1]/state/counters/in-octets")) != 4242 {
		t.Fatalf("tree: %v", tr)
	}
	bad := &Client{Target: srv.Listener.Addr().String(), Username: "u", Password: "x", TLS: true, Insecure: true}
	defer bad.Close()
	if _, err := bad.Capabilities(ctx); err == nil || err.Error() != "gnmi: unauthenticated (bad credentials)" {
		t.Fatalf("trailers-only error: %v", err)
	}
}

func writeFrame(w http.ResponseWriter, msg []byte) {
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "grpc-status, grpc-message")
	w.WriteHeader(200)
	frame := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(msg)))
	copy(frame[5:], msg)
	w.Write(frame)
	w.Header().Set("grpc-status", "0")
}
