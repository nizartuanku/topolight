// Package flow collects NetFlow v5/v9, IPFIX and sFlow v5 exports and turns
// them into per-exporter traffic summaries: who talks to whom, which
// applications, how much per interface. Raw flows are never stored — only
// bounded top-N aggregates per 5-minute bucket — so disk stays predictable.
package flow

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"
	"time"
)

// Record is one normalised flow (or sampled packet) from any protocol.
type Record struct {
	Src, Dst         netip.Addr
	SrcPort, DstPort uint16
	Proto            uint8
	InIf, OutIf      uint32
	Bytes, Packets   uint64 // already multiplied by the sampling rate
}

// templateKey identifies a NetFlow v9 / IPFIX template.
type templateKey struct {
	exporter string
	domain   uint32
	id       uint16
}

type field struct {
	typ        uint16
	length     uint16
	enterprise uint32
}

type template struct {
	fields []field
	size   int // total record length, 0 when a field is variable-length
	seen   time.Time
}

// Parser keeps the template cache shared by every exporter.
type Parser struct {
	mu        sync.Mutex
	templates map[templateKey]*template
	// Stats
	Datagrams, Records, NoTemplate, Malformed int64
}

// NewParser creates an empty template cache.
func NewParser() *Parser { return &Parser{templates: map[templateKey]*template{}} }

var errShort = errors.New("flow: datagram too short")

// Parse decodes one UDP datagram from exporter and returns the flows in it.
// The protocol is chosen by the leading version field: 5, 9, 10 (IPFIX) or
// sFlow v5 (also "5" — disambiguated by port in the collector, see ParseSFlow).
func (p *Parser) Parse(exporter string, b []byte, now time.Time) ([]Record, error) {
	p.mu.Lock()
	p.Datagrams++
	p.mu.Unlock()
	if len(b) < 4 {
		return nil, errShort
	}
	switch binary.BigEndian.Uint16(b) {
	case 5:
		return p.parseV5(b)
	case 9:
		return p.parseV9(exporter, b, now)
	case 10:
		return p.parseIPFIX(exporter, b, now)
	}
	p.bad()
	return nil, errors.New("flow: unknown NetFlow version")
}

func (p *Parser) bad() { p.mu.Lock(); p.Malformed++; p.mu.Unlock() }

// ---- NetFlow v5 ---------------------------------------------------------------

func (p *Parser) parseV5(b []byte) ([]Record, error) {
	if len(b) < 24 {
		p.bad()
		return nil, errShort
	}
	count := int(binary.BigEndian.Uint16(b[2:]))
	// sampling: 2-bit mode + 14-bit interval (0 = not sampled)
	si := binary.BigEndian.Uint16(b[22:])
	rate := uint64(si & 0x3fff)
	if (si>>14) == 0 || rate == 0 {
		rate = 1
	}
	if len(b) < 24+count*48 {
		p.bad()
		return nil, errShort
	}
	out := make([]Record, 0, count)
	for i := 0; i < count; i++ {
		r := b[24+i*48:]
		rec := Record{
			Src:     netip.AddrFrom4([4]byte{r[0], r[1], r[2], r[3]}),
			Dst:     netip.AddrFrom4([4]byte{r[4], r[5], r[6], r[7]}),
			InIf:    uint32(binary.BigEndian.Uint16(r[12:])),
			OutIf:   uint32(binary.BigEndian.Uint16(r[14:])),
			Packets: uint64(binary.BigEndian.Uint32(r[16:])) * rate,
			Bytes:   uint64(binary.BigEndian.Uint32(r[20:])) * rate,
			SrcPort: binary.BigEndian.Uint16(r[32:]),
			DstPort: binary.BigEndian.Uint16(r[34:]),
			Proto:   r[38],
		}
		out = append(out, rec)
	}
	p.mu.Lock()
	p.Records += int64(len(out))
	p.mu.Unlock()
	return out, nil
}

// ---- NetFlow v9 and IPFIX -----------------------------------------------------

// Information element ids shared by v9 and IPFIX (IANA ipfix registry).
const (
	ieInBytes             = 1
	ieInPkts              = 2
	ieProtocol            = 4
	ieSrcPort             = 7
	ieIPv4Src             = 8
	ieInputSNMP           = 10
	ieDstPort             = 11
	ieIPv4Dst             = 12
	ieOutputSNMP          = 14
	ieOutBytes            = 23
	ieOutPkts             = 24
	ieIPv6Src             = 27
	ieIPv6Dst             = 28
	ieSamplingIntv        = 34
	ieSamplerRate         = 49 // NetFlow v9 SAMPLER_INTERVAL... (rarely in data records)
	ieOctetDelta          = 1
	iePacketDelta         = 2
	ieFlowSampler         = 48
	ieSamplingPktInterval = 305
)

func (p *Parser) parseV9(exporter string, b []byte, now time.Time) ([]Record, error) {
	if len(b) < 20 {
		p.bad()
		return nil, errShort
	}
	domain := binary.BigEndian.Uint32(b[16:])
	return p.parseSets(exporter, domain, b[20:], now, false)
}

func (p *Parser) parseIPFIX(exporter string, b []byte, now time.Time) ([]Record, error) {
	if len(b) < 16 {
		p.bad()
		return nil, errShort
	}
	length := int(binary.BigEndian.Uint16(b[2:]))
	if length > len(b) {
		length = len(b)
	}
	domain := binary.BigEndian.Uint32(b[12:])
	return p.parseSets(exporter, domain, b[16:length], now, true)
}

// parseSets walks the flowsets/sets of a v9 or IPFIX message.
func (p *Parser) parseSets(exporter string, domain uint32, b []byte, now time.Time, ipfix bool) ([]Record, error) {
	var out []Record
	for len(b) >= 4 {
		id := binary.BigEndian.Uint16(b)
		l := int(binary.BigEndian.Uint16(b[2:]))
		if l < 4 || l > len(b) {
			p.bad()
			break
		}
		body := b[4:l]
		switch {
		case (!ipfix && id == 0) || (ipfix && id == 2):
			p.readTemplates(exporter, domain, body, now, ipfix)
		case (!ipfix && id == 1) || (ipfix && id == 3):
			// options templates: not needed (sampling comes from data or config)
		case id >= 256:
			recs, ok := p.decodeData(exporter, domain, id, body)
			if !ok {
				p.mu.Lock()
				p.NoTemplate++
				p.mu.Unlock()
			}
			out = append(out, recs...)
		}
		b = b[l:]
	}
	p.mu.Lock()
	p.Records += int64(len(out))
	p.mu.Unlock()
	return out, nil
}

func (p *Parser) readTemplates(exporter string, domain uint32, b []byte, now time.Time, ipfix bool) {
	for len(b) >= 4 {
		tid := binary.BigEndian.Uint16(b)
		n := int(binary.BigEndian.Uint16(b[2:]))
		b = b[4:]
		t := &template{seen: now}
		ok := true
		for i := 0; i < n; i++ {
			if len(b) < 4 {
				ok = false
				break
			}
			f := field{typ: binary.BigEndian.Uint16(b), length: binary.BigEndian.Uint16(b[2:])}
			b = b[4:]
			if ipfix && f.typ&0x8000 != 0 {
				if len(b) < 4 {
					ok = false
					break
				}
				f.enterprise = binary.BigEndian.Uint32(b)
				f.typ &^= 0x8000
				b = b[4:]
			}
			if f.length == 0xffff {
				t.size = -1 // variable length: unsupported, records skipped
			} else if t.size >= 0 {
				t.size += int(f.length)
			}
			t.fields = append(t.fields, f)
		}
		if !ok || n == 0 {
			p.bad()
			return
		}
		p.mu.Lock()
		p.templates[templateKey{exporter, domain, tid}] = t
		// forget templates not refreshed for an hour
		for k, v := range p.templates {
			if now.Sub(v.seen) > time.Hour {
				delete(p.templates, k)
			}
		}
		p.mu.Unlock()
	}
}

func (p *Parser) decodeData(exporter string, domain uint32, tid uint16, b []byte) ([]Record, bool) {
	p.mu.Lock()
	t := p.templates[templateKey{exporter, domain, tid}]
	p.mu.Unlock()
	if t == nil || t.size <= 0 {
		return nil, t != nil
	}
	var out []Record
	for len(b) >= t.size {
		r := b[:t.size]
		b = b[t.size:]
		var rec Record
		var outBytes, outPkts uint64
		rate := uint64(1)
		off := 0
		for _, f := range t.fields {
			v := r[off : off+int(f.length)]
			off += int(f.length)
			if f.enterprise != 0 {
				continue
			}
			switch f.typ {
			case ieInBytes:
				rec.Bytes = uintN(v)
			case ieInPkts:
				rec.Packets = uintN(v)
			case ieOutBytes:
				outBytes = uintN(v)
			case ieOutPkts:
				outPkts = uintN(v)
			case ieProtocol:
				rec.Proto = uint8(uintN(v))
			case ieSrcPort:
				rec.SrcPort = uint16(uintN(v))
			case ieDstPort:
				rec.DstPort = uint16(uintN(v))
			case ieIPv4Src:
				if len(v) == 4 {
					rec.Src = netip.AddrFrom4([4]byte(v))
				}
			case ieIPv4Dst:
				if len(v) == 4 {
					rec.Dst = netip.AddrFrom4([4]byte(v))
				}
			case ieIPv6Src:
				if len(v) == 16 {
					rec.Src = netip.AddrFrom16([16]byte(v))
				}
			case ieIPv6Dst:
				if len(v) == 16 {
					rec.Dst = netip.AddrFrom16([16]byte(v))
				}
			case ieInputSNMP:
				rec.InIf = uint32(uintN(v))
			case ieOutputSNMP:
				rec.OutIf = uint32(uintN(v))
			case ieSamplingIntv, ieSamplingPktInterval:
				if x := uintN(v); x > 1 {
					rate = x
				}
			}
		}
		// some exporters report only OUT_* (egress accounting)
		if rec.Bytes == 0 && outBytes > 0 {
			rec.Bytes, rec.Packets = outBytes, outPkts
		}
		if !rec.Src.IsValid() || !rec.Dst.IsValid() {
			continue
		}
		rec.Bytes *= rate
		rec.Packets *= rate
		out = append(out, rec)
	}
	return out, true
}

func uintN(v []byte) uint64 {
	var x uint64
	for _, c := range v {
		x = x<<8 | uint64(c)
	}
	return x
}

// ---- sFlow v5 -----------------------------------------------------------------

// ParseSFlow decodes an sFlow v5 datagram: every flow sample with a raw
// packet header becomes one Record scaled by the sampling rate. Counter
// samples are ignored (SNMP already gives us interface counters).
func (p *Parser) ParseSFlow(b []byte) ([]Record, error) {
	p.mu.Lock()
	p.Datagrams++
	p.mu.Unlock()
	if len(b) < 28 || binary.BigEndian.Uint32(b) != 5 {
		p.bad()
		return nil, errors.New("flow: not sFlow v5")
	}
	off := 4
	addrType := binary.BigEndian.Uint32(b[off:])
	off += 4
	if addrType == 1 {
		off += 4
	} else if addrType == 2 {
		off += 16
	} else {
		p.bad()
		return nil, errors.New("flow: bad agent address type")
	}
	if len(b) < off+16 {
		p.bad()
		return nil, errShort
	}
	off += 12 // sub agent id, sequence, uptime
	n := int(binary.BigEndian.Uint32(b[off:]))
	off += 4
	var out []Record
	for i := 0; i < n && off+8 <= len(b); i++ {
		format := binary.BigEndian.Uint32(b[off:])
		l := int(binary.BigEndian.Uint32(b[off+4:]))
		off += 8
		if off+l > len(b) {
			p.bad()
			break
		}
		body := b[off : off+l]
		off += l
		enterprise, fmtID := format>>12, format&0xfff
		if enterprise != 0 {
			continue
		}
		switch fmtID {
		case 1: // flow sample
			if rec, ok := p.flowSample(body, false); ok {
				out = append(out, rec)
			}
		case 3: // expanded flow sample
			if rec, ok := p.flowSample(body, true); ok {
				out = append(out, rec)
			}
		}
	}
	p.mu.Lock()
	p.Records += int64(len(out))
	p.mu.Unlock()
	return out, nil
}

func (p *Parser) flowSample(b []byte, expanded bool) (Record, bool) {
	var rec Record
	off := 4 // sequence number
	if expanded {
		if len(b) < off+8 {
			return rec, false
		}
		off += 8 // source id type + index
	} else {
		if len(b) < off+4 {
			return rec, false
		}
		off += 4
	}
	if len(b) < off+12 {
		return rec, false
	}
	rate := uint64(binary.BigEndian.Uint32(b[off:]))
	off += 12 // rate, pool, drops
	if rate == 0 {
		rate = 1
	}
	if expanded {
		if len(b) < off+16 {
			return rec, false
		}
		rec.InIf = binary.BigEndian.Uint32(b[off+4:])
		rec.OutIf = binary.BigEndian.Uint32(b[off+12:])
		off += 16
	} else {
		if len(b) < off+8 {
			return rec, false
		}
		rec.InIf = binary.BigEndian.Uint32(b[off:]) & 0x3fffffff
		rec.OutIf = binary.BigEndian.Uint32(b[off+4:]) & 0x3fffffff
		off += 8
	}
	if len(b) < off+4 {
		return rec, false
	}
	nrec := int(binary.BigEndian.Uint32(b[off:]))
	off += 4
	found := false
	for i := 0; i < nrec && off+8 <= len(b); i++ {
		format := binary.BigEndian.Uint32(b[off:])
		l := int(binary.BigEndian.Uint32(b[off+4:]))
		off += 8
		if off+l > len(b) {
			return rec, false
		}
		body := b[off : off+l]
		off += l
		if format == 1 && len(body) >= 16 { // raw packet header
			proto := binary.BigEndian.Uint32(body)
			frameLen := binary.BigEndian.Uint32(body[4:])
			hdrLen := int(binary.BigEndian.Uint32(body[12:]))
			if proto == 1 && 16+hdrLen <= len(body) {
				if parseEthernet(body[16:16+hdrLen], &rec) {
					rec.Bytes = uint64(frameLen) * rate
					rec.Packets = rate
					found = true
				}
			}
		}
	}
	return rec, found
}

// parseEthernet fills addresses, ports and protocol from a sampled frame.
func parseEthernet(f []byte, rec *Record) bool {
	if len(f) < 14 {
		return false
	}
	et := binary.BigEndian.Uint16(f[12:])
	off := 14
	for (et == 0x8100 || et == 0x88a8) && len(f) >= off+4 { // 802.1Q / QinQ
		et = binary.BigEndian.Uint16(f[off+2:])
		off += 4
	}
	ip := f[off:]
	switch et {
	case 0x0800:
		if len(ip) < 20 {
			return false
		}
		ihl := int(ip[0]&0x0f) * 4
		if ihl < 20 || len(ip) < ihl {
			return false
		}
		rec.Proto = ip[9]
		rec.Src = netip.AddrFrom4([4]byte(ip[12:16]))
		rec.Dst = netip.AddrFrom4([4]byte(ip[16:20]))
		frag := binary.BigEndian.Uint16(ip[6:]) & 0x1fff
		if frag == 0 {
			ports(ip[ihl:], rec)
		}
		return true
	case 0x86dd:
		if len(ip) < 40 {
			return false
		}
		rec.Proto = ip[6]
		rec.Src = netip.AddrFrom16([16]byte(ip[8:24]))
		rec.Dst = netip.AddrFrom16([16]byte(ip[24:40]))
		ports(ip[40:], rec)
		return true
	}
	return false
}

func ports(l4 []byte, rec *Record) {
	if (rec.Proto == 6 || rec.Proto == 17 || rec.Proto == 132) && len(l4) >= 4 {
		rec.SrcPort = binary.BigEndian.Uint16(l4)
		rec.DstPort = binary.BigEndian.Uint16(l4[2:])
	}
}
