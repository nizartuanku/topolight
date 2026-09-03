package snmp

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Well-known OIDs used by traps and polling.
const (
	OIDSysUpTime   = "1.3.6.1.2.1.1.3.0"
	OIDSnmpTrapOID = "1.3.6.1.6.3.1.1.4.1.0"

	TrapColdStart   = "1.3.6.1.6.3.1.1.5.1"
	TrapWarmStart   = "1.3.6.1.6.3.1.1.5.2"
	TrapLinkDown    = "1.3.6.1.6.3.1.1.5.3"
	TrapLinkUp      = "1.3.6.1.6.3.1.1.5.4"
	TrapAuthFailure = "1.3.6.1.6.3.1.1.5.5"
)

// Trap is a decoded v2c notification.
type Trap struct {
	From      string
	Community string
	Uptime    int64 // ticks
	TrapOID   string
	VarBinds  []VarBind
	Inform    bool
	RequestID int32
	V3User    string // set for SNMPv3 notifications
}

// DecodeTrap parses a datagram received on UDP/162.
func DecodeTrap(from net.Addr, b []byte) (Trap, error) {
	community, pdu, err := DecodeV2Message(b)
	if err != nil {
		return Trap{}, err
	}
	if pdu.Type != pduTrapV2 && pdu.Type != pduInform {
		return Trap{}, fmt.Errorf("snmp: pdu 0x%02x is not a notification", pdu.Type)
	}
	t := Trap{Community: community, VarBinds: pdu.VarBinds, Inform: pdu.Type == pduInform, RequestID: pdu.RequestID}
	if from != nil {
		host, _, err := net.SplitHostPort(from.String())
		if err == nil {
			t.From = host
		} else {
			t.From = from.String()
		}
	}
	for _, vb := range pdu.VarBinds {
		switch vb.OID {
		case OIDSysUpTime:
			t.Uptime = vb.Value.Int
		case OIDSnmpTrapOID:
			t.TrapOID = vb.Value.OID
		}
	}
	if t.TrapOID == "" {
		return t, errors.New("snmp: notification without snmpTrapOID")
	}
	return t, nil
}

// Get returns the first varbind beneath prefix (the instance-suffixed one).
func (t Trap) Get(prefix string) (VarBind, bool) {
	for _, vb := range t.VarBinds {
		if OIDHasPrefix(vb.OID, prefix) {
			return vb, true
		}
	}
	return VarBind{}, false
}

// InformResponse builds the v2c response that acknowledges an INFORM.
func InformResponse(community string, pdu PDU) []byte {
	pdu.Type = pduGetResponse
	body, err := EncodePDU(pdu)
	if err != nil {
		return nil
	}
	return encodeSequence(encodeInt(tagInteger, 1), encodeOctets([]byte(community)), body)
}

// EncodeTrapV2 builds a v2c TRAP message (used by tests and the lab).
func EncodeTrapV2(community string, uptime int64, trapOID string, vbs []VarBind) ([]byte, error) {
	all := append([]VarBind{
		{OID: OIDSysUpTime, Value: Value{Kind: KindTimeTicks, Int: uptime}},
		{OID: OIDSnmpTrapOID, Value: Value{Kind: KindOID, OID: trapOID}},
	}, vbs...)
	body, err := EncodePDU(PDU{Type: pduTrapV2, RequestID: 1, VarBinds: all})
	if err != nil {
		return nil, err
	}
	return encodeSequence(encodeInt(tagInteger, 1), encodeOctets([]byte(community)), body), nil
}

// MACString formats a 6-byte octet string as aa:bb:cc:dd:ee:ff.
func MACString(b []byte) string {
	if len(b) != 6 {
		return ""
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// PrintableOrHex renders an octet string for humans.
func PrintableOrHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	printable := true
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			if c != '\n' && c != '\r' && c != '\t' {
				printable = false
				break
			}
		}
	}
	if printable {
		return strings.TrimSpace(string(b))
	}
	if len(b) == 6 {
		return MACString(b)
	}
	return fmt.Sprintf("%x", b)
}

// ---- SNMPv3 notifications ----

// V3User is what the receiver needs to authenticate a v3 trap sender.
type V3User struct {
	User, AuthProto, AuthPass, PrivProto, PrivPass string
}

// V3Receiver decodes v3 traps/informs for a set of USM users. Keys are
// localised per (user, engineID) once and cached — the RFC 3414 key
// derivation costs ~1M hash operations, far too much per datagram.
type V3Receiver struct {
	mu    sync.Mutex
	users func() []V3User
	keys  map[string]*Client // user|engineID → prepared client
	// Our own engine identity, used when we are the authoritative engine
	// (informs): senders discover it with an unauthenticated request and we
	// answer with a Report carrying it.
	EngineID []byte
	Boots    int32
	started  time.Time
}

// NewV3Receiver builds one; users is called on every unknown (user, engine)
// pair so credential edits take effect without a restart.
func NewV3Receiver(users func() []V3User) *V3Receiver {
	return &V3Receiver{users: users, keys: map[string]*Client{}, started: time.Now()}
}

// SetEngine sets the receiver's own engine id and boots counter.
func (v *V3Receiver) SetEngine(id []byte, boots int32) {
	v.mu.Lock()
	v.EngineID, v.Boots, v.started = append([]byte(nil), id...), boots, time.Now()
	v.mu.Unlock()
}

func (v *V3Receiver) engineTime() int32 { return int32(time.Since(v.started).Seconds()) }

// discoveryReport answers an engine-discovery probe (empty engine id) with
// the unauthenticated Report every manager expects (RFC 3414 §4).
func (v *V3Receiver) discoveryReport(m v3msg) []byte {
	v.mu.Lock()
	id, boots, et := v.EngineID, v.Boots, v.engineTime()
	v.mu.Unlock()
	if len(id) == 0 {
		return nil
	}
	pdu := PDU{Type: pduReport, RequestID: 0, VarBinds: []VarBind{{OID: oidUnknownEngineID, Value: Value{Kind: KindCounter32, Int: 1}}}}
	scoped := encodeSequence(encodeOctets(id), encodeOctets(nil), mustPDU(pdu))
	usm := encodeSequence(encodeOctets(id), encodeInt(tagInteger, int64(boots)), encodeInt(tagInteger, int64(et)),
		encodeOctets(nil), encodeOctets(nil), encodeOctets(nil))
	c := &Client{}
	return encodeSequence(encodeInt(tagInteger, 3), c.globalData(m.msgID, 0), encodeOctets(usm), scoped)
}

// Errors reported by Decode.
var (
	ErrV3UnknownUser = errors.New("snmp: v3 trap from unknown user")
	ErrV3Auth        = errors.New("snmp: v3 trap authentication failed")
	// ErrV3Discovery is returned with the Report to send back; not a failure.
	ErrV3Discovery = errors.New("snmp: v3 engine discovery")
)

// Decode verifies and (if needed) decrypts a v3 notification. For an inform it
// also returns the authenticated response to send back.
func (v *V3Receiver) Decode(from net.Addr, b []byte) (Trap, []byte, error) {
	m, err := parseV3(b)
	if err != nil {
		return Trap{}, nil, err
	}
	if len(m.engineID) == 0 && m.flags&0x04 != 0 {
		// engine discovery: not a trap, just tell them who we are
		if rep := v.discoveryReport(m); rep != nil {
			return Trap{}, rep, ErrV3Discovery
		}
		return Trap{}, nil, ErrV3UnknownUser
	}
	if m.user == "" {
		return Trap{}, nil, ErrV3UnknownUser
	}
	c, err := v.clientFor(m.user, m.engineID)
	if err != nil {
		return Trap{}, nil, err
	}
	// the sender is authoritative for traps: its boots/time drive the IV.
	// For informs addressed to our engine we are authoritative and answer
	// with our own boots/time.
	c.boots, c.etime, c.syncedAt = m.boots, m.etime, time.Now()
	v.mu.Lock()
	ours := len(v.EngineID) > 0 && bytes.Equal(m.engineID, v.EngineID)
	if ours {
		c.boots, c.etime = v.Boots, v.engineTime()
	}
	v.mu.Unlock()
	if c.AuthProto != "" {
		if m.flags&0x01 == 0 || !c.verify(b, m) {
			return Trap{}, nil, ErrV3Auth
		}
	}
	scoped := m.scoped
	if m.flags&0x02 != 0 {
		if c.PrivProto == "" {
			return Trap{}, nil, ErrV3Auth
		}
		dec, err := c.decrypt(m.encrypted, m.boots, m.etime, m.privParams)
		if err != nil {
			return Trap{}, nil, err
		}
		scoped = dec
	} else if c.PrivProto != "" {
		return Trap{}, nil, ErrV3Auth // we require what the credential says
	}
	pdu, ctxEngine, ctxName, err := parseScopedFull(scoped)
	if err != nil {
		return Trap{}, nil, err
	}
	if pdu.Type != pduTrapV2 && pdu.Type != pduInform {
		return Trap{}, nil, fmt.Errorf("snmp: pdu 0x%02x is not a notification", pdu.Type)
	}
	t := Trap{Community: "v3:" + m.user, VarBinds: pdu.VarBinds, Inform: pdu.Type == pduInform, RequestID: pdu.RequestID, V3User: m.user}
	if from != nil {
		if host, _, err := net.SplitHostPort(from.String()); err == nil {
			t.From = host
		} else {
			t.From = from.String()
		}
	}
	for _, vb := range pdu.VarBinds {
		switch vb.OID {
		case OIDSysUpTime:
			t.Uptime = vb.Value.Int
		case OIDSnmpTrapOID:
			t.TrapOID = vb.Value.OID
		}
	}
	if t.TrapOID == "" {
		return t, nil, errors.New("snmp: notification without snmpTrapOID")
	}
	var resp []byte
	if t.Inform {
		pdu.Type = pduGetResponse
		// a shallow copy without the mutex: the cached client must not carry per-message context
		rc := &Client{Version: V3, User: c.User, AuthProto: c.AuthProto, PrivProto: c.PrivProto, engineID: c.engineID, boots: c.boots, etime: c.etime, syncedAt: c.syncedAt, authKey: c.authKey, privKey: c.privKey, salt: uint64(time.Now().UnixNano()), ctxEngineID: ctxEngine, ContextName: ctxName}
		resp, err = rc.buildV3(m.msgID, m.flags&0x03, pdu)
		if err != nil {
			return t, nil, fmt.Errorf("snmp: inform response: %w", err)
		}
	}
	return t, resp, nil
}

func (v *V3Receiver) clientFor(user string, engineID []byte) (*Client, error) {
	key := user + "|" + fmt.Sprintf("%x", engineID)
	v.mu.Lock()
	defer v.mu.Unlock()
	if c := v.keys[key]; c != nil {
		return c, nil
	}
	for _, u := range v.users() {
		if u.User != user {
			continue
		}
		c := &Client{Version: V3, User: u.User, AuthProto: u.AuthProto, AuthPass: u.AuthPass, PrivProto: u.PrivProto, PrivPass: u.PrivPass, engineID: append([]byte(nil), engineID...)}
		c.prepareKeys()
		if len(v.keys) > 4096 {
			v.keys = map[string]*Client{}
		}
		v.keys[key] = c
		return c, nil
	}
	return nil, ErrV3UnknownUser
}

// Forget drops cached keys (after credential edits).
func (v *V3Receiver) Forget() {
	v.mu.Lock()
	v.keys = map[string]*Client{}
	v.mu.Unlock()
}

// IsV3 reports whether a datagram carries SNMP version 3.
func IsV3(b []byte) bool {
	r := &reader{b: b}
	tag, seq, err := r.tlv()
	if err != nil || tag != tagSequence {
		return false
	}
	rr := &reader{b: seq}
	t, c, err := rr.tlv()
	if err != nil || t != tagInteger {
		return false
	}
	ver, _ := decodeInt(c)
	return ver == 3
}
