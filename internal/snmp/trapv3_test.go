package snmp

import (
	"net"
	"testing"
)

func TestV3TrapRoundTrip(t *testing.T) {
	users := func() []V3User {
		return []V3User{{User: "nms", AuthProto: "sha", AuthPass: "authpass123", PrivProto: "aes", PrivPass: "privpass123"}}
	}
	engine := []byte{0x80, 0x00, 0x1f, 0x88, 0x80, 0x01, 0x02, 0x03, 0x04}
	// the "agent" side: same keys localised to its own engine id
	agent := &Client{Version: V3, User: "nms", AuthProto: "sha", AuthPass: "authpass123", PrivProto: "aes", PrivPass: "privpass123", engineID: engine, boots: 3, etime: 1000}
	agent.prepareKeys()
	pdu := PDU{Type: pduTrapV2, RequestID: 77, VarBinds: []VarBind{
		{OID: OIDSysUpTime, Value: Value{Kind: KindTimeTicks, Int: 12345}},
		{OID: OIDSnmpTrapOID, Value: Value{Kind: KindOID, OID: TrapLinkDown}},
		{OID: "1.3.6.1.2.1.2.2.1.1.7", Value: Value{Kind: KindInteger, Int: 7}},
	}}
	msg, err := agent.buildV3(4242, agent.flags(), pdu)
	if err != nil {
		t.Fatal(err)
	}
	if !IsV3(msg) {
		t.Fatal("not detected as v3")
	}
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 4000}
	rx := NewV3Receiver(users)
	tr, resp, err := rx.Decode(from, msg)
	if err != nil {
		t.Fatal(err)
	}
	if tr.TrapOID != TrapLinkDown || tr.From != "10.0.0.5" || tr.V3User != "nms" || len(tr.VarBinds) != 3 || resp != nil {
		t.Fatalf("trap: %+v", tr)
	}
	// tampered digest must fail
	bad := append([]byte(nil), msg...)
	bad[len(bad)-1] ^= 0xff
	if _, _, err := rx.Decode(from, bad); err == nil {
		t.Fatal("tampered message accepted")
	}
	// wrong password must fail
	rx2 := NewV3Receiver(func() []V3User {
		return []V3User{{User: "nms", AuthProto: "sha", AuthPass: "wrong-wrong", PrivProto: "aes", PrivPass: "privpass123"}}
	})
	if _, _, err := rx2.Decode(from, msg); err != ErrV3Auth {
		t.Fatalf("wrong password: %v", err)
	}
	// unknown user
	rx3 := NewV3Receiver(func() []V3User { return nil })
	if _, _, err := rx3.Decode(from, msg); err != ErrV3UnknownUser {
		t.Fatalf("unknown user: %v", err)
	}
	// inform → authenticated response that the agent can verify
	pdu.Type = pduInform
	msg, _ = agent.buildV3(4243, agent.flags(), pdu)
	tr, resp, err = rx.Decode(from, msg)
	if err != nil || !tr.Inform || resp == nil {
		t.Fatalf("inform: %v %+v", err, tr)
	}
	m, err := parseV3(resp)
	if err != nil || m.msgID != 4243 || !agent.verify(resp, m) {
		t.Fatalf("inform response not verifiable: %v", err)
	}
	dec, err := agent.decrypt(m.encrypted, m.boots, m.etime, m.privParams)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := parseScoped(dec); err != nil || p.Type != pduGetResponse || p.RequestID != 77 {
		t.Fatalf("inform response pdu: %v %+v", err, p)
	}
	// authNoPriv user
	agent2 := &Client{Version: V3, User: "ro", AuthProto: "sha256", AuthPass: "authpass123", engineID: engine, boots: 1, etime: 5}
	agent2.prepareKeys()
	msg, _ = agent2.buildV3(1, agent2.flags(), pdu)
	rx4 := NewV3Receiver(func() []V3User { return []V3User{{User: "ro", AuthProto: "sha256", AuthPass: "authpass123"}} })
	if _, _, err := rx4.Decode(from, msg); err != nil {
		t.Fatalf("authNoPriv: %v", err)
	}
}
