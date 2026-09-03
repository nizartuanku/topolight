// Package gnmi is a small gNMI client (Capabilities and Get) built on the
// standard library only: gRPC unary calls are plain HTTP/2 POSTs with a
// five-byte length prefix, and the protobuf messages are hand-encoded from
// the gnmi.proto field numbers. It covers what TopoLight needs — reading
// OpenConfig state from devices that have gNMI but no useful SNMP — and is
// deliberately not a general gRPC stack: no streaming, no compression.
package gnmi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client talks to one gNMI target.
type Client struct {
	Target   string // host:port
	Username string
	Password string
	TLS      bool // gRPC over TLS (the norm); false = h2c "prior knowledge" plaintext
	Insecure bool // skip certificate verification
	Timeout  time.Duration

	http *http.Client
}

// Encoding values from gnmi.proto.
const (
	EncJSON     = 0
	EncBytes    = 1
	EncProto    = 2
	EncASCII    = 3
	EncJSONIETF = 4
)

// Capabilities is the CapabilityResponse.
type Capabilities struct {
	Version   string
	Models    []Model
	Encodings []int
}

// Model is one supported YANG model.
type Model struct{ Name, Organization, Version string }

// Update is one leaf or subtree from a GetResponse.
type Update struct {
	Path string // /a/b[k=v]/c
	Val  any    // string, int64, uint64, bool, float64, []byte, json.RawMessage (json / json_ietf), []any (leaf-list)
}

func (c *Client) client() *http.Client {
	if c.http != nil {
		return c.http
	}
	tr := &http.Transport{ForceAttemptHTTP2: true, DisableCompression: true, ResponseHeaderTimeout: c.timeout(),
		DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	if c.TLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: c.Insecure, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12} //nolint:gosec // operator's choice for self-signed device certs
	} else {
		p := new(http.Protocols)
		p.SetUnencryptedHTTP2(true)
		tr.Protocols = p
	}
	c.http = &http.Client{Transport: tr, Timeout: c.timeout() + 5*time.Second}
	return c.http
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

// Close releases idle connections.
func (c *Client) Close() {
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// call performs one unary gRPC request.
func (c *Client) call(ctx context.Context, method string, msg []byte) ([]byte, error) {
	scheme := "http"
	if c.TLS {
		scheme = "https"
	}
	frame := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(msg)))
	copy(frame[5:], msg)
	req, err := http.NewRequestWithContext(ctx, "POST", scheme+"://"+c.Target+"/gnmi.gNMI/"+method, bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc+proto")
	req.Header.Set("TE", "trailers")
	req.Header.Set("User-Agent", "topolight-gnmi/1")
	if c.Username != "" {
		req.Header.Set("username", c.Username)
		req.Header.Set("password", c.Password)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		return nil, fmt.Errorf("gnmi: %s did not speak HTTP/2 (got HTTP/%d.%d) — is this a gNMI port?", c.Target, resp.ProtoMajor, resp.ProtoMinor)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gnmi: HTTP %d from %s", resp.StatusCode, c.Target)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	// grpc-status arrives in the trailers (or in the headers for trailers-only replies)
	st := resp.Trailer.Get("grpc-status")
	gm := resp.Trailer.Get("grpc-message")
	if st == "" {
		st, gm = resp.Header.Get("grpc-status"), resp.Header.Get("grpc-message")
	}
	if st != "" && st != "0" {
		return nil, fmt.Errorf("gnmi: %s (%s)", grpcStatus(st), unescape(gm))
	}
	if len(body) < 5 {
		if st == "" {
			return nil, errors.New("gnmi: empty reply without a grpc-status")
		}
		return nil, nil
	}
	if body[0] != 0 {
		return nil, errors.New("gnmi: compressed responses are not supported")
	}
	n := binary.BigEndian.Uint32(body[1:5])
	if int(n) > len(body)-5 {
		return nil, errors.New("gnmi: truncated response frame")
	}
	return body[5 : 5+n], nil
}

func grpcStatus(code string) string {
	names := map[string]string{"1": "cancelled", "2": "unknown error", "3": "invalid argument", "4": "deadline exceeded", "5": "not found", "7": "permission denied", "12": "unimplemented", "13": "internal error", "14": "unavailable", "16": "unauthenticated"}
	if n, ok := names[code]; ok {
		return n
	}
	return "grpc status " + code
}

func unescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				out.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// Capabilities asks the target what it supports.
func (c *Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	raw, err := c.call(ctx, "Capabilities", nil)
	if err != nil {
		return nil, err
	}
	fs, err := decode(raw)
	if err != nil {
		return nil, err
	}
	out := &Capabilities{}
	for _, f := range fs {
		switch f.num {
		case 1: // supported_models
			m := Model{}
			if sub, err := decode(f.data); err == nil {
				for _, g := range sub {
					switch g.num {
					case 1:
						m.Name = string(g.data)
					case 2:
						m.Organization = string(g.data)
					case 3:
						m.Version = string(g.data)
					}
				}
			}
			out.Models = append(out.Models, m)
		case 2: // supported_encodings (packed or not)
			if f.wt == wireBytes {
				b := f.data
				for len(b) > 0 {
					v, n := binary.Uvarint(b)
					if n <= 0 {
						break
					}
					out.Encodings = append(out.Encodings, int(v))
					b = b[n:]
				}
			} else {
				out.Encodings = append(out.Encodings, int(f.u))
			}
		case 3:
			out.Version = string(f.data)
		}
	}
	return out, nil
}

// Supports reports whether an encoding is offered (an empty list means the
// target did not say — assume yes).
func (cp *Capabilities) Supports(enc int) bool {
	if len(cp.Encodings) == 0 {
		return true
	}
	for _, e := range cp.Encodings {
		if e == enc {
			return true
		}
	}
	return false
}

// ---- paths ----

type pathElem struct {
	name string
	keys [][2]string
}

// parsePath turns "/interfaces/interface[name=Ethernet1]/state" into elements.
// Escaped ']' inside key values ("\]") are honoured.
func parsePath(p string) (origin string, elems []pathElem) {
	p = strings.TrimSpace(p)
	if i := strings.Index(p, ":"); i > 0 && !strings.Contains(p[:i], "/") && !strings.Contains(p[:i], "[") {
		origin, p = p[:i], p[i+1:]
	}
	p = strings.TrimPrefix(p, "/")
	var cur strings.Builder
	var el pathElem
	inKey := false
	flush := func() {
		if cur.Len() > 0 || el.name != "" {
			if el.name == "" {
				el.name = cur.String()
			}
			elems = append(elems, el)
		}
		el = pathElem{}
		cur.Reset()
	}
	var kv strings.Builder
	for i := 0; i < len(p); i++ {
		ch := p[i]
		switch {
		case inKey:
			if ch == '\\' && i+1 < len(p) {
				i++
				kv.WriteByte(p[i])
				continue
			}
			if ch == ']' {
				k, v, _ := strings.Cut(kv.String(), "=")
				el.keys = append(el.keys, [2]string{k, v})
				kv.Reset()
				inKey = false
				continue
			}
			kv.WriteByte(ch)
		case ch == '[':
			if el.name == "" {
				el.name = cur.String()
			}
			inKey = true
		case ch == '/':
			flush()
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return origin, elems
}

func encodePath(origin string, elems []pathElem) *enc {
	e := &enc{}
	e.str(2, origin)
	for _, el := range elems {
		sub := &enc{}
		sub.str(1, el.name)
		for _, kv := range el.keys {
			m := &enc{}
			m.str(1, kv[0])
			m.str(2, kv[1])
			sub.msg(2, m)
		}
		e.msg(3, sub)
	}
	return e
}

func decodePath(b []byte) (string, error) {
	fs, err := decode(b)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, f := range fs {
		switch f.num {
		case 1: // deprecated element
			sb.WriteByte('/')
			sb.Write(f.data)
		case 3:
			sub, err := decode(f.data)
			if err != nil {
				return "", err
			}
			sb.WriteByte('/')
			for _, g := range sub {
				switch g.num {
				case 1:
					sb.Write(g.data)
				case 2:
					kv, _ := decode(g.data)
					var k, v string
					for _, x := range kv {
						if x.num == 1 {
							k = string(x.data)
						} else if x.num == 2 {
							v = string(x.data)
						}
					}
					sb.WriteString("[" + k + "=" + strings.ReplaceAll(v, "]", "\\]") + "]")
				}
			}
		}
	}
	return sb.String(), nil
}

// ---- Get ----

// Get reads the given paths. dataType: 0 ALL, 1 CONFIG, 2 STATE, 3 OPERATIONAL.
func (c *Client) Get(ctx context.Context, paths []string, dataType, encoding int) ([]Update, error) {
	req := &enc{}
	for _, p := range paths {
		origin, elems := parsePath(p)
		req.msg(2, encodePath(origin, elems))
	}
	req.uint(3, uint64(dataType))
	req.uint(5, uint64(encoding))
	raw, err := c.call(ctx, "Get", req.b)
	if err != nil {
		return nil, err
	}
	fs, err := decode(raw)
	if err != nil {
		return nil, err
	}
	var out []Update
	for _, f := range fs {
		if f.num != 1 { // notification
			continue
		}
		nf, err := decode(f.data)
		if err != nil {
			return nil, err
		}
		prefix := ""
		var ups [][]byte
		for _, g := range nf {
			switch g.num {
			case 2:
				prefix, _ = decodePath(g.data)
			case 4:
				ups = append(ups, g.data)
			}
		}
		for _, u := range ups {
			uf, err := decode(u)
			if err != nil {
				return nil, err
			}
			up := Update{}
			for _, x := range uf {
				switch x.num {
				case 1:
					p, _ := decodePath(x.data)
					up.Path = prefix + p
				case 3:
					up.Val = decodeTyped(x.data)
				}
			}
			if up.Path == "" {
				up.Path = prefix
			}
			out = append(out, up)
		}
	}
	return out, nil
}

func decodeTyped(b []byte) any {
	fs, err := decode(b)
	if err != nil || len(fs) == 0 {
		return nil
	}
	f := fs[0]
	switch f.num {
	case 1, 12:
		return string(f.data)
	case 2:
		return int64(f.u)
	case 3:
		return f.u
	case 4:
		return f.u != 0
	case 5, 13:
		return f.data
	case 6:
		return f32(f.u)
	case 14:
		return f64(f.u)
	case 7: // Decimal64 {digits=1 sint64, precision=2 uint32}
		sub, _ := decode(f.data)
		var digits int64
		var prec uint64
		for _, g := range sub {
			if g.num == 1 {
				digits = int64(g.u)
			} else if g.num == 2 {
				prec = g.u
			}
		}
		v := float64(digits)
		for i := uint64(0); i < prec; i++ {
			v /= 10
		}
		return v
	case 8: // ScalarArray
		sub, _ := decode(f.data)
		var arr []any
		for _, g := range sub {
			if g.num == 1 {
				arr = append(arr, decodeTyped(g.data))
			}
		}
		return arr
	case 10, 11:
		return json.RawMessage(f.data)
	}
	return nil
}

// ---- trees ----

// Tree merges updates into one nested map so callers can walk OpenConfig
// data regardless of whether the target answered with one JSON blob per
// container or one typed leaf per path. Module prefixes in JSON_IETF keys
// ("openconfig-interfaces:interfaces") are stripped.
func Tree(ups []Update) map[string]any {
	root := map[string]any{}
	for _, u := range ups {
		_, elems := parsePath(u.Path)
		node := root
		for i, el := range elems {
			last := i == len(elems)-1
			if len(el.keys) > 0 {
				// list entry: keep as a slice of maps under the list name
				lst, _ := node[el.name].([]any)
				var entry map[string]any
				for _, x := range lst {
					m, _ := x.(map[string]any)
					if m != nil && keysMatch(m, el.keys) {
						entry = m
						break
					}
				}
				if entry == nil {
					entry = map[string]any{}
					for _, kv := range el.keys {
						entry[kv[0]] = kv[1]
					}
					lst = append(lst, entry)
					node[el.name] = lst
				}
				if last {
					mergeInto(entry, unwrap(normalise(u.Val), el.name))
				}
				node = entry
				continue
			}
			if last {
				if v := unwrap(normalise(u.Val), el.name); isMap(v) {
					child, _ := node[el.name].(map[string]any)
					if child == nil {
						child = map[string]any{}
						node[el.name] = child
					}
					mergeInto(child, v)
				} else {
					node[el.name] = v
				}
				break
			}
			child, _ := node[el.name].(map[string]any)
			if child == nil {
				child = map[string]any{}
				node[el.name] = child
			}
			node = child
		}
		if len(elems) == 0 {
			mergeInto(root, u.Val)
		}
	}
	return root
}

func isMap(v any) bool { _, ok := v.(map[string]any); return ok }

// unwrap drops a redundant outer container: some targets answer a Get of
// /interfaces with {"interfaces": {...}} rather than the node's contents.
func unwrap(v any, name string) any {
	if m, ok := v.(map[string]any); ok && len(m) == 1 {
		if inner, ok := m[name]; ok && isMap(inner) {
			return inner
		}
	}
	return v
}

func keysMatch(m map[string]any, keys [][2]string) bool {
	for _, kv := range keys {
		if fmt.Sprint(m[kv[0]]) != kv[1] {
			return false
		}
	}
	return true
}

// normalise decodes JSON values and strips module prefixes from keys.
func normalise(v any) any {
	switch x := v.(type) {
	case json.RawMessage:
		var out any
		d := json.NewDecoder(bytes.NewReader(x))
		d.UseNumber()
		if d.Decode(&out) != nil {
			return string(x)
		}
		return normalise(out)
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			if i := strings.LastIndex(k, ":"); i >= 0 {
				k = k[i+1:]
			}
			m[k] = normalise(val)
		}
		return m
	case []any:
		for i := range x {
			x[i] = normalise(x[i])
		}
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if u, err := strconv.ParseUint(string(x), 10, 64); err == nil {
			return u
		}
		f, _ := x.Float64()
		return f
	}
	return v
}

func mergeInto(dst map[string]any, v any) {
	src, _ := normalise(v).(map[string]any)
	for k, val := range src {
		if sm, ok := val.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeInto(dm, sm)
				continue
			}
		}
		dst[k] = val
	}
}

// Lookup walks a tree by "/a/b/c"; list entries are matched by [k=v].
func Lookup(tree map[string]any, path string) any {
	_, elems := parsePath(path)
	var cur any = tree
	for _, el := range elems {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[el.name]
		if len(el.keys) > 0 {
			lst, _ := cur.([]any)
			cur = nil
			for _, x := range lst {
				if em, _ := x.(map[string]any); em != nil && keysMatch(em, el.keys) {
					cur = em
					break
				}
			}
		}
	}
	return cur
}

// Number coerces a leaf into a float64 (ints, uints, floats, numeric strings).
func Number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// Uint coerces a counter leaf.
func Uint(v any) uint64 {
	switch x := v.(type) {
	case uint64:
		return x
	case int64:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case float64:
		return uint64(x)
	case string:
		u, _ := strconv.ParseUint(x, 10, 64)
		return u
	}
	return 0
}

// Str coerces a leaf to string.
func Str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}
