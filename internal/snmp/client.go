package snmp

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version of the protocol.
type Version int

// Supported versions.
const (
	V2c Version = 1
	V3  Version = 3
)

// DefaultPort is the well-known UDP port for SNMP agents. Callers may dial a
// different one by putting host:port in Addr — lab agents and containerised
// devices often sit on a high port because 161 needs privilege.
const DefaultPort = 161

// Client talks to one agent. It is safe for sequential use from one goroutine
// and is cheap to create per device.
type Client struct {
	Addr      string // host or host:port (default port 161)
	Version   Version
	Community string
	// v3
	User      string
	AuthProto string // "" | md5 | sha | sha256
	AuthPass  string
	PrivProto string // "" | des | aes
	PrivPass  string
	// ContextName selects an SNMPv3 context (e.g. "vlan-10" on Cisco IOS for
	// the per-VLAN bridge tables). Empty is the default context.
	ContextName string

	Timeout time.Duration
	Retries int
	MaxRep  int // GETBULK max-repetitions (default 25)

	mu       sync.Mutex
	conn     net.Conn
	reqID    int32
	engineID []byte
	boots    int32
	etime    int32
	syncedAt time.Time
	authKey  []byte
	privKey  []byte
	salt     uint64
	// ctxEngineID overrides the scoped-PDU context engine (inform responses)
	ctxEngineID []byte
}

// Errors.
var (
	ErrTimeout = errors.New("snmp: timeout")
	ErrAuth    = errors.New("snmp: authentication failed")
)

var globalReqID int32

func (c *Client) nextReqID() int32 {
	return atomic.AddInt32(&globalReqID, 1)&0x3fffffff + 1
}

func (c *Client) dial() error {
	if c.conn != nil {
		return nil
	}
	addr := c.Addr
	if !strings.Contains(addr, ":") || (strings.Count(addr, ":") > 1 && !strings.Contains(addr, "]")) {
		addr = net.JoinHostPort(addr, strconv.Itoa(DefaultPort))
	}
	conn, err := net.DialTimeout("udp", addr, c.timeout())
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}

// Close releases the socket.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *Client) timeout() time.Duration {
	if c.Timeout <= 0 {
		return 2 * time.Second
	}
	return c.Timeout
}

func (c *Client) retries() int {
	if c.Retries < 0 {
		return 0
	}
	if c.Retries == 0 {
		return 1
	}
	return c.Retries
}

// Get performs a GET.
func (c *Client) Get(oids ...string) ([]VarBind, error) {
	return c.request(pduGetRequest, 0, 0, oids)
}

// GetNext performs a GETNEXT.
func (c *Client) GetNext(oids ...string) ([]VarBind, error) {
	return c.request(pduGetNext, 0, 0, oids)
}

// GetBulk performs a GETBULK with the given max-repetitions.
func (c *Client) GetBulk(maxRep int, oids ...string) ([]VarBind, error) {
	return c.request(pduGetBulk, 0, maxRep, oids)
}

// Walk returns every variable beneath prefix using GETBULK.
func (c *Client) Walk(prefix string) ([]VarBind, error) {
	return c.WalkContext(context.Background(), prefix)
}

// WalkContext is Walk with cancellation.
func (c *Client) WalkContext(ctx context.Context, prefix string) ([]VarBind, error) {
	maxRep := c.MaxRep
	if maxRep <= 0 {
		maxRep = 25
	}
	cur := prefix
	var out []VarBind
	for i := 0; i < 20000; i++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		vbs, err := c.GetBulk(maxRep, cur)
		if err != nil {
			return out, err
		}
		if len(vbs) == 0 {
			return out, nil
		}
		for _, vb := range vbs {
			if vb.Value.Kind == KindEndOfMibView || !OIDHasPrefix(vb.OID, prefix) {
				return out, nil
			}
			// Guard against agents that loop.
			if len(out) > 0 && compareOID(vb.OID, out[len(out)-1].OID) <= 0 {
				return out, nil
			}
			out = append(out, vb)
		}
		cur = vbs[len(vbs)-1].OID
	}
	return out, nil
}

func compareOID(a, b string) int {
	x, _ := ParseOID(a)
	y, _ := ParseOID(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(x) < len(y):
		return -1
	case len(x) > len(y):
		return 1
	}
	return 0
}

func (c *Client) request(typ byte, nonRep, maxRep int, oids []string) ([]VarBind, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dial(); err != nil {
		return nil, err
	}
	vbs := make([]VarBind, len(oids))
	for i, o := range oids {
		vbs[i] = VarBind{OID: o, Value: Value{Kind: KindNull}}
	}
	var lastErr error
	for attempt := 0; attempt <= c.retries(); attempt++ {
		pdu := PDU{Type: typ, RequestID: c.nextReqID(), NonRepeaters: nonRep, MaxRepetitions: maxRep, VarBinds: vbs}
		var resp PDU
		var err error
		if c.Version == V3 {
			resp, err = c.exchangeV3(pdu)
		} else {
			resp, err = c.exchangeV2(pdu)
		}
		if err != nil {
			lastErr = err
			if errors.Is(err, ErrAuth) {
				return nil, err
			}
			continue
		}
		if resp.ErrorCode != 0 {
			return resp.VarBinds, fmt.Errorf("snmp: agent error %d at index %d", resp.ErrorCode, resp.ErrorIndex)
		}
		return resp.VarBinds, nil
	}
	if lastErr == nil {
		lastErr = ErrTimeout
	}
	return nil, lastErr
}

// ---- v2c ----

func (c *Client) exchangeV2(pdu PDU) (PDU, error) {
	body, err := EncodePDU(pdu)
	if err != nil {
		return PDU{}, err
	}
	msg := encodeSequence(encodeInt(tagInteger, 1), encodeOctets([]byte(c.Community)), body)
	raw, err := c.roundTrip(msg, func(b []byte) (bool, error) {
		p, err := decodeV2(b)
		if err != nil {
			return false, nil
		}
		return p.RequestID == pdu.RequestID, nil
	})
	if err != nil {
		return PDU{}, err
	}
	return decodeV2(raw)
}

func decodeV2(b []byte) (PDU, error) {
	r := &reader{b: b}
	tag, seq, err := r.tlv()
	if err != nil || tag != tagSequence {
		return PDU{}, errors.New("snmp: not a message")
	}
	rr := &reader{b: seq}
	t, c, err := rr.tlv()
	if err != nil || t != tagInteger {
		return PDU{}, errors.New("snmp: version")
	}
	v, _ := decodeInt(c)
	if v != 1 && v != 0 {
		return PDU{}, fmt.Errorf("snmp: unexpected version %d", v)
	}
	t, _, err = rr.tlv()
	if err != nil || t != tagOctetString {
		return PDU{}, errors.New("snmp: community")
	}
	t, c, err = rr.tlv()
	if err != nil {
		return PDU{}, err
	}
	return DecodePDU(t, c)
}

// DecodeV2Message decodes a complete v1/v2c message (used by the trap receiver)
// and returns the community and PDU.
func DecodeV2Message(b []byte) (community string, pdu PDU, err error) {
	r := &reader{b: b}
	tag, seq, err := r.tlv()
	if err != nil || tag != tagSequence {
		return "", PDU{}, errors.New("snmp: not a message")
	}
	rr := &reader{b: seq}
	t, c, err := rr.tlv()
	if err != nil || t != tagInteger {
		return "", PDU{}, errors.New("snmp: version")
	}
	v, _ := decodeInt(c)
	if v != 1 {
		return "", PDU{}, fmt.Errorf("snmp: unsupported version %d (v2c only)", v)
	}
	t, c, err = rr.tlv()
	if err != nil || t != tagOctetString {
		return "", PDU{}, errors.New("snmp: community")
	}
	community = string(c)
	t, c, err = rr.tlv()
	if err != nil {
		return community, PDU{}, err
	}
	pdu, err = DecodePDU(t, c)
	return community, pdu, err
}

func (c *Client) roundTrip(msg []byte, match func([]byte) (bool, error)) ([]byte, error) {
	deadline := time.Now().Add(c.timeout())
	_ = c.conn.SetWriteDeadline(deadline)
	if _, err := c.conn.Write(msg); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		n, err := c.conn.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil, ErrTimeout
			}
			return nil, err
		}
		b := append([]byte(nil), buf[:n]...)
		ok, err := match(b)
		if err != nil {
			return nil, err
		}
		if ok {
			return b, nil
		}
		// stale reply from an earlier attempt: keep waiting
	}
}

// ---- v3 / USM ----

func (c *Client) hashNew() func() hash.Hash {
	switch strings.ToLower(c.AuthProto) {
	case "md5":
		return md5.New
	case "sha", "sha1":
		return sha1.New
	case "sha256", "sha-256":
		return sha256.New
	}
	return nil
}

func (c *Client) authLen() int {
	switch strings.ToLower(c.AuthProto) {
	case "sha256", "sha-256":
		return 24
	case "md5", "sha", "sha1":
		return 12
	}
	return 0
}

// localizeKey implements RFC 3414 §A.2 password-to-key + localisation.
func localizeKey(h func() hash.Hash, password string, engineID []byte) []byte {
	if password == "" {
		return nil
	}
	hh := h()
	pw := []byte(password)
	const total = 1048576
	var buf [64]byte
	idx, count := 0, 0
	for count < total {
		n := 64
		if total-count < n {
			n = total - count
		}
		for i := 0; i < n; i++ {
			buf[i] = pw[idx%len(pw)]
			idx++
		}
		hh.Write(buf[:n])
		count += n
	}
	ku := hh.Sum(nil)
	hh2 := h()
	hh2.Write(ku)
	hh2.Write(engineID)
	hh2.Write(ku)
	return hh2.Sum(nil)
}

func (c *Client) prepareKeys() {
	h := c.hashNew()
	if h == nil {
		return
	}
	c.authKey = localizeKey(h, c.AuthPass, c.engineID)
	if c.PrivProto != "" {
		c.privKey = localizeKey(h, c.PrivPass, c.engineID)
	}
}

func (c *Client) flags() byte {
	var f byte = 0x04 // reportable
	if c.AuthProto != "" {
		f |= 0x01
		if c.PrivProto != "" {
			f |= 0x02
		}
	}
	return f
}

const (
	oidUnknownEngineID = "1.3.6.1.6.3.15.1.1.4.0"
	oidNotInTimeWindow = "1.3.6.1.6.3.15.1.1.2.0"
	oidWrongDigest     = "1.3.6.1.6.3.15.1.1.5.0"
	oidUnknownUser     = "1.3.6.1.6.3.15.1.1.3.0"
	oidDecryptError    = "1.3.6.1.6.3.15.1.1.6.0"
)

func (c *Client) exchangeV3(pdu PDU) (PDU, error) {
	if len(c.engineID) == 0 {
		if err := c.discover(); err != nil {
			return PDU{}, err
		}
	}
	resp, err := c.sendV3(pdu, true)
	if err != nil {
		return PDU{}, err
	}
	if resp.Type == pduReport {
		if rep := reportOID(resp); rep == oidNotInTimeWindow || rep == oidUnknownEngineID {
			// clock/engine resynchronised by sendV3; retry once
			resp, err = c.sendV3(pdu, true)
			if err != nil {
				return PDU{}, err
			}
			if resp.Type == pduReport {
				return PDU{}, fmt.Errorf("snmp: agent report %s", reportOID(resp))
			}
			return resp, nil
		} else if rep == oidWrongDigest || rep == oidUnknownUser || rep == oidDecryptError {
			return PDU{}, fmt.Errorf("%w (%s)", ErrAuth, rep)
		}
		return PDU{}, fmt.Errorf("snmp: agent report %s", reportOID(resp))
	}
	return resp, nil
}

func reportOID(p PDU) string {
	if len(p.VarBinds) > 0 {
		return p.VarBinds[0].OID
	}
	return ""
}

// discover learns the agent's engine id, boots and time.
func (c *Client) discover() error {
	c.engineID, c.boots, c.etime = nil, 0, 0
	msgID := c.nextReqID()
	usm := encodeSequence(encodeOctets(nil), encodeInt(tagInteger, 0), encodeInt(tagInteger, 0), encodeOctets(nil), encodeOctets(nil), encodeOctets(nil))
	scoped := encodeSequence(encodeOctets(nil), encodeOctets(nil), mustPDU(PDU{Type: pduGetRequest, RequestID: msgID}))
	msg := encodeSequence(encodeInt(tagInteger, 3), c.globalData(msgID, 0x04), encodeOctets(usm), scoped)
	raw, err := c.roundTrip(msg, func(b []byte) (bool, error) {
		m, err := parseV3(b)
		return err == nil && m.msgID == msgID, nil
	})
	if err != nil {
		return err
	}
	m, err := parseV3(raw)
	if err != nil {
		return err
	}
	if len(m.engineID) == 0 {
		return errors.New("snmp: discovery returned no engine id")
	}
	c.engineID, c.boots, c.etime, c.syncedAt = m.engineID, m.boots, m.etime, time.Now()
	c.prepareKeys()
	return nil
}

func mustPDU(p PDU) []byte {
	b, _ := EncodePDU(p)
	return b
}

func (c *Client) globalData(msgID int32, flags byte) []byte {
	return encodeSequence(encodeInt(tagInteger, int64(msgID)), encodeInt(tagInteger, 65507), encodeOctets([]byte{flags}), encodeInt(tagInteger, 3))
}

func (c *Client) curTime() int32 {
	if c.syncedAt.IsZero() {
		return c.etime
	}
	return c.etime + int32(time.Since(c.syncedAt).Seconds())
}

// sendV3 builds, signs, encrypts and sends one request; it also absorbs
// time-window reports by resynchronising.
// buildV3 encodes one authenticated/encrypted v3 message for the client's
// current engine parameters.
func (c *Client) buildV3(msgID int32, flags byte, pdu PDU) ([]byte, error) {
	ctxEngine := c.engineID
	if c.ctxEngineID != nil {
		ctxEngine = c.ctxEngineID // responses echo the requester's context engine
	}
	scoped := encodeSequence(encodeOctets(ctxEngine), encodeOctets([]byte(c.ContextName)), mustPDU(pdu))
	var privParams []byte
	var payload []byte
	if flags&0x02 != 0 {
		enc, salt, err := c.encrypt(scoped)
		if err != nil {
			return nil, err
		}
		privParams = salt
		payload = encodeOctets(enc)
	} else {
		payload = scoped
	}
	authLen := 0
	if flags&0x01 != 0 {
		authLen = c.authLen()
	}
	placeholder := make([]byte, authLen)
	if authLen > 0 {
		if _, err := rand.Read(placeholder); err != nil {
			return nil, err
		}
	}
	usm := encodeSequence(encodeOctets(c.engineID), encodeInt(tagInteger, int64(c.boots)), encodeInt(tagInteger, int64(c.curTime())),
		encodeOctets([]byte(c.User)), encodeOctets(placeholder), encodeOctets(privParams))
	msg := encodeSequence(encodeInt(tagInteger, 3), c.globalData(msgID, flags), encodeOctets(usm), payload)
	if authLen > 0 {
		idx := bytes.Index(msg, placeholder)
		if idx < 0 {
			return nil, errors.New("snmp: internal auth placeholder")
		}
		for i := 0; i < authLen; i++ {
			msg[idx+i] = 0
		}
		mac := c.mac(msg)
		copy(msg[idx:], mac[:authLen])
	}
	return msg, nil
}

func (c *Client) sendV3(pdu PDU, allowResync bool) (PDU, error) {
	msgID := c.nextReqID()
	authLen := c.authLen()
	msg, err := c.buildV3(msgID, c.flags(), pdu)
	if err != nil {
		return PDU{}, err
	}
	raw, err := c.roundTrip(msg, func(b []byte) (bool, error) {
		m, err := parseV3(b)
		return err == nil && m.msgID == msgID, nil
	})
	if err != nil {
		return PDU{}, err
	}
	m, err := parseV3(raw)
	if err != nil {
		return PDU{}, err
	}
	// Reports come unauthenticated when the agent could not authenticate us.
	if m.flags&0x01 != 0 && authLen > 0 {
		if !c.verify(raw, m) {
			return PDU{}, fmt.Errorf("%w: response digest mismatch", ErrAuth)
		}
	}
	var scopedResp []byte
	if m.flags&0x02 != 0 {
		dec, err := c.decrypt(m.encrypted, m.boots, m.etime, m.privParams)
		if err != nil {
			return PDU{}, err
		}
		scopedResp = dec
	} else {
		scopedResp = m.scoped
	}
	resp, err := parseScoped(scopedResp)
	if err != nil {
		return PDU{}, err
	}
	if resp.Type == pduReport && allowResync {
		rep := reportOID(resp)
		if rep == oidNotInTimeWindow || rep == oidUnknownEngineID {
			if len(m.engineID) > 0 && !bytes.Equal(m.engineID, c.engineID) {
				c.engineID = m.engineID
				c.prepareKeys()
			}
			c.boots, c.etime, c.syncedAt = m.boots, m.etime, time.Now()
		}
	}
	return resp, nil
}

func (c *Client) mac(msg []byte) []byte {
	h := hmac.New(c.hashNew(), c.authKey)
	h.Write(msg)
	return h.Sum(nil)
}

func (c *Client) verify(raw []byte, m v3msg) bool {
	if m.authOffset < 0 || len(m.authParams) != c.authLen() {
		return false
	}
	cp := append([]byte(nil), raw...)
	for i := 0; i < len(m.authParams); i++ {
		cp[m.authOffset+i] = 0
	}
	mac := c.mac(cp)
	return hmac.Equal(mac[:len(m.authParams)], m.authParams)
}

func (c *Client) nextSalt() []byte {
	c.salt++
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, c.salt)
	return b
}

func (c *Client) encrypt(plain []byte) (cipherText, salt []byte, err error) {
	switch strings.ToLower(c.PrivProto) {
	case "aes", "aes128", "aes-128":
		if len(c.privKey) < 16 {
			return nil, nil, errors.New("snmp: aes key too short")
		}
		block, err := aes.NewCipher(c.privKey[:16])
		if err != nil {
			return nil, nil, err
		}
		salt = c.nextSalt()
		iv := make([]byte, 16)
		binary.BigEndian.PutUint32(iv[0:], uint32(c.boots))
		binary.BigEndian.PutUint32(iv[4:], uint32(c.curTime()))
		copy(iv[8:], salt)
		out := make([]byte, len(plain))
		cipher.NewCFBEncrypter(block, iv).XORKeyStream(out, plain)
		return out, salt, nil
	case "des":
		if len(c.privKey) < 16 {
			return nil, nil, errors.New("snmp: des key too short")
		}
		block, err := des.NewCipher(c.privKey[:8])
		if err != nil {
			return nil, nil, err
		}
		preIV := c.privKey[8:16]
		salt = make([]byte, 8)
		binary.BigEndian.PutUint32(salt[0:], uint32(c.boots))
		c.salt++
		binary.BigEndian.PutUint32(salt[4:], uint32(c.salt))
		iv := make([]byte, 8)
		for i := range iv {
			iv[i] = preIV[i] ^ salt[i]
		}
		pad := 8 - len(plain)%8
		padded := append(append([]byte(nil), plain...), make([]byte, pad)...)
		out := make([]byte, len(padded))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
		return out, salt, nil
	}
	return nil, nil, fmt.Errorf("snmp: unsupported privacy protocol %q", c.PrivProto)
}

func (c *Client) decrypt(data []byte, boots, etime int32, salt []byte) ([]byte, error) {
	switch strings.ToLower(c.PrivProto) {
	case "aes", "aes128", "aes-128":
		block, err := aes.NewCipher(c.privKey[:16])
		if err != nil {
			return nil, err
		}
		iv := make([]byte, 16)
		binary.BigEndian.PutUint32(iv[0:], uint32(boots))
		binary.BigEndian.PutUint32(iv[4:], uint32(etime))
		copy(iv[8:], salt)
		out := make([]byte, len(data))
		cipher.NewCFBDecrypter(block, iv).XORKeyStream(out, data)
		return out, nil
	case "des":
		block, err := des.NewCipher(c.privKey[:8])
		if err != nil {
			return nil, err
		}
		if len(data)%8 != 0 || len(salt) != 8 {
			return nil, errors.New("snmp: bad des payload")
		}
		iv := make([]byte, 8)
		for i := range iv {
			iv[i] = c.privKey[8+i] ^ salt[i]
		}
		out := make([]byte, len(data))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
		return out, nil
	}
	return nil, errors.New("snmp: unsupported privacy protocol")
}

type v3msg struct {
	msgID      int32
	flags      byte
	engineID   []byte
	boots      int32
	etime      int32
	user       string
	authParams []byte
	authOffset int
	privParams []byte
	scoped     []byte // plaintext scoped pdu (SEQ tlv)
	encrypted  []byte
}

func parseV3(b []byte) (v3msg, error) {
	var m v3msg
	m.authOffset = -1
	r := &reader{b: b}
	tag, seq, err := r.tlv()
	if err != nil || tag != tagSequence {
		return m, errors.New("snmp: not a message")
	}
	base := r.pos - len(seq) // offset of seq content within b
	rr := &reader{b: seq}
	t, c, err := rr.tlv()
	if err != nil || t != tagInteger {
		return m, errors.New("snmp: version")
	}
	if v, _ := decodeInt(c); v != 3 {
		return m, fmt.Errorf("snmp: version %d", v)
	}
	t, g, err := rr.tlv()
	if err != nil || t != tagSequence {
		return m, errors.New("snmp: global data")
	}
	gr := &reader{b: g}
	if t, c, err = gr.tlv(); err != nil || t != tagInteger {
		return m, errors.New("snmp: msgID")
	}
	id, _ := decodeInt(c)
	m.msgID = int32(id)
	if _, _, err = gr.tlv(); err != nil {
		return m, err
	}
	if t, c, err = gr.tlv(); err != nil || t != tagOctetString || len(c) != 1 {
		return m, errors.New("snmp: flags")
	}
	m.flags = c[0]
	if _, _, err = gr.tlv(); err != nil {
		return m, err
	}
	// security parameters
	t, sp, err := rr.tlv()
	if err != nil || t != tagOctetString {
		return m, errors.New("snmp: security params")
	}
	spOffset := base + (rr.pos - len(sp)) // offset of sp content in b
	sr := &reader{b: sp}
	t, usm, err := sr.tlv()
	if err != nil || t != tagSequence {
		return m, errors.New("snmp: usm")
	}
	usmOffset := spOffset + (sr.pos - len(usm))
	ur := &reader{b: usm}
	if t, c, err = ur.tlv(); err != nil || t != tagOctetString {
		return m, errors.New("snmp: engine id")
	}
	m.engineID = append([]byte(nil), c...)
	if t, c, err = ur.tlv(); err != nil || t != tagInteger {
		return m, errors.New("snmp: boots")
	}
	bo, _ := decodeInt(c)
	m.boots = int32(bo)
	if t, c, err = ur.tlv(); err != nil || t != tagInteger {
		return m, errors.New("snmp: time")
	}
	et, _ := decodeInt(c)
	m.etime = int32(et)
	if t, c, err = ur.tlv(); err != nil || t != tagOctetString {
		return m, errors.New("snmp: user")
	}
	m.user = string(c)
	if t, c, err = ur.tlv(); err != nil || t != tagOctetString {
		return m, errors.New("snmp: auth params")
	}
	m.authParams = append([]byte(nil), c...)
	m.authOffset = usmOffset + (ur.pos - len(c))
	if t, c, err = ur.tlv(); err != nil || t != tagOctetString {
		return m, errors.New("snmp: priv params")
	}
	m.privParams = append([]byte(nil), c...)
	// payload
	start := rr.pos
	t, c, err = rr.tlv()
	if err != nil {
		return m, err
	}
	switch t {
	case tagOctetString:
		m.encrypted = append([]byte(nil), c...)
	case tagSequence:
		m.scoped = append([]byte(nil), seq[start:rr.pos]...)
	default:
		return m, errors.New("snmp: payload")
	}
	return m, nil
}

func parseScoped(b []byte) (PDU, error) {
	p, _, _, err := parseScopedFull(b)
	return p, err
}

// parseScopedFull also returns the context engine id and name.
func parseScopedFull(b []byte) (PDU, []byte, string, error) {
	r := &reader{b: b}
	tag, seq, err := r.tlv()
	if err != nil || tag != tagSequence {
		return PDU{}, nil, "", errors.New("snmp: scoped pdu")
	}
	rr := &reader{b: seq}
	t, eng, err := rr.tlv()
	if err != nil || t != tagOctetString {
		return PDU{}, nil, "", errors.New("snmp: context engine")
	}
	t, name, err := rr.tlv()
	if err != nil || t != tagOctetString {
		return PDU{}, nil, "", errors.New("snmp: context name")
	}
	t, c, err := rr.tlv()
	if err != nil {
		return PDU{}, nil, "", err
	}
	p, err := DecodePDU(t, c)
	return p, append([]byte(nil), eng...), string(name), err
}
