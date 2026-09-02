// Package snmp is a dependency-free SNMP v2c / v3 (USM) client and PDU codec
// built on the Go standard library. It implements exactly what a monitoring
// poller needs: GET, GETNEXT, GETBULK, walks, engine discovery, HMAC-MD5/SHA1/
// SHA-256 authentication, DES/AES-128 privacy, and v2c trap decoding.
package snmp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ASN.1 / SNMP tags.
const (
	tagInteger        byte = 0x02
	tagOctetString    byte = 0x04
	tagNull           byte = 0x05
	tagOID            byte = 0x06
	tagSequence       byte = 0x30
	tagIPAddress      byte = 0x40
	tagCounter32      byte = 0x41
	tagGauge32        byte = 0x42
	tagTimeTicks      byte = 0x43
	tagOpaque         byte = 0x44
	tagCounter64      byte = 0x46
	tagNoSuchObject   byte = 0x80
	tagNoSuchInstance byte = 0x81
	tagEndOfMibView   byte = 0x82

	pduGetRequest  byte = 0xA0
	pduGetNext     byte = 0xA1
	pduGetResponse byte = 0xA2
	pduSetRequest  byte = 0xA3
	pduGetBulk     byte = 0xA5
	pduInform      byte = 0xA6
	pduTrapV2      byte = 0xA7
	pduReport      byte = 0xA8
)

// Kind is the value type of a variable binding.
type Kind byte

// Value kinds.
const (
	KindInteger Kind = iota
	KindOctetString
	KindNull
	KindOID
	KindIPAddress
	KindCounter32
	KindGauge32
	KindTimeTicks
	KindOpaque
	KindCounter64
	KindNoSuchObject
	KindNoSuchInstance
	KindEndOfMibView
)

// Value is a decoded SNMP value.
type Value struct {
	Kind  Kind
	Int   int64  // integers, counters, gauges, ticks (Counter64 as int64)
	Uint  uint64 // Counter64 exact
	Bytes []byte // octet strings, opaque, ip address (4 bytes)
	OID   string // for KindOID
}

// String renders a readable form.
func (v Value) String() string {
	switch v.Kind {
	case KindInteger, KindCounter32, KindGauge32, KindTimeTicks:
		return strconv.FormatInt(v.Int, 10)
	case KindCounter64:
		return strconv.FormatUint(v.Uint, 10)
	case KindOctetString, KindOpaque:
		return string(v.Bytes)
	case KindIPAddress:
		if len(v.Bytes) == 4 {
			return fmt.Sprintf("%d.%d.%d.%d", v.Bytes[0], v.Bytes[1], v.Bytes[2], v.Bytes[3])
		}
		return ""
	case KindOID:
		return v.OID
	case KindNoSuchObject:
		return "noSuchObject"
	case KindNoSuchInstance:
		return "noSuchInstance"
	case KindEndOfMibView:
		return "endOfMibView"
	}
	return ""
}

// IsNumber reports whether Int carries a numeric value.
func (v Value) IsNumber() bool {
	switch v.Kind {
	case KindInteger, KindCounter32, KindGauge32, KindTimeTicks, KindCounter64:
		return true
	}
	return false
}

// Float returns the numeric value as float64 (0 for non-numbers).
func (v Value) Float() float64 {
	if v.Kind == KindCounter64 {
		return float64(v.Uint)
	}
	if v.IsNumber() {
		return float64(v.Int)
	}
	return 0
}

// Exception reports NoSuchObject/NoSuchInstance/EndOfMibView.
func (v Value) Exception() bool {
	return v.Kind == KindNoSuchObject || v.Kind == KindNoSuchInstance || v.Kind == KindEndOfMibView
}

// VarBind pairs an OID with a value.
type VarBind struct {
	OID   string
	Value Value
}

// PDU is a decoded protocol data unit.
type PDU struct {
	Type       byte
	RequestID  int32
	ErrorCode  int
	ErrorIndex int
	// GetBulk only
	NonRepeaters   int
	MaxRepetitions int
	VarBinds       []VarBind
}

// ---- encoding ----

func encodeLength(n int) []byte {
	if n < 128 {
		return []byte{byte(n)}
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte(n & 0xff)}, b...)
		n >>= 8
	}
	return append([]byte{0x80 | byte(len(b))}, b...)
}

func encodeTLV(tag byte, content []byte) []byte {
	out := make([]byte, 0, 2+len(content)+4)
	out = append(out, tag)
	out = append(out, encodeLength(len(content))...)
	return append(out, content...)
}

func encodeInt(tag byte, v int64) []byte {
	// two's complement, minimal length
	var b []byte
	switch {
	case v == 0:
		b = []byte{0}
	case v > 0:
		for v > 0 {
			b = append([]byte{byte(v & 0xff)}, b...)
			v >>= 8
		}
		if b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
	default:
		for v < -1 {
			b = append([]byte{byte(v & 0xff)}, b...)
			v >>= 8
		}
		b = append([]byte{byte(v & 0xff)}, b...) // v == -1
		if b[0]&0x80 == 0 {
			b = append([]byte{0xff}, b...)
		}
	}
	return encodeTLV(tag, b)
}

func encodeUint(tag byte, v uint64) []byte {
	var b []byte
	if v == 0 {
		b = []byte{0}
	}
	for v > 0 {
		b = append([]byte{byte(v & 0xff)}, b...)
		v >>= 8
	}
	if b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return encodeTLV(tag, b)
}

func encodeOctets(b []byte) []byte { return encodeTLV(tagOctetString, b) }

func encodeNull() []byte { return []byte{tagNull, 0} }

func encodeSequence(parts ...[]byte) []byte {
	var content []byte
	for _, p := range parts {
		content = append(content, p...)
	}
	return encodeTLV(tagSequence, content)
}

func encodeSubID(n int) []byte {
	if n < 128 {
		return []byte{byte(n)}
	}
	var b []byte
	b = append(b, byte(n&0x7f))
	n >>= 7
	for n > 0 {
		b = append([]byte{byte(n&0x7f) | 0x80}, b...)
		n >>= 7
	}
	return b
}

// EncodeOID encodes a dotted OID string.
func EncodeOID(oid string) ([]byte, error) {
	ids, err := ParseOID(oid)
	if err != nil {
		return nil, err
	}
	if len(ids) < 2 {
		return nil, errors.New("oid too short")
	}
	var content []byte
	content = append(content, byte(ids[0]*40+ids[1]))
	for _, id := range ids[2:] {
		content = append(content, encodeSubID(id)...)
	}
	return encodeTLV(tagOID, content), nil
}

// ParseOID parses "1.3.6.1..." (leading dot tolerated) into sub-ids.
func ParseOID(s string) ([]int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), ".")
	if s == "" {
		return nil, errors.New("empty oid")
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("bad oid %q", s)
		}
		out = append(out, n)
	}
	return out, nil
}

func encodeValue(v Value) ([]byte, error) {
	switch v.Kind {
	case KindInteger:
		return encodeInt(tagInteger, v.Int), nil
	case KindOctetString:
		return encodeOctets(v.Bytes), nil
	case KindNull:
		return encodeNull(), nil
	case KindOID:
		return EncodeOID(v.OID)
	case KindIPAddress:
		return encodeTLV(tagIPAddress, v.Bytes), nil
	case KindCounter32:
		return encodeUint(tagCounter32, uint64(v.Int)), nil
	case KindGauge32:
		return encodeUint(tagGauge32, uint64(v.Int)), nil
	case KindTimeTicks:
		return encodeUint(tagTimeTicks, uint64(v.Int)), nil
	case KindOpaque:
		return encodeTLV(tagOpaque, v.Bytes), nil
	case KindCounter64:
		return encodeUint(tagCounter64, v.Uint), nil
	}
	return encodeNull(), nil
}

func encodeVarBinds(vbs []VarBind) ([]byte, error) {
	var content []byte
	for _, vb := range vbs {
		oid, err := EncodeOID(vb.OID)
		if err != nil {
			return nil, err
		}
		val, err := encodeValue(vb.Value)
		if err != nil {
			return nil, err
		}
		content = append(content, encodeSequence(oid, val)...)
	}
	return encodeTLV(tagSequence, content), nil
}

// EncodePDU encodes a PDU (the A0..A8 block).
func EncodePDU(p PDU) ([]byte, error) {
	vbs, err := encodeVarBinds(p.VarBinds)
	if err != nil {
		return nil, err
	}
	var a, b []byte
	if p.Type == pduGetBulk {
		a = encodeInt(tagInteger, int64(p.NonRepeaters))
		b = encodeInt(tagInteger, int64(p.MaxRepetitions))
	} else {
		a = encodeInt(tagInteger, int64(p.ErrorCode))
		b = encodeInt(tagInteger, int64(p.ErrorIndex))
	}
	content := append([]byte{}, encodeInt(tagInteger, int64(p.RequestID))...)
	content = append(content, a...)
	content = append(content, b...)
	content = append(content, vbs...)
	return encodeTLV(p.Type, content), nil
}

// ---- decoding ----

type reader struct {
	b   []byte
	pos int
}

func (r *reader) tlv() (tag byte, content []byte, err error) {
	if r.pos >= len(r.b) {
		return 0, nil, errors.New("ber: unexpected end")
	}
	tag = r.b[r.pos]
	r.pos++
	if r.pos >= len(r.b) {
		return 0, nil, errors.New("ber: truncated length")
	}
	l := int(r.b[r.pos])
	r.pos++
	if l&0x80 != 0 {
		n := l & 0x7f
		if n == 0 || n > 4 || r.pos+n > len(r.b) {
			return 0, nil, errors.New("ber: bad length")
		}
		l = 0
		for i := 0; i < n; i++ {
			l = l<<8 | int(r.b[r.pos])
			r.pos++
		}
	}
	if l < 0 || r.pos+l > len(r.b) {
		return 0, nil, errors.New("ber: length beyond buffer")
	}
	content = r.b[r.pos : r.pos+l]
	r.pos += l
	return tag, content, nil
}

func decodeInt(b []byte) (int64, error) {
	if len(b) == 0 || len(b) > 9 {
		return 0, errors.New("ber: bad integer")
	}
	var v int64
	if b[0]&0x80 != 0 {
		v = -1
	}
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	return v, nil
}

func decodeUint(b []byte) (uint64, error) {
	if len(b) == 0 || len(b) > 9 {
		return 0, errors.New("ber: bad unsigned")
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

func decodeOID(b []byte) (string, error) {
	if len(b) == 0 {
		return "", errors.New("ber: empty oid")
	}
	var sb strings.Builder
	first := int(b[0])
	sb.WriteString(strconv.Itoa(first / 40))
	sb.WriteByte('.')
	sb.WriteString(strconv.Itoa(first % 40))
	var cur int
	for _, c := range b[1:] {
		cur = cur<<7 | int(c&0x7f)
		if c&0x80 == 0 {
			sb.WriteByte('.')
			sb.WriteString(strconv.Itoa(cur))
			cur = 0
		}
	}
	return sb.String(), nil
}

func decodeValue(tag byte, content []byte) (Value, error) {
	switch tag {
	case tagInteger:
		n, err := decodeInt(content)
		return Value{Kind: KindInteger, Int: n}, err
	case tagOctetString:
		return Value{Kind: KindOctetString, Bytes: append([]byte(nil), content...)}, nil
	case tagNull:
		return Value{Kind: KindNull}, nil
	case tagOID:
		s, err := decodeOID(content)
		return Value{Kind: KindOID, OID: s}, err
	case tagIPAddress:
		return Value{Kind: KindIPAddress, Bytes: append([]byte(nil), content...)}, nil
	case tagCounter32:
		n, err := decodeUint(content)
		return Value{Kind: KindCounter32, Int: int64(uint32(n))}, err
	case tagGauge32:
		n, err := decodeUint(content)
		return Value{Kind: KindGauge32, Int: int64(uint32(n))}, err
	case tagTimeTicks:
		n, err := decodeUint(content)
		return Value{Kind: KindTimeTicks, Int: int64(uint32(n))}, err
	case tagOpaque:
		return Value{Kind: KindOpaque, Bytes: append([]byte(nil), content...)}, nil
	case tagCounter64:
		n, err := decodeUint(content)
		return Value{Kind: KindCounter64, Uint: n, Int: int64(n)}, err
	case tagNoSuchObject:
		return Value{Kind: KindNoSuchObject}, nil
	case tagNoSuchInstance:
		return Value{Kind: KindNoSuchInstance}, nil
	case tagEndOfMibView:
		return Value{Kind: KindEndOfMibView}, nil
	}
	return Value{}, fmt.Errorf("ber: unsupported value tag 0x%02x", tag)
}

func decodeVarBinds(b []byte) ([]VarBind, error) {
	r := &reader{b: b}
	var out []VarBind
	for r.pos < len(r.b) {
		tag, seq, err := r.tlv()
		if err != nil {
			return nil, err
		}
		if tag != tagSequence {
			return nil, errors.New("ber: varbind not a sequence")
		}
		rr := &reader{b: seq}
		t1, c1, err := rr.tlv()
		if err != nil || t1 != tagOID {
			return nil, errors.New("ber: varbind oid")
		}
		oid, err := decodeOID(c1)
		if err != nil {
			return nil, err
		}
		t2, c2, err := rr.tlv()
		if err != nil {
			return nil, err
		}
		v, err := decodeValue(t2, c2)
		if err != nil {
			return nil, err
		}
		out = append(out, VarBind{OID: oid, Value: v})
	}
	return out, nil
}

// DecodePDU decodes an A0..A8 block.
func DecodePDU(tag byte, content []byte) (PDU, error) {
	p := PDU{Type: tag}
	r := &reader{b: content}
	t, c, err := r.tlv()
	if err != nil || t != tagInteger {
		return p, errors.New("ber: pdu request-id")
	}
	rid, _ := decodeInt(c)
	p.RequestID = int32(rid)
	t, c, err = r.tlv()
	if err != nil || t != tagInteger {
		return p, errors.New("ber: pdu error-status")
	}
	a, _ := decodeInt(c)
	t, c, err = r.tlv()
	if err != nil || t != tagInteger {
		return p, errors.New("ber: pdu error-index")
	}
	b, _ := decodeInt(c)
	if tag == pduGetBulk {
		p.NonRepeaters, p.MaxRepetitions = int(a), int(b)
	} else {
		p.ErrorCode, p.ErrorIndex = int(a), int(b)
	}
	t, c, err = r.tlv()
	if err != nil || t != tagSequence {
		return p, errors.New("ber: pdu varbinds")
	}
	p.VarBinds, err = decodeVarBinds(c)
	return p, err
}

// OIDHasPrefix reports whether oid is prefix or lies beneath it.
func OIDHasPrefix(oid, prefix string) bool {
	oid, prefix = strings.TrimPrefix(oid, "."), strings.TrimPrefix(prefix, ".")
	return oid == prefix || strings.HasPrefix(oid, prefix+".")
}

// OIDSuffix returns the part of oid after prefix (without leading dot).
func OIDSuffix(oid, prefix string) string {
	oid, prefix = strings.TrimPrefix(oid, "."), strings.TrimPrefix(prefix, ".")
	return strings.TrimPrefix(strings.TrimPrefix(oid, prefix), ".")
}
