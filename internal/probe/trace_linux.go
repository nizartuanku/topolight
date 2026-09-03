//go:build linux

package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// rawTracer sends ICMP echo requests with increasing TTL on a raw socket and
// reads Time Exceeded (type 11) and Echo Reply (type 0) answers. One socket
// serves every traceroute; runs are serialised per target by the runner.
type rawTracer struct {
	fd    int
	ident uint16
	mu    sync.Mutex
	seq   uint16
	wait  map[uint16]chan hopReply
}

type hopReply struct {
	from string
	done bool
	at   time.Time
}

// NewTracer opens the raw socket; an error means no CAP_NET_RAW.
func NewTracer() (Tracer, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.IPPROTO_ICMP)
	if err != nil {
		return nil, fmt.Errorf("traceroute: raw ICMP socket: %v", err)
	}
	t := &rawTracer{fd: fd, ident: uint16((os.Getpid() + 7919) & 0xffff), wait: map[uint16]chan hopReply{}}
	go t.reader()
	return t, nil
}

func (t *rawTracer) reader() {
	buf := make([]byte, 1500)
	for {
		n, from, err := syscall.Recvfrom(t.fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EBADF) {
				return
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if n < 28 {
			continue
		}
		ihl := int(buf[0]&0x0f) * 4
		if n < ihl+8 {
			continue
		}
		icmpb := buf[ihl:n]
		src := ""
		if sa, ok := from.(*syscall.SockaddrInet4); ok {
			src = net.IP(sa.Addr[:]).String()
		}
		switch icmpb[0] {
		case 0: // echo reply to us?
			if binary.BigEndian.Uint16(icmpb[4:]) != t.ident {
				continue
			}
			t.deliver(binary.BigEndian.Uint16(icmpb[6:]), hopReply{from: src, done: true, at: time.Now()})
		case 11, 3: // time exceeded / unreachable: the original datagram follows the 8-byte header
			if len(icmpb) < 8+20+8 {
				continue
			}
			inner := icmpb[8:]
			iihl := int(inner[0]&0x0f) * 4
			if len(inner) < iihl+8 || inner[iihl] != 8 {
				continue
			}
			if binary.BigEndian.Uint16(inner[iihl+4:]) != t.ident {
				continue
			}
			t.deliver(binary.BigEndian.Uint16(inner[iihl+6:]), hopReply{from: src, done: icmpb[0] == 3, at: time.Now()})
		}
	}
}

func (t *rawTracer) deliver(seq uint16, r hopReply) {
	t.mu.Lock()
	ch := t.wait[seq]
	t.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default:
		}
	}
}

// Trace probes ttl 1..maxHops, one packet per hop, stopping at the target.
func (t *rawTracer) Trace(ctx context.Context, ip string, maxHops int, perHop time.Duration) ([]Hop, error) {
	addr := net.ParseIP(ip).To4()
	if addr == nil {
		return nil, fmt.Errorf("traceroute: %q is not IPv4", ip)
	}
	sa := &syscall.SockaddrInet4{}
	copy(sa.Addr[:], addr)
	var hops []Hop
	for ttl := 1; ttl <= maxHops; ttl++ {
		if ctx.Err() != nil {
			return hops, ctx.Err()
		}
		if err := syscall.SetsockoptInt(t.fd, syscall.IPPROTO_IP, syscall.IP_TTL, ttl); err != nil {
			return hops, err
		}
		t.mu.Lock()
		t.seq++
		if t.seq == 0 {
			t.seq = 1
		}
		seq := t.seq
		ch := make(chan hopReply, 1)
		t.wait[seq] = ch
		t.mu.Unlock()
		pkt := make([]byte, 8+16)
		pkt[0] = 8
		binary.BigEndian.PutUint16(pkt[4:], t.ident)
		binary.BigEndian.PutUint16(pkt[6:], seq)
		copy(pkt[8:], "topolight-trace")
		binary.BigEndian.PutUint16(pkt[2:], checksum(pkt))
		sent := time.Now()
		h := Hop{TTL: ttl}
		if err := syscall.Sendto(t.fd, pkt, 0, sa); err == nil {
			select {
			case r := <-ch:
				h.Addr, h.Done = r.from, r.done
				h.Ms = float64(r.at.Sub(sent).Microseconds()) / 1000
			case <-time.After(perHop):
			case <-ctx.Done():
			}
		}
		t.mu.Lock()
		delete(t.wait, seq)
		t.mu.Unlock()
		hops = append(hops, h)
		if h.Done {
			break
		}
	}
	return hops, nil
}

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
