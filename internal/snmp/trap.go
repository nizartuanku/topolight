package snmp

import (
	"errors"
	"fmt"
	"net"
	"strings"
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
