package flow

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sizes of the kept top-N lists per bucket. These bound both memory and disk:
// a 5-minute summary is ~20–30 KB of JSON regardless of how many flows arrived.
const (
	topTalkers = 100
	topConv    = 200
	topApps    = 50
	maxKeys    = 50000 // distinct keys tracked per exporter per minute before overflow
	minuteRing = 60    // minutes kept in memory per exporter
	fiveRing   = 288   // 5-minute summaries kept in memory per exporter (24 h)
)

// Entry is one row of a top list.
type Entry struct {
	Key     string `json:"k"`
	Bytes   uint64 `json:"b"`
	Packets uint64 `json:"p"`
}

// Conv is one conversation (src → dst on proto/port).
type Conv struct {
	Src     string `json:"s"`
	Dst     string `json:"d"`
	Proto   uint8  `json:"pr"`
	Port    uint16 `json:"po"` // the "service" port (see appPort)
	Bytes   uint64 `json:"b"`
	Packets uint64 `json:"p"`
}

// App is traffic by protocol/port.
type App struct {
	Proto   uint8  `json:"pr"`
	Port    uint16 `json:"po"`
	Name    string `json:"n"`
	Bytes   uint64 `json:"b"`
	Packets uint64 `json:"p"`
}

// IfStat is traffic seen entering/leaving an ifIndex of the exporter.
type IfStat struct {
	IfIndex  uint32 `json:"i"`
	InBytes  uint64 `json:"ib"`
	OutBytes uint64 `json:"ob"`
	InPkts   uint64 `json:"ip"`
	OutPkts  uint64 `json:"op"`
}

// Summary is the aggregate of one bucket for one exporter.
type Summary struct {
	TS       time.Time `json:"ts"`
	Span     int       `json:"span"`              // seconds asked for
	Covered  int       `json:"covered,omitempty"` // seconds that actually hold data (Window only)
	Exporter string    `json:"exporter"`
	Bytes    uint64    `json:"bytes"`
	Packets  uint64    `json:"packets"`
	Flows    uint64    `json:"flows"`
	Overflow bool      `json:"overflow,omitempty"`
	Talkers  []Entry   `json:"talkers"`
	Targets  []Entry   `json:"targets"`
	Convs    []Conv    `json:"convs"`
	Apps     []App     `json:"apps"`
	Ifaces   []IfStat  `json:"ifaces"`
}

type convKey struct {
	src, dst netip.Addr
	proto    uint8
	port     uint16
}
type appKey struct {
	proto uint8
	port  uint16
}
type counter struct{ b, p uint64 }
type ifCounter struct{ ib, ob, ip, op uint64 }

// bucket accumulates one minute for one exporter.
type bucket struct {
	start    time.Time
	bytes    uint64
	packets  uint64
	flows    uint64
	overflow bool
	src      map[netip.Addr]*counter
	dst      map[netip.Addr]*counter
	conv     map[convKey]*counter
	app      map[appKey]*counter
	ifs      map[uint32]*ifCounter
}

func newBucket(start time.Time) *bucket {
	return &bucket{start: start, src: map[netip.Addr]*counter{}, dst: map[netip.Addr]*counter{}, conv: map[convKey]*counter{}, app: map[appKey]*counter{}, ifs: map[uint32]*ifCounter{}}
}

func (bk *bucket) add(r Record) {
	bk.bytes += r.Bytes
	bk.packets += r.Packets
	bk.flows++
	if i := bk.ifs[r.InIf]; i != nil {
		i.ib += r.Bytes
		i.ip += r.Packets
	} else if len(bk.ifs) < 4096 {
		bk.ifs[r.InIf] = &ifCounter{ib: r.Bytes, ip: r.Packets}
	}
	if r.OutIf != r.InIf || r.OutIf == 0 {
		if o := bk.ifs[r.OutIf]; o != nil {
			o.ob += r.Bytes
			o.op += r.Packets
		} else if len(bk.ifs) < 4096 {
			bk.ifs[r.OutIf] = &ifCounter{ob: r.Bytes, op: r.Packets}
		}
	}
	bump := func(m map[netip.Addr]*counter, k netip.Addr) {
		if c := m[k]; c != nil {
			c.b += r.Bytes
			c.p += r.Packets
		} else if len(m) < maxKeys {
			m[k] = &counter{r.Bytes, r.Packets}
		} else {
			bk.overflow = true
		}
	}
	bump(bk.src, r.Src)
	bump(bk.dst, r.Dst)
	port := appPort(r)
	ck := convKey{r.Src, r.Dst, r.Proto, port}
	if c := bk.conv[ck]; c != nil {
		c.b += r.Bytes
		c.p += r.Packets
	} else if len(bk.conv) < maxKeys {
		bk.conv[ck] = &counter{r.Bytes, r.Packets}
	} else {
		bk.overflow = true
	}
	ak := appKey{r.Proto, port}
	if c := bk.app[ak]; c != nil {
		c.b += r.Bytes
		c.p += r.Packets
	} else if len(bk.app) < 8192 {
		bk.app[ak] = &counter{r.Bytes, r.Packets}
	}
}

// appPort picks the port that names the service: a well-known destination
// port first, then a well-known source port, else the lower of the two
// (ephemeral ports are high). Non-port protocols get 0.
func appPort(r Record) uint16 {
	if r.Proto != 6 && r.Proto != 17 && r.Proto != 132 {
		return 0
	}
	if _, ok := portNames[r.DstPort]; ok {
		return r.DstPort
	}
	if _, ok := portNames[r.SrcPort]; ok {
		return r.SrcPort
	}
	if r.SrcPort != 0 && r.SrcPort < r.DstPort {
		return r.SrcPort
	}
	return r.DstPort
}

var portNames = map[uint16]string{
	20: "ftp-data", 21: "ftp", 22: "ssh", 23: "telnet", 25: "smtp", 53: "dns", 67: "dhcp", 68: "dhcp", 69: "tftp", 80: "http", 88: "kerberos",
	110: "pop3", 123: "ntp", 135: "msrpc", 137: "netbios", 138: "netbios", 139: "netbios", 143: "imap", 161: "snmp", 162: "snmp-trap", 179: "bgp",
	389: "ldap", 443: "https", 445: "smb", 465: "smtps", 500: "isakmp", 514: "syslog", 515: "lpd", 520: "rip", 548: "afp", 554: "rtsp", 587: "submission",
	631: "ipp", 636: "ldaps", 853: "dns-tls", 873: "rsync", 989: "ftps", 990: "ftps", 993: "imaps", 995: "pop3s", 1194: "openvpn", 1433: "mssql", 1521: "oracle",
	1723: "pptp", 1812: "radius", 1813: "radius-acct", 2049: "nfs", 2055: "netflow", 3128: "proxy", 3306: "mysql", 3389: "rdp", 3478: "stun", 4500: "ipsec-nat",
	5060: "sip", 5061: "sips", 5222: "xmpp", 5432: "postgres", 5900: "vnc", 5938: "teamviewer", 6343: "sflow", 6379: "redis", 8080: "http-alt", 8443: "https-alt",
	8883: "mqtts", 1883: "mqtt", 9100: "printer", 9200: "elastic", 27017: "mongodb", 51820: "wireguard", 3000: "http-3000", 5000: "http-5000",
}

var protoNames = map[uint8]string{1: "icmp", 2: "igmp", 6: "tcp", 17: "udp", 47: "gre", 50: "esp", 51: "ah", 58: "icmpv6", 89: "ospf", 132: "sctp"}

// AppName renders proto/port as a label.
func AppName(proto uint8, port uint16) string {
	if n, ok := portNames[port]; ok && (proto == 6 || proto == 17 || proto == 132) {
		return n
	}
	pn := protoNames[proto]
	if pn == "" {
		pn = "ip" + strconv.Itoa(int(proto))
	}
	if port == 0 {
		return pn
	}
	return pn + "/" + strconv.Itoa(int(port))
}

// summarise turns a bucket into a bounded Summary.
func (bk *bucket) summarise(exporter string, span int) Summary {
	s := Summary{TS: bk.start, Span: span, Exporter: exporter, Bytes: bk.bytes, Packets: bk.packets, Flows: bk.flows, Overflow: bk.overflow,
		Talkers: []Entry{}, Targets: []Entry{}, Convs: []Conv{}, Apps: []App{}, Ifaces: []IfStat{}}
	top := func(m map[netip.Addr]*counter) []Entry {
		list := make([]Entry, 0, len(m))
		for k, c := range m {
			list = append(list, Entry{Key: k.String(), Bytes: c.b, Packets: c.p})
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Bytes > list[b].Bytes })
		if len(list) > topTalkers {
			list = list[:topTalkers]
		}
		return list
	}
	s.Talkers = top(bk.src)
	s.Targets = top(bk.dst)
	for k, c := range bk.conv {
		s.Convs = append(s.Convs, Conv{Src: k.src.String(), Dst: k.dst.String(), Proto: k.proto, Port: k.port, Bytes: c.b, Packets: c.p})
	}
	sort.Slice(s.Convs, func(a, b int) bool { return s.Convs[a].Bytes > s.Convs[b].Bytes })
	if len(s.Convs) > topConv {
		s.Convs = s.Convs[:topConv]
	}
	for k, c := range bk.app {
		s.Apps = append(s.Apps, App{Proto: k.proto, Port: k.port, Name: AppName(k.proto, k.port), Bytes: c.b, Packets: c.p})
	}
	sort.Slice(s.Apps, func(a, b int) bool { return s.Apps[a].Bytes > s.Apps[b].Bytes })
	if len(s.Apps) > topApps {
		s.Apps = s.Apps[:topApps]
	}
	for i, c := range bk.ifs {
		s.Ifaces = append(s.Ifaces, IfStat{IfIndex: i, InBytes: c.ib, OutBytes: c.ob, InPkts: c.ip, OutPkts: c.op})
	}
	sort.Slice(s.Ifaces, func(a, b int) bool { return s.Ifaces[a].IfIndex < s.Ifaces[b].IfIndex })
	return s
}

// Merge combines summaries (same exporter or across exporters) into one
// covering [start, start+span). Top lists are merged by key and re-cut, so
// the tail is approximate — the head, which is what people look at, is exact
// as long as an entry made each source list.
func Merge(list []Summary, start time.Time, span int, exporter string) Summary {
	out := Summary{TS: start, Span: span, Exporter: exporter, Talkers: []Entry{}, Targets: []Entry{}, Convs: []Conv{}, Apps: []App{}, Ifaces: []IfStat{}}
	talk := map[string]*counter{}
	targ := map[string]*counter{}
	conv := map[Conv]*counter{}
	app := map[appKey]*counter{}
	ifs := map[uint32]*ifCounter{}
	add := func(m map[string]*counter, e Entry) {
		if c := m[e.Key]; c != nil {
			c.b += e.Bytes
			c.p += e.Packets
		} else {
			m[e.Key] = &counter{e.Bytes, e.Packets}
		}
	}
	for _, s := range list {
		out.Bytes += s.Bytes
		out.Packets += s.Packets
		out.Flows += s.Flows
		out.Overflow = out.Overflow || s.Overflow
		for _, e := range s.Talkers {
			add(talk, e)
		}
		for _, e := range s.Targets {
			add(targ, e)
		}
		for _, c := range s.Convs {
			k := Conv{Src: c.Src, Dst: c.Dst, Proto: c.Proto, Port: c.Port}
			if x := conv[k]; x != nil {
				x.b += c.Bytes
				x.p += c.Packets
			} else {
				conv[k] = &counter{c.Bytes, c.Packets}
			}
		}
		for _, a := range s.Apps {
			k := appKey{a.Proto, a.Port}
			if x := app[k]; x != nil {
				x.b += a.Bytes
				x.p += a.Packets
			} else {
				app[k] = &counter{a.Bytes, a.Packets}
			}
		}
		// interfaces only make sense within one exporter
		if exporter != "" {
			for _, i := range s.Ifaces {
				if x := ifs[i.IfIndex]; x != nil {
					x.ib += i.InBytes
					x.ob += i.OutBytes
					x.ip += i.InPkts
					x.op += i.OutPkts
				} else {
					ifs[i.IfIndex] = &ifCounter{i.InBytes, i.OutBytes, i.InPkts, i.OutPkts}
				}
			}
		}
	}
	cut := func(m map[string]*counter) []Entry {
		l := make([]Entry, 0, len(m))
		for k, c := range m {
			l = append(l, Entry{k, c.b, c.p})
		}
		sort.Slice(l, func(a, b int) bool { return l[a].Bytes > l[b].Bytes })
		if len(l) > topTalkers {
			l = l[:topTalkers]
		}
		return l
	}
	out.Talkers = cut(talk)
	out.Targets = cut(targ)
	for k, c := range conv {
		k.Bytes, k.Packets = c.b, c.p
		out.Convs = append(out.Convs, k)
	}
	sort.Slice(out.Convs, func(a, b int) bool { return out.Convs[a].Bytes > out.Convs[b].Bytes })
	if len(out.Convs) > topConv {
		out.Convs = out.Convs[:topConv]
	}
	for k, c := range app {
		out.Apps = append(out.Apps, App{Proto: k.proto, Port: k.port, Name: AppName(k.proto, k.port), Bytes: c.b, Packets: c.p})
	}
	sort.Slice(out.Apps, func(a, b int) bool { return out.Apps[a].Bytes > out.Apps[b].Bytes })
	if len(out.Apps) > topApps {
		out.Apps = out.Apps[:topApps]
	}
	for i, c := range ifs {
		out.Ifaces = append(out.Ifaces, IfStat{i, c.ib, c.ob, c.ip, c.op})
	}
	sort.Slice(out.Ifaces, func(a, b int) bool { return out.Ifaces[a].IfIndex < out.Ifaces[b].IfIndex })
	return out
}

// exporterState is the per-exporter working set.
type exporterState struct {
	cur     *bucket
	minutes []Summary // newest last, ≤ minuteRing
	fives   []Summary // newest last, ≤ fiveRing (persisted)
	last    time.Time // last datagram
	records uint64
}

// Aggregator owns every exporter's buckets and the on-disk journal.
type Aggregator struct {
	mu  sync.Mutex
	exp map[string]*exporterState
	dir string // "" = memory only
	day string
	f   *os.File
	w   *bufio.Writer
	// OnSummary, when set, receives every 5-minute summary (for the event stream).
	OnSummary func(Summary)
}

// NewAggregator opens (or creates) the flow directory and reloads the last
// 24 hours of 5-minute summaries so graphs survive a restart.
func NewAggregator(dir string) (*Aggregator, error) {
	a := &Aggregator{exp: map[string]*exporterState{}, dir: dir}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		a.reload(time.Now())
	}
	return a, nil
}

func (a *Aggregator) state(exporter string) *exporterState {
	e := a.exp[exporter]
	if e == nil {
		e = &exporterState{}
		a.exp[exporter] = e
	}
	return e
}

// Add folds records from one exporter into the current minute; when the
// minute rolls over the previous bucket is summarised.
func (a *Aggregator) Add(exporter string, recs []Record, now time.Time) {
	if len(recs) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.state(exporter)
	e.last = now
	e.records += uint64(len(recs))
	minute := now.Truncate(time.Minute)
	if e.cur == nil {
		e.cur = newBucket(minute)
	} else if !e.cur.start.Equal(minute) {
		a.rollLocked(exporter, e, minute)
	}
	for _, r := range recs {
		e.cur.add(r)
	}
}

// Tick closes idle buckets: call once a minute.
func (a *Aggregator) Tick(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	minute := now.Truncate(time.Minute)
	for exporter, e := range a.exp {
		if e.cur != nil && !e.cur.start.Equal(minute) {
			a.rollLocked(exporter, e, minute)
		}
	}
	if a.w != nil {
		a.w.Flush()
	}
}

// rollLocked finalises e.cur into the minute ring, and every 5 minutes folds
// the ring into a persisted 5-minute summary.
func (a *Aggregator) rollLocked(exporter string, e *exporterState, next time.Time) {
	s := e.cur.summarise(exporter, 60)
	e.cur = newBucket(next)
	e.minutes = append(e.minutes, s)
	if len(e.minutes) > minuteRing {
		e.minutes = e.minutes[len(e.minutes)-minuteRing:]
	}
	// fold when the finished minute was the last of its 5-minute slot, or when
	// the ring has skipped past the slot (idle exporter)
	slot := s.TS.Truncate(5 * time.Minute)
	if next.Sub(slot) >= 5*time.Minute {
		var in []Summary
		for _, m := range e.minutes {
			if m.TS.Truncate(5 * time.Minute).Equal(slot) {
				in = append(in, m)
			}
		}
		if len(in) > 0 {
			f := Merge(in, slot, 300, exporter)
			e.fives = append(e.fives, f)
			if len(e.fives) > fiveRing {
				e.fives = e.fives[len(e.fives)-fiveRing:]
			}
			a.persistLocked(f)
			if a.OnSummary != nil {
				go a.OnSummary(f)
			}
		}
	}
}

func (a *Aggregator) persistLocked(s Summary) {
	if a.dir == "" {
		return
	}
	day := s.TS.UTC().Format("2006-01-02")
	if a.f == nil || day != a.day {
		a.closeLocked()
		f, err := os.OpenFile(filepath.Join(a.dir, day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		a.f, a.w, a.day = f, bufio.NewWriterSize(f, 64<<10), day
		go a.compressOld(day)
	}
	b, _ := json.Marshal(s)
	a.w.Write(b)
	a.w.WriteByte('\n')
}

func (a *Aggregator) closeLocked() {
	if a.w != nil {
		a.w.Flush()
	}
	if a.f != nil {
		a.f.Close()
	}
	a.f, a.w = nil, nil
}

// Close flushes the journal.
func (a *Aggregator) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeLocked()
}

// compressOld gzips every finished day's journal.
func (a *Aggregator) compressOld(today string) {
	entries, _ := os.ReadDir(a.dir)
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".jsonl") || strings.TrimSuffix(n, ".jsonl") == today {
			continue
		}
		src := filepath.Join(a.dir, n)
		dst := src + ".gz"
		in, err := os.Open(src)
		if err != nil {
			continue
		}
		out, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			in.Close()
			continue
		}
		gz := gzip.NewWriter(out)
		_, cerr := bufio.NewReader(in).WriteTo(gz)
		gz.Close()
		out.Close()
		in.Close()
		if cerr == nil {
			os.Rename(dst+".tmp", dst)
			os.Remove(src)
		} else {
			os.Remove(dst + ".tmp")
		}
	}
}

// reload reads the last 24 h of 5-minute summaries back into memory.
func (a *Aggregator) reload(now time.Time) {
	since := now.Add(-24 * time.Hour)
	for _, day := range []string{since.UTC().Format("2006-01-02"), now.UTC().Format("2006-01-02")} {
		for _, name := range []string{day + ".jsonl", day + ".jsonl.gz"} {
			f, err := os.Open(filepath.Join(a.dir, name))
			if err != nil {
				continue
			}
			var r *bufio.Reader
			if strings.HasSuffix(name, ".gz") {
				gz, err := gzip.NewReader(f)
				if err != nil {
					f.Close()
					continue
				}
				r = bufio.NewReader(gz)
			} else {
				r = bufio.NewReader(f)
			}
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 1<<20), 8<<20)
			for sc.Scan() {
				var s Summary
				if json.Unmarshal(sc.Bytes(), &s) != nil || s.TS.Before(since) {
					continue
				}
				e := a.state(s.Exporter)
				e.fives = append(e.fives, s)
				if len(e.fives) > fiveRing {
					e.fives = e.fives[1:]
				}
			}
			f.Close()
		}
	}
}

// Prune deletes journal days older than keep.
func (a *Aggregator) Prune(keep time.Duration) int {
	if a.dir == "" {
		return 0
	}
	cut := time.Now().Add(-keep).UTC().Format("2006-01-02")
	entries, _ := os.ReadDir(a.dir)
	n := 0
	for _, e := range entries {
		day := strings.SplitN(e.Name(), ".", 2)[0]
		if len(day) == 10 && day < cut {
			if os.Remove(filepath.Join(a.dir, e.Name())) == nil {
				n++
			}
		}
	}
	return n
}

// ExporterInfo is what the API lists per exporter.
type ExporterInfo struct {
	Exporter string    `json:"exporter"`
	Last     time.Time `json:"last"`
	Records  uint64    `json:"records"`
	Bytes24h uint64    `json:"bytes_24h"`
}

// Exporters lists everything that has sent flows.
func (a *Aggregator) Exporters() []ExporterInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ExporterInfo, 0, len(a.exp))
	for k, e := range a.exp {
		info := ExporterInfo{Exporter: k, Last: e.last, Records: e.records}
		for _, f := range e.fives {
			info.Bytes24h += f.Bytes
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes24h > out[j].Bytes24h })
	return out
}

// pick returns the summaries of one exporter that fall in [since, now):
// the 1-minute ring (plus the still-open minute) for short windows, 5-minute
// summaries for long ones — and, after a restart (when the minute ring starts
// late), the reloaded 5-minute summaries that precede the first minute, so a
// short window is never empty just because the process was restarted.
func (e *exporterState) pick(name string, since time.Time, short bool) []Summary {
	var out []Summary
	if !short {
		for _, f := range e.fives {
			if !f.TS.Before(since) {
				out = append(out, f)
			}
		}
		return out
	}
	var first time.Time
	for _, m := range e.minutes {
		if !m.TS.Before(since) {
			out = append(out, m)
			if first.IsZero() || m.TS.Before(first) {
				first = m.TS
			}
		}
	}
	if e.cur != nil && e.cur.flows > 0 {
		out = append(out, e.cur.summarise(name, 60))
		if first.IsZero() {
			first = e.cur.start
		}
	}
	for _, f := range e.fives {
		end := f.TS.Add(time.Duration(f.Span) * time.Second)
		if !f.TS.Before(since) && (first.IsZero() || !end.After(first)) {
			out = append(out, f)
		}
	}
	return out
}

// Window returns a merged summary for one exporter ("" = all) covering the
// last d (5m, 15m, 1h, 6h, 24h). The still-open minute is included so the
// page feels live; Covered says how many seconds actually hold data.
func (a *Aggregator) Window(exporter string, d time.Duration, now time.Time) Summary {
	a.mu.Lock()
	defer a.mu.Unlock()
	since := now.Add(-d)
	var in []Summary
	for k, e := range a.exp {
		if exporter != "" && k != exporter {
			continue
		}
		in = append(in, e.pick(k, since, d <= time.Hour)...)
	}
	out := Merge(in, since, int(d/time.Second), exporter)
	seen := map[int64]int{}
	for _, s := range in {
		if s.Span > seen[s.TS.Unix()] {
			seen[s.TS.Unix()] = s.Span
		}
	}
	for _, sp := range seen {
		out.Covered += sp
	}
	if out.Covered > out.Span {
		out.Covered = out.Span
	}
	return out
}

// Point is one bucket of the throughput series.
type Point struct {
	T       int64  `json:"t"`
	Span    int    `json:"s"`
	Bytes   uint64 `json:"b"`
	Packets uint64 `json:"p"`
}

// Series returns per-bucket totals for one exporter ("" = all) — the chart
// behind the tables. Buckets are 1 minute for ≤1 h, else 5 minutes; after a
// restart a short window may mix both, so each point carries its span.
func (a *Aggregator) Series(exporter string, d time.Duration, now time.Time) []Point {
	a.mu.Lock()
	defer a.mu.Unlock()
	since := now.Add(-d)
	acc := map[int64]*Point{}
	for k, e := range a.exp {
		if exporter != "" && k != exporter {
			continue
		}
		for _, s := range e.pick(k, since, d <= time.Hour) {
			t := s.TS.Unix()
			if c := acc[t]; c != nil {
				c.Bytes += s.Bytes
				c.Packets += s.Packets
				if s.Span > c.Span {
					c.Span = s.Span
				}
			} else {
				acc[t] = &Point{T: t, Span: s.Span, Bytes: s.Bytes, Packets: s.Packets}
			}
		}
	}
	out := make([]Point, 0, len(acc))
	for _, c := range acc {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

// Stats for Admin → System.
func (a *Aggregator) Stats() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	var recs uint64
	for _, e := range a.exp {
		recs += e.records
	}
	size := int64(0)
	if a.dir != "" {
		filepath.Walk(a.dir, func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				size += info.Size()
			}
			return nil
		})
	}
	return map[string]any{"exporters": len(a.exp), "records": recs, "disk_bytes": size}
}

func (s Summary) String() string {
	return fmt.Sprintf("%s %s %ds %d flows %d B", s.Exporter, s.TS.Format(time.RFC3339), s.Span, s.Flows, s.Bytes)
}
