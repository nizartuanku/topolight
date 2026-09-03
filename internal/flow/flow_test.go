package flow

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

func be16(b []byte, v uint16) []byte { return binary.BigEndian.AppendUint16(b, v) }
func be32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }

// v5 datagram with two records; sampling 1:4 set in the header.
func v5Datagram() []byte {
	b := []byte{}
	b = be16(b, 5)
	b = be16(b, 2)
	b = be32(b, 1000)       // uptime
	b = be32(b, 1725000000) // unix secs
	b = be32(b, 0)          // nsecs
	b = be32(b, 1)          // flow seq
	b = append(b, 0, 0)     // engine type/id
	b = be16(b, 1<<14|4)    // sampling mode 1, interval 4
	rec := func(src, dst [4]byte, in, out uint16, pkts, bytes uint32, sp, dp uint16, proto byte) []byte {
		r := []byte{}
		r = append(r, src[:]...)
		r = append(r, dst[:]...)
		r = append(r, 0, 0, 0, 0) // nexthop
		r = be16(r, in)
		r = be16(r, out)
		r = be32(r, pkts)
		r = be32(r, bytes)
		r = be32(r, 0)
		r = be32(r, 0)
		r = be16(r, sp)
		r = be16(r, dp)
		r = append(r, 0, 0x18, proto, 0) // pad, flags, proto, tos
		r = be16(r, 0)
		r = be16(r, 0)
		r = append(r, 24, 24, 0, 0)
		return r
	}
	b = append(b, rec([4]byte{10, 0, 0, 5}, [4]byte{1, 1, 1, 1}, 3, 7, 10, 1500, 51000, 443, 6)...)
	b = append(b, rec([4]byte{10, 0, 0, 9}, [4]byte{10, 0, 0, 5}, 7, 3, 2, 200, 40000, 53, 17)...)
	return b
}

func TestNetFlowV5(t *testing.T) {
	p := NewParser()
	recs, err := p.Parse("10.0.0.1", v5Datagram(), time.Now())
	if err != nil || len(recs) != 2 {
		t.Fatalf("v5: %v %d", err, len(recs))
	}
	r := recs[0]
	if r.Src != netip.MustParseAddr("10.0.0.5") || r.Dst != netip.MustParseAddr("1.1.1.1") || r.DstPort != 443 || r.Proto != 6 || r.InIf != 3 || r.OutIf != 7 {
		t.Fatalf("v5 fields: %+v", r)
	}
	if r.Bytes != 1500*4 || r.Packets != 10*4 {
		t.Fatalf("v5 sampling not applied: %+v", r)
	}
}

// v9: template flowset then a data flowset with two records, sent as two datagrams.
func v9Template(fields [][2]uint16) []byte {
	b := []byte{}
	b = be16(b, 9)
	b = be16(b, 1) // count (flowsets)
	b = be32(b, 1000)
	b = be32(b, 1725000000)
	b = be32(b, 1)
	b = be32(b, 42) // source id
	fs := []byte{}
	fs = be16(fs, 256) // template id
	fs = be16(fs, uint16(len(fields)))
	for _, f := range fields {
		fs = be16(fs, f[0])
		fs = be16(fs, f[1])
	}
	b = be16(b, 0)
	b = be16(b, uint16(4+len(fs)))
	b = append(b, fs...)
	return b
}

func v9Data(records [][]byte) []byte {
	b := []byte{}
	b = be16(b, 9)
	b = be16(b, 1)
	b = be32(b, 2000)
	b = be32(b, 1725000060)
	b = be32(b, 2)
	b = be32(b, 42)
	body := []byte{}
	for _, r := range records {
		body = append(body, r...)
	}
	for len(body)%4 != 0 {
		body = append(body, 0)
	}
	b = be16(b, 256)
	b = be16(b, uint16(4+len(body)))
	b = append(b, body...)
	return b
}

func TestNetFlowV9Templates(t *testing.T) {
	p := NewParser()
	fields := [][2]uint16{{ieIPv4Src, 4}, {ieIPv4Dst, 4}, {ieSrcPort, 2}, {ieDstPort, 2}, {ieProtocol, 1}, {ieInputSNMP, 2}, {ieOutputSNMP, 2}, {ieInBytes, 4}, {ieInPkts, 4}}
	mk := func(src, dst [4]byte, sp, dp uint16, proto byte, in, out uint16, bytes, pkts uint32) []byte {
		r := []byte{}
		r = append(r, src[:]...)
		r = append(r, dst[:]...)
		r = be16(r, sp)
		r = be16(r, dp)
		r = append(r, proto)
		r = be16(r, in)
		r = be16(r, out)
		r = be32(r, bytes)
		r = be32(r, pkts)
		return r
	}
	data := v9Data([][]byte{mk([4]byte{192, 168, 1, 10}, [4]byte{8, 8, 8, 8}, 55000, 53, 17, 1, 2, 300, 3), mk([4]byte{192, 168, 1, 11}, [4]byte{93, 184, 216, 34}, 55001, 80, 6, 1, 2, 90000, 70)})
	// data before template must be counted as no-template and yield nothing
	recs, _ := p.Parse("10.0.0.2", data, time.Now())
	if len(recs) != 0 || p.NoTemplate != 1 {
		t.Fatalf("data before template: %d recs, noTemplate=%d", len(recs), p.NoTemplate)
	}
	if _, err := p.Parse("10.0.0.2", v9Template(fields), time.Now()); err != nil {
		t.Fatal(err)
	}
	recs, _ = p.Parse("10.0.0.2", data, time.Now())
	if len(recs) != 2 {
		t.Fatalf("v9 data: %d", len(recs))
	}
	if recs[1].Dst != netip.MustParseAddr("93.184.216.34") || recs[1].Bytes != 90000 || recs[1].Packets != 70 || recs[1].DstPort != 80 || recs[1].InIf != 1 {
		t.Fatalf("v9 record: %+v", recs[1])
	}
	// a different exporter must not see this template
	recs, _ = p.Parse("10.0.0.3", data, time.Now())
	if len(recs) != 0 {
		t.Fatal("template leaked across exporters")
	}
}

func TestIPFIX(t *testing.T) {
	p := NewParser()
	// template set (id 2) with IPv6 addresses and 8-byte counters + sampling interval
	fields := [][2]uint16{{ieIPv6Src, 16}, {ieIPv6Dst, 16}, {ieSrcPort, 2}, {ieDstPort, 2}, {ieProtocol, 1}, {ieOctetDelta, 8}, {iePacketDelta, 8}, {ieSamplingIntv, 4}}
	tmpl := []byte{}
	tmpl = be16(tmpl, 300)
	tmpl = be16(tmpl, uint16(len(fields)))
	for _, f := range fields {
		tmpl = be16(tmpl, f[0])
		tmpl = be16(tmpl, f[1])
	}
	set := be16(nil, 2)
	set = be16(set, uint16(4+len(tmpl)))
	set = append(set, tmpl...)
	hdr := func(body []byte) []byte {
		b := be16(nil, 10)
		b = be16(b, uint16(16+len(body)))
		b = be32(b, 1725000000)
		b = be32(b, 1)
		b = be32(b, 7)
		return append(b, body...)
	}
	if _, err := p.Parse("fd00::1", hdr(set), time.Now()); err != nil {
		t.Fatal(err)
	}
	src := netip.MustParseAddr("2001:db8::10").As16()
	dst := netip.MustParseAddr("2606:4700::1111").As16()
	r := append([]byte{}, src[:]...)
	r = append(r, dst[:]...)
	r = be16(r, 40000)
	r = be16(r, 443)
	r = append(r, 6)
	r = binary.BigEndian.AppendUint64(r, 12345)
	r = binary.BigEndian.AppendUint64(r, 12)
	r = be32(r, 100) // sampling 1:100
	data := be16(nil, 300)
	data = be16(data, uint16(4+len(r)))
	data = append(data, r...)
	recs, err := p.Parse("fd00::1", hdr(data), time.Now())
	if err != nil || len(recs) != 1 {
		t.Fatalf("ipfix: %v %d", err, len(recs))
	}
	if recs[0].Src.String() != "2001:db8::10" || recs[0].Bytes != 12345*100 || recs[0].Packets != 1200 || recs[0].DstPort != 443 {
		t.Fatalf("ipfix record: %+v", recs[0])
	}
}

func sflowDatagram() []byte {
	// one flow sample (format 1) with a raw ethernet+IPv4+TCP header, rate 512
	frame := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 0x81, 0x00, 0x00, 0x64, 0x08, 0x00} // vlan 100 then IPv4
	ip := []byte{0x45, 0, 0, 0x28, 0, 0, 0x40, 0, 64, 6, 0, 0, 172, 16, 5, 20, 172, 16, 9, 1}
	tcp := be16(nil, 44444)
	tcp = be16(tcp, 3389)
	tcp = append(tcp, make([]byte, 16)...)
	frame = append(frame, ip...)
	frame = append(frame, tcp...)
	rec := be32(nil, 1)                 // protocol ethernet
	rec = be32(rec, 1400)               // frame length
	rec = be32(rec, 4)                  // stripped
	rec = be32(rec, uint32(len(frame))) // header length
	rec = append(rec, frame...)
	for len(rec)%4 != 0 {
		rec = append(rec, 0)
	}
	sample := be32(nil, 1) // seq
	sample = be32(sample, 0x00000005)
	sample = be32(sample, 512) // rate
	sample = be32(sample, 100000)
	sample = be32(sample, 0)
	sample = be32(sample, 5) // input
	sample = be32(sample, 9) // output
	sample = be32(sample, 1) // one record
	sample = be32(sample, 1) // record format raw header
	sample = be32(sample, uint32(len(rec)))
	sample = append(sample, rec...)
	b := be32(nil, 5)
	b = be32(b, 1)
	b = append(b, 172, 16, 0, 1) // agent
	b = be32(b, 0)
	b = be32(b, 1)
	b = be32(b, 1000)
	b = be32(b, 1) // nsamples
	b = be32(b, 1) // format flow sample
	b = be32(b, uint32(len(sample)))
	b = append(b, sample...)
	return b
}

func TestSFlow(t *testing.T) {
	p := NewParser()
	recs, err := p.ParseSFlow(sflowDatagram())
	if err != nil || len(recs) != 1 {
		t.Fatalf("sflow: %v %d", err, len(recs))
	}
	r := recs[0]
	if r.Src.String() != "172.16.5.20" || r.Dst.String() != "172.16.9.1" || r.Proto != 6 || r.DstPort != 3389 || r.InIf != 5 || r.OutIf != 9 {
		t.Fatalf("sflow record: %+v", r)
	}
	if r.Bytes != 1400*512 || r.Packets != 512 {
		t.Fatalf("sflow scaling: %+v", r)
	}
}

func TestAggregateAndMerge(t *testing.T) {
	a, _ := NewAggregator("")
	now := time.Date(2026, 9, 3, 10, 0, 30, 0, time.UTC)
	mk := func(src, dst string, dport uint16, bytes uint64) Record {
		return Record{Src: netip.MustParseAddr(src), Dst: netip.MustParseAddr(dst), SrcPort: 50000, DstPort: dport, Proto: 6, InIf: 1, OutIf: 2, Bytes: bytes, Packets: bytes / 1000}
	}
	// minute 0
	a.Add("10.0.0.1", []Record{mk("10.1.1.1", "1.1.1.1", 443, 5000), mk("10.1.1.2", "1.1.1.1", 443, 3000), mk("10.1.1.1", "8.8.8.8", 53, 100)}, now)
	// minute 1 (rolls minute 0 into the ring)
	a.Add("10.0.0.1", []Record{mk("10.1.1.3", "1.1.1.1", 80, 7000)}, now.Add(time.Minute))
	w := a.Window("10.0.0.1", 5*time.Minute, now.Add(time.Minute))
	if w.Bytes != 15100 || w.Flows != 4 {
		t.Fatalf("window totals: %+v", w)
	}
	if w.Talkers[0].Key != "10.1.1.3" || w.Talkers[1].Key != "10.1.1.1" || w.Talkers[1].Bytes != 5100 {
		t.Fatalf("talkers: %+v", w.Talkers)
	}
	if w.Targets[0].Key != "1.1.1.1" || w.Targets[0].Bytes != 15000 {
		t.Fatalf("targets: %+v", w.Targets)
	}
	if w.Apps[0].Name != "http" && w.Apps[0].Name != "https" {
		t.Fatalf("apps: %+v", w.Apps)
	}
	if len(w.Ifaces) != 2 || w.Ifaces[0].InBytes != 15100 || w.Ifaces[1].OutBytes != 15100 {
		t.Fatalf("ifaces: %+v", w.Ifaces)
	}
	// advance past the 5-minute boundary: Tick folds minutes into a persisted 5-minute summary
	a.Tick(now.Add(6 * time.Minute))
	a.mu.Lock()
	fives := len(a.exp["10.0.0.1"].fives)
	a.mu.Unlock()
	if fives != 1 {
		t.Fatalf("expected one 5-minute summary, got %d", fives)
	}
	w24 := a.Window("", 24*time.Hour, now.Add(6*time.Minute))
	if w24.Bytes != 15100 || len(w24.Ifaces) != 0 { // cross-exporter merge drops ifaces
		t.Fatalf("24h window: %+v", w24)
	}
	if s := a.Series("10.0.0.1", 24*time.Hour, now.Add(6*time.Minute)); len(s) != 1 || s[0].Bytes != 15100 {
		t.Fatalf("series: %+v", s)
	}
}

func TestAppPortAndNames(t *testing.T) {
	if p := appPort(Record{Proto: 6, SrcPort: 443, DstPort: 51234}); p != 443 {
		t.Fatal("reverse flow should pick 443")
	}
	if p := appPort(Record{Proto: 17, SrcPort: 40000, DstPort: 40001}); p != 40000 {
		t.Fatal("unknown ports: lower wins")
	}
	if p := appPort(Record{Proto: 1}); p != 0 {
		t.Fatal("icmp has no port")
	}
	if AppName(1, 0) != "icmp" || AppName(6, 443) != "https" || AppName(17, 40000) != "udp/40000" || AppName(47, 0) != "gre" {
		t.Fatalf("names: %s %s %s %s", AppName(1, 0), AppName(6, 443), AppName(17, 40000), AppName(47, 0))
	}
}

func TestPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	a, err := NewAggregator(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	a.Add("10.0.0.9", []Record{{Src: netip.MustParseAddr("10.2.2.2"), Dst: netip.MustParseAddr("10.3.3.3"), Proto: 17, DstPort: 53, Bytes: 777, Packets: 7}}, now)
	a.Tick(now.Add(6 * time.Minute))
	a.Close()
	b, _ := NewAggregator(dir)
	w := b.Window("10.0.0.9", 24*time.Hour, now.Add(7*time.Minute))
	if w.Bytes != 777 || len(w.Talkers) != 1 {
		t.Fatalf("reload: %+v", w)
	}
	// a short window right after the restart must still see the reloaded 5-minute summary
	short := b.Window("10.0.0.9", 15*time.Minute, now.Add(7*time.Minute))
	if short.Bytes != 777 || short.Covered != 300 {
		t.Fatalf("short window after restart: bytes %d covered %d", short.Bytes, short.Covered)
	}
	// new minutes after the restart are added, the old five is kept until a minute overlaps it
	b.Add("10.0.0.9", []Record{{Src: netip.MustParseAddr("10.2.2.2"), Dst: netip.MustParseAddr("10.3.3.3"), Proto: 17, DstPort: 53, Bytes: 100, Packets: 1}}, now.Add(8*time.Minute))
	short = b.Window("10.0.0.9", 15*time.Minute, now.Add(8*time.Minute))
	if short.Bytes != 877 {
		t.Fatalf("mixed window: %+v", short)
	}
	if pts := b.Series("10.0.0.9", 15*time.Minute, now.Add(8*time.Minute)); len(pts) != 2 || pts[0].Span != 300 || pts[1].Span != 60 {
		t.Fatalf("mixed series: %+v", pts)
	}
}
