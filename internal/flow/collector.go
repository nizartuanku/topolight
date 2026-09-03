package flow

import (
	"context"
	"log"
	"net"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/store"
)

// Collector binds the NetFlow/IPFIX and sFlow ports and feeds the aggregator.
type Collector struct {
	st     *store.Store
	Parser *Parser
	Agg    *Aggregator

	// Forward, when set, ships raw datagrams to the leader instead of parsing them here.
	Forward func(from string, sflow bool, raw []byte)

	mu       sync.Mutex
	Received int64            // datagrams
	Unknown  map[string]int64 // exporters that are not devices (ip → datagrams)
}

// New wires a collector to the store (for exporter → device lookups).
func New(st *store.Store, agg *Aggregator) *Collector {
	return &Collector{st: st, Parser: NewParser(), Agg: agg, Unknown: map[string]int64{}}
}

// ListenAndServe binds netflowAddr (":2055") and sflowAddr (":6343"); either
// may be "" to disable. Returns when ctx ends or a bind fails.
func (c *Collector) ListenAndServe(ctx context.Context, netflowAddr, sflowAddr string) error {
	var pcs []net.PacketConn
	if netflowAddr != "" {
		pc, err := net.ListenPacket("udp", netflowAddr)
		if err != nil {
			return err
		}
		pcs = append(pcs, pc)
		go c.serve(ctx, pc, false)
	}
	if sflowAddr != "" {
		pc, err := net.ListenPacket("udp", sflowAddr)
		if err != nil {
			for _, p := range pcs {
				p.Close()
			}
			return err
		}
		pcs = append(pcs, pc)
		go c.serve(ctx, pc, true)
	}
	// minute ticker closes idle buckets and flushes the journal
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if c.Agg != nil {
					c.Agg.Tick(now)
				}
			}
		}
	}()
	<-ctx.Done()
	for _, p := range pcs {
		p.Close()
	}
	return nil
}

func (c *Collector) serve(ctx context.Context, pc net.PacketConn, sflow bool) {
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		ip := from.(*net.UDPAddr).IP.String()
		if c.Forward != nil {
			c.mu.Lock()
			c.Received++
			c.mu.Unlock()
			c.Forward(ip, sflow, append([]byte(nil), buf[:n]...))
			continue
		}
		c.Feed(ip, sflow, buf[:n])
	}
}

// Feed parses one datagram from ip (used by the listener and by the cluster ingest).
func (c *Collector) Feed(ip string, sflow bool, b []byte) {
	{
		now := time.Now()
		var recs []Record
		if sflow {
			recs, _ = c.Parser.ParseSFlow(b)
		} else {
			recs, _ = c.Parser.Parse(ip, b, now)
		}
		c.mu.Lock()
		c.Received++
		if _, ok := c.st.DeviceByIP(ip); !ok {
			c.Unknown[ip]++
			if len(c.Unknown) > 1000 { // never grow without bound
				for k := range c.Unknown {
					delete(c.Unknown, k)
					break
				}
			}
		}
		c.mu.Unlock()
		c.Agg.Add(ip, recs, now)
	}
}

// Stats for Admin → System.
func (c *Collector) Stats() map[string]any {
	c.mu.Lock()
	unknown := len(c.Unknown)
	recv := c.Received
	c.mu.Unlock()
	c.Parser.mu.Lock()
	m := map[string]any{"datagrams": recv, "records": c.Parser.Records, "no_template": c.Parser.NoTemplate, "malformed": c.Parser.Malformed, "unknown_exporters": unknown}
	c.Parser.mu.Unlock()
	for k, v := range c.Agg.Stats() {
		m[k] = v
	}
	return m
}

// LogUnknown prints a one-line hint for exporters that are not in the inventory.
func (c *Collector) LogUnknown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ip, n := range c.Unknown {
		log.Printf("flow: %d datagram(s) from %s, which is not a monitored device — add it (or its loopback address) as a device to see its traffic by name", n, ip)
	}
}
