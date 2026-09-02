//go:build linux

// Package icmp is a small IPv4 echo pinger built on syscalls only. It prefers
// an unprivileged ICMP datagram socket (Linux ping_group_range) and falls back
// to a raw socket when it has CAP_NET_RAW. One socket serves every target.
package icmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

type pending struct {
	sent time.Time
	ch   chan time.Duration
}

// Pinger owns the socket and the reply dispatcher.
type Pinger struct {
	fd    int
	raw   bool
	ident uint16

	mu      sync.Mutex
	seq     uint16
	waiting map[uint16]*pending
	closed  bool
}

// New opens the socket. It returns a descriptive error when neither socket
// type is available so the caller can print a real fix.
func New() (*Pinger, error) {
	p := &Pinger{waiting: map[uint16]*pending{}, ident: uint16(os.Getpid() & 0xffff)}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_ICMP)
	if err == nil {
		p.fd = fd
	} else {
		fd, err2 := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.IPPROTO_ICMP)
		if err2 != nil {
			return nil, fmt.Errorf("icmp: no usable socket (datagram: %v; raw: %v). Run as root, grant CAP_NET_RAW (setcap cap_net_raw+ep topolight), or set net.ipv4.ping_group_range", err, err2)
		}
		p.fd, p.raw = fd, true
	}
	_ = syscall.SetsockoptInt(p.fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
	go p.reader()
	return p, nil
}

// Close stops the pinger.
func (p *Pinger) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	syscall.Close(p.fd)
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

func (p *Pinger) reader() {
	buf := make([]byte, 1500)
	for {
		n, _, err := syscall.Recvfrom(p.fd, buf, 0)
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed || errors.Is(err, syscall.EBADF) {
				return
			}
			if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) {
				continue
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		pkt := buf[:n]
		if p.raw {
			if n < 20 {
				continue
			}
			ihl := int(pkt[0]&0x0f) * 4
			if n < ihl+8 {
				continue
			}
			pkt = pkt[ihl:]
		}
		if len(pkt) < 8 || pkt[0] != 0 { // echo reply
			continue
		}
		ident := binary.BigEndian.Uint16(pkt[4:6])
		seq := binary.BigEndian.Uint16(pkt[6:8])
		if p.raw && ident != p.ident {
			continue
		}
		p.mu.Lock()
		w := p.waiting[seq]
		p.mu.Unlock()
		if w != nil {
			select {
			case w.ch <- time.Since(w.sent):
			default:
			}
		}
	}
}

// Probe sends count echo requests to ip with the given spacing and timeout.
func (p *Pinger) Probe(ip string, count int, interval, timeout time.Duration) (Result, error) {
	addr := net.ParseIP(ip).To4()
	if addr == nil {
		return Result{}, fmt.Errorf("icmp: %q is not an IPv4 address", ip)
	}
	sa := &syscall.SockaddrInet4{}
	copy(sa.Addr[:], addr)
	var res Result
	var rtts []time.Duration
	for i := 0; i < count; i++ {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return res, errors.New("icmp: closed")
		}
		p.seq++
		if p.seq == 0 {
			p.seq = 1
		}
		seq := p.seq
		w := &pending{sent: time.Now(), ch: make(chan time.Duration, 1)}
		p.waiting[seq] = w
		p.mu.Unlock()

		pkt := make([]byte, 8+16)
		pkt[0] = 8 // echo request
		binary.BigEndian.PutUint16(pkt[4:], p.ident)
		binary.BigEndian.PutUint16(pkt[6:], seq)
		binary.BigEndian.PutUint64(pkt[8:], uint64(time.Now().UnixNano()))
		copy(pkt[16:], "topolight")
		binary.BigEndian.PutUint16(pkt[2:], checksum(pkt))
		res.Sent++
		err := syscall.Sendto(p.fd, pkt, 0, sa)
		if err != nil {
			p.mu.Lock()
			delete(p.waiting, seq)
			p.mu.Unlock()
			// Unreachable network etc.: count as loss, keep going.
			if i < count-1 {
				time.Sleep(interval)
			}
			continue
		}
		select {
		case rtt := <-w.ch:
			res.Received++
			rtts = append(rtts, rtt)
		case <-time.After(timeout):
		}
		p.mu.Lock()
		delete(p.waiting, seq)
		p.mu.Unlock()
		if i < count-1 {
			time.Sleep(interval)
		}
	}
	if res.Sent > 0 {
		res.LossPct = float64(res.Sent-res.Received) * 100 / float64(res.Sent)
	}
	if len(rtts) > 0 {
		res.MinRTT, res.MaxRTT = rtts[0], rtts[0]
		var sum time.Duration
		for _, r := range rtts {
			sum += r
			if r < res.MinRTT {
				res.MinRTT = r
			}
			if r > res.MaxRTT {
				res.MaxRTT = r
			}
		}
		res.AvgRTT = sum / time.Duration(len(rtts))
		if len(rtts) > 1 {
			var jit time.Duration
			for i := 1; i < len(rtts); i++ {
				d := rtts[i] - rtts[i-1]
				if d < 0 {
					d = -d
				}
				jit += d
			}
			res.Jitter = jit / time.Duration(len(rtts)-1)
		}
	}
	return res, nil
}
