package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nizartuanku/topolight/internal/cluster"
	"github.com/nizartuanku/topolight/internal/flow"
	"github.com/nizartuanku/topolight/internal/icmp"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/poller"
	"github.com/nizartuanku/topolight/internal/profile"
	"github.com/nizartuanku/topolight/internal/store"
	"github.com/nizartuanku/topolight/internal/syslog"
	"github.com/nizartuanku/topolight/internal/trap"
	"github.com/nizartuanku/topolight/internal/version"
	"github.com/nizartuanku/topolight/internal/webui"
)

// clusterOpts are the cluster-related flags.
type clusterOpts struct {
	listen, advertise, join, token, role, name string
	console                                    string // this node's console URL as seen by peers
}

// prepareCluster loads the node identity, performs a first-time join when
// asked, and returns the identity plus the process mode:
// "standalone" | "leader" | "standby" | "collector".
func prepareCluster(dir string, o clusterOpts) (*cluster.Identity, string) {
	if dir == "" {
		return nil, "standalone"
	}
	name := o.name
	if name == "" {
		name, _ = os.Hostname()
	}
	ident, err := cluster.LoadIdentity(dir, name)
	if err != nil {
		log.Fatalf("cluster: %v", err)
	}
	if o.advertise != "" {
		ident.Addr = o.advertise
	} else if ident.Addr == "" {
		ident.Addr = "https://" + net.JoinHostPort(localIP(), portOf(o.listen, "8434"))
	}
	if o.console != "" {
		ident.Console = o.console
	} else if ident.Console == "" {
		ident.Console = "http://" + net.JoinHostPort(localIP(), portOf(os.Getenv("TOPOLIGHT_CONSOLE_PORT"), fmt.Sprint(version.Port)))
	}
	if o.role == cluster.RoleCollector && !ident.Enabled {
		ident.Role = cluster.RoleCollector
	}
	if o.join != "" && !ident.Enabled {
		if o.token == "" {
			log.Fatalf("cluster: -join needs -join-token")
		}
		log.Printf("cluster: joining %s as %s", o.join, ident.Role)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := cluster.Join(ctx, ident, o.join, o.token)
		cancel()
		if err != nil {
			log.Fatalf("cluster: join failed: %v", err)
		}
		log.Printf("cluster: joined as %s (%s); %d member(s)", ident.Role, ident.ID, len(ident.MemberList()))
	}
	_ = ident.Save()
	switch {
	case !ident.Enabled:
		return ident, "standalone"
	case ident.Role == cluster.RoleCollector:
		return ident, "collector"
	case ident.WasLeader:
		return ident, "leader"
	default:
		return ident, "standby"
	}
}

func portOf(listen, def string) string {
	if _, p, err := net.SplitHostPort(listen); err == nil && p != "" {
		return p
	}
	if listen != "" && !strings.Contains(listen, ":") {
		return listen
	}
	return def
}

// localIP picks the address a peer would use to reach this host.
func localIP() string {
	if c, err := net.Dial("udp", "10.255.255.255:9"); err == nil {
		defer c.Close()
		if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
			return a.IP.String()
		}
	}
	return "127.0.0.1"
}

// restartProcess re-executes the binary with the same arguments so the new
// role is picked up by the normal startup path.
func restartProcess(why string, cleanup func()) {
	log.Printf("cluster: restarting — %s", why)
	if cleanup != nil {
		cleanup()
	}
	if runtime.GOOS == "linux" {
		if exe, err := os.Executable(); err == nil {
			_ = syscall.Exec(exe, os.Args, os.Environ())
		}
	}
	os.Exit(0) // the service manager restarts us
}

// dataTS reports the freshness of the local copy (state.json mtime).
func dataTS(dir string) func() time.Time {
	return func() time.Time {
		fi, err := os.Stat(filepath.Join(dir, "state.json"))
		if err != nil {
			return time.Time{}
		}
		return fi.ModTime()
	}
}

// leaderHooks wires the running full stack to the cluster node.
type leaderDeps struct {
	st   *store.Store
	pl   *poller.Poller
	db   poller.MetricSink
	sys  *syslog.Receiver
	tr   *trap.Receiver
	fc   *flow.Collector
	dir  string
	stop func()
}

func leaderHooks(d leaderDeps) cluster.Hooks {
	return cluster.Hooks{
		Version: version.Version,
		DataDir: d.dir,
		DataTS:  dataTS(d.dir),
		Assigned: func() int {
			if n := d.pl.Assigned(); n >= 0 {
				return n
			}
			_, m := d.st.DeviceCount()
			return m
		},
		Devices: func() []cluster.DeviceRef {
			var out []cluster.DeviceRef
			for _, dev := range d.st.Devices() {
				if dev.Monitored {
					out = append(out, cluster.DeviceRef{ID: dev.ID, Site: dev.SiteID})
				}
			}
			return out
		},
		OnAssign: func(ids []string, _ map[string]string) { d.pl.SetAssigned(ids) },
		Demote: func() {
			restartProcess("lost leadership", d.stop)
		},
		Ingest: func(b cluster.Batch) error {
			for _, raw := range b.Devices {
				var s model.DeviceSample
				if json.Unmarshal(raw, &s) == nil {
					select {
					case d.pl.DeviceSamples <- s:
					default:
					}
				}
			}
			for _, raw := range b.Ifaces {
				var s model.InterfaceSample
				if json.Unmarshal(raw, &s) == nil {
					select {
					case d.pl.InterfaceSamples <- s:
					default:
					}
				}
			}
			for _, raw := range b.Events {
				var e model.Event
				if json.Unmarshal(raw, &e) == nil {
					select {
					case d.pl.Events <- e:
					default:
					}
				}
			}
			for _, m := range b.Metrics {
				d.db.Append(m.S, m.T, m.V)
			}
			for _, l := range b.Logs {
				d.sys.Handle(l.Host, l.Raw)
			}
			for _, t := range b.Traps {
				d.tr.HandleRaw(t.From, t.Data)
			}
			for _, f := range b.Flows {
				d.fc.Feed(f.From, f.SFlow, f.Data)
			}
			return nil
		},
	}
}

// runStandby is the whole program for a standby or collector node: mirror
// the leader's data (standby), poll the assigned share of devices, forward
// everything to the leader, proxy the console, and take over when elected.
func runStandby(dir, mode string, ident *cluster.Identity, o clusterOpts, listen, syslogAddr, trapAddr, flowAddr, sflowAddr string, noICMP bool, workers int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// read-only view of the leader's snapshot (collectors get state.json only)
	st, err := store.OpenReadOnly(dir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	lib := profile.Load(filepath.Join(dir, "profiles"))
	var ping *icmp.Pinger
	if !noICMP {
		if p, err := icmp.New(); err == nil {
			ping = p
		} else {
			log.Printf("WARNING: %v", err)
		}
	}
	var node *cluster.Node
	var fw *cluster.Forwarder
	pl := poller.New(st, nil, lib, ping)
	pl.Workers = workers
	pl.NoRouting = true        // the leader keeps protocol state; standbys only forward samples
	pl.SetAssigned([]string{}) // nothing until the leader assigns
	stop := func() { cancel(); time.Sleep(300 * time.Millisecond) }
	hooks := cluster.Hooks{Version: version.Version, DataDir: dir, DataTS: dataTS(dir),
		Assigned: func() int {
			n := pl.Assigned()
			if n < 0 {
				return 0
			}
			return n
		},
		Queue: func() int {
			if fw == nil {
				return 0
			}
			return fw.Queue()
		},
		OnAssign: func(ids []string, _ map[string]string) { pl.SetAssigned(ids) },
		Promote:  func() { restartProcess("elected leader", stop) },
	}
	node, err = cluster.New(ident, mode, hooks)
	if err != nil {
		log.Fatalf("cluster: %v", err)
	}
	fw = cluster.NewForwarder(node)
	pl.SetSink(fw)
	go func() {
		if err := node.Serve(ctx, o.listen); err != nil {
			log.Fatalf("cluster listener on %s: %v", o.listen, err)
		}
	}()
	mirror := cluster.NewMirror(node, dir)
	if mode == "collector" {
		mirror.Only = []string{"state.json", "profiles/"}
	}
	mirror.OnChange = func(changed []string) {
		for _, c := range changed {
			if c == "state.json" {
				if err := st.Reload(); err != nil {
					log.Printf("cluster: reload snapshot: %v", err)
				}
			}
		}
	}
	// wait briefly for a leader, then bootstrap the copy
	for i := 0; i < 20; i++ {
		if _, ok := node.Leader(); ok {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := mirror.Bootstrap(ctx); err != nil {
		log.Printf("cluster: initial sync incomplete: %v (will keep trying)", err)
	}
	go mirror.Run(ctx)
	go fw.Run(ctx)
	go pl.Run(ctx)
	// drain the poller's outputs into the forwarder
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-pl.DeviceSamples:
				fw.Device(s)
			case s := <-pl.InterfaceSamples:
				fw.Iface(s)
			case e := <-pl.Events:
				fw.Event(e)
			}
		}
	}()
	// listeners forward raw input
	if syslogAddr != "" {
		sys := syslog.New(st, nil)
		sys.Forward = fw.Log
		go func() {
			if err := sys.ListenAndServe(ctx, syslogAddr); err != nil {
				log.Printf("WARNING: syslog listener on %s: %v", syslogAddr, err)
			}
		}()
	}
	if trapAddr != "" {
		tr := trap.New(st, nil)
		tr.Forward = fw.Trap
		go func() {
			if err := tr.ListenAndServe(ctx, trapAddr); err != nil {
				log.Printf("WARNING: trap listener on %s: %v", trapAddr, err)
			}
		}()
	}
	if flowAddr != "" || sflowAddr != "" {
		fc := flow.New(st, nil)
		fc.Forward = fw.Flow
		go func() {
			if err := fc.ListenAndServe(ctx, flowAddr, sflowAddr); err != nil {
				log.Printf("WARNING: flow listener: %v", err)
			}
		}()
	}
	// console: proxy to the leader
	h := webui.StandbyHandler(webui.StandbyDeps{
		Leader: func() (string, bool) {
			l, ok := node.Leader()
			return l.Console, ok
		},
		Status: func() any {
			s := node.Status()
			return map[string]any{"node": s, "forwarder": fw.Stats(), "mirror": mirror.Stats(), "version": version.Version}
		},
	})
	httpSrv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("%s node %s (%s): console on http://%s proxies to the leader; cluster port %s", mode, ident.Name, ident.ID, listen, o.listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("console: %v", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Print("shutting down")
	cancel()
	shutCtx, c2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer c2()
	_ = httpSrv.Shutdown(shutCtx)
	fw.Flush(context.Background())
}
