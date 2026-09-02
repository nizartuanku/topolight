package snmp

import (
	"crypto/md5"
	"crypto/sha1"
	"hash"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The live tests need a net-snmp agent; run scripts/lab/snmpd.sh first or set
// TOPOLIGHT_SNMPD=127.0.0.1:16161. Without it they are skipped.
func agentAddr(t *testing.T) string {
	t.Helper()
	a := os.Getenv("TOPOLIGHT_SNMPD")
	if a == "" {
		t.Skip("TOPOLIGHT_SNMPD not set")
	}
	return a
}

func TestBERRoundTrip(t *testing.T) {
	p := PDU{Type: pduGetRequest, RequestID: 12345, VarBinds: []VarBind{
		{OID: "1.3.6.1.2.1.1.5.0", Value: Value{Kind: KindNull}},
		{OID: "1.3.6.1.2.1.2.2.1.10.100000", Value: Value{Kind: KindCounter64, Uint: 18446744073709551615}},
		{OID: "1.3.6.1.4.1.9.9.109.1.1.1.1.7.1", Value: Value{Kind: KindInteger, Int: -129}},
		{OID: "1.3.6.1.2.1.1.1.0", Value: Value{Kind: KindOctetString, Bytes: []byte("Cisco IOS")}},
		{OID: "1.3.6.1.2.1.1.2.0", Value: Value{Kind: KindOID, OID: "1.3.6.1.4.1.9.1.2494"}},
		{OID: "1.3.6.1.2.1.4.20.1.1.10.20.1.1", Value: Value{Kind: KindIPAddress, Bytes: []byte{10, 20, 1, 1}}},
		{OID: "1.3.6.1.2.1.1.3.0", Value: Value{Kind: KindTimeTicks, Int: 4294967295}},
	}}
	b, err := EncodePDU(p)
	if err != nil {
		t.Fatal(err)
	}
	r := &reader{b: b}
	tag, content, err := r.tlv()
	if err != nil {
		t.Fatal(err)
	}
	q, err := DecodePDU(tag, content)
	if err != nil {
		t.Fatal(err)
	}
	if q.RequestID != 12345 || len(q.VarBinds) != len(p.VarBinds) {
		t.Fatalf("decoded %+v", q)
	}
	for i := range p.VarBinds {
		if q.VarBinds[i].OID != p.VarBinds[i].OID {
			t.Fatalf("oid %d: %s != %s", i, q.VarBinds[i].OID, p.VarBinds[i].OID)
		}
		if q.VarBinds[i].Value.String() != p.VarBinds[i].Value.String() {
			t.Fatalf("value %d: %s != %s", i, q.VarBinds[i].Value.String(), p.VarBinds[i].Value.String())
		}
	}
	// Large lengths (>127 and >255) must encode with long form.
	big := PDU{Type: pduGetResponse, RequestID: 1, VarBinds: []VarBind{{OID: "1.3.6.1.2.1.1.1.0", Value: Value{Kind: KindOctetString, Bytes: []byte(strings.Repeat("x", 300))}}}}
	bb, _ := EncodePDU(big)
	r = &reader{b: bb}
	tag, content, _ = r.tlv()
	qq, err := DecodePDU(tag, content)
	if err != nil || len(qq.VarBinds[0].Value.Bytes) != 300 {
		t.Fatalf("long form: %v", err)
	}
}

func TestLocalizeKeyVector(t *testing.T) {
	// RFC 3414 A.3.1 test vector: password "maplesyrup", engine 00..0002.
	engine := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}
	k := localizeKey(md5New, "maplesyrup", engine)
	want := "526f5eed9fcce26f8964c2930787d82b"
	if got := hexStr(k); got != want {
		t.Fatalf("md5 localized key %s != %s", got, want)
	}
	k = localizeKey(sha1New, "maplesyrup", engine)
	want = "6695febc9288e36282235fc7151f128497b38f3f"
	if got := hexStr(k); got != want {
		t.Fatalf("sha1 localized key %s != %s", got, want)
	}
}

func TestLiveV2c(t *testing.T) {
	c := &Client{Addr: agentAddr(t), Version: V2c, Community: "public", Timeout: 2 * time.Second}
	defer c.Close()
	vbs, err := c.Get("1.3.6.1.2.1.1.5.0", "1.3.6.1.2.1.1.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(vbs) != 2 || vbs[0].Value.String() != "lab-agent" || vbs[1].Value.Kind != KindTimeTicks {
		t.Fatalf("got %+v", vbs)
	}
	sys, err := c.Walk("1.3.6.1.2.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sys) < 7 {
		t.Fatalf("walk system returned %d", len(sys))
	}
	// Compare with net-snmp if available.
	if _, err := exec.LookPath("snmpwalk"); err == nil {
		out, err := exec.Command("snmpwalk", "-v2c", "-c", "public", "-On", "-Oq", c.Addr, "1.3.6.1.2.1.1").Output()
		if err == nil {
			ref := strings.Count(strings.TrimSpace(string(out)), "\n") + 1
			if ref != len(sys) {
				t.Fatalf("walk count %d != snmpwalk %d", len(sys), ref)
			}
		}
	}
	// Wrong community must time out, not hang forever.
	bad := &Client{Addr: agentAddr(t), Version: V2c, Community: "nope", Timeout: 300 * time.Millisecond, Retries: 1}
	defer bad.Close()
	if _, err := bad.Get("1.3.6.1.2.1.1.5.0"); err == nil {
		t.Fatal("wrong community accepted")
	}
}

func TestLiveV3(t *testing.T) {
	addr := agentAddr(t)
	cases := []struct {
		name, user, auth, priv string
	}{
		{"sha-aes", "usersha", "sha", "aes"},
		{"sha256-aes", "user256", "sha256", "aes"},
		{"md5-des", "usermd5", "md5", "des"},
		{"noauth", "usernoauth", "", ""},
		{"sha-authonly", "usersha", "sha", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{Addr: addr, Version: V3, User: tc.user, AuthProto: tc.auth, AuthPass: "authpass123", PrivProto: tc.priv, PrivPass: "privpass123", Timeout: 2 * time.Second}
			defer c.Close()
			if tc.name == "sha-authonly" {
				// agent requires priv for this user; expect a report, not a hang
				_, err := c.Get("1.3.6.1.2.1.1.5.0")
				if err == nil {
					t.Fatal("authNoPriv accepted for priv-only user")
				}
				return
			}
			vbs, err := c.Get("1.3.6.1.2.1.1.5.0")
			if err != nil {
				t.Fatal(err)
			}
			if vbs[0].Value.String() != "lab-agent" {
				t.Fatalf("got %+v", vbs)
			}
			w, err := c.Walk("1.3.6.1.2.1.2.2.1.2")
			if err != nil || len(w) == 0 {
				t.Fatalf("walk ifDescr: %v %d", err, len(w))
			}
		})
	}
	// Wrong auth password must fail closed.
	c := &Client{Addr: addr, Version: V3, User: "usersha", AuthProto: "sha", AuthPass: "wrong", PrivProto: "aes", PrivPass: "privpass123", Timeout: time.Second}
	defer c.Close()
	if _, err := c.Get("1.3.6.1.2.1.1.5.0"); err == nil {
		t.Fatal("wrong auth accepted")
	}
}

func hexStr(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, h[c>>4], h[c&15])
	}
	return string(out)
}

func md5New() hash.Hash  { return md5.New() }
func sha1New() hash.Hash { return sha1.New() }
