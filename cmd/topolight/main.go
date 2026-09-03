// Command topolight is the whole product: collectors, state engine, storage
// and console in one static binary.
package main

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nizartuanku/topolight/internal/backup"
	"github.com/nizartuanku/topolight/internal/cluster"
	"github.com/nizartuanku/topolight/internal/discovery"
	"github.com/nizartuanku/topolight/internal/endpoint"
	"github.com/nizartuanku/topolight/internal/flow"
	"github.com/nizartuanku/topolight/internal/icmp"
	"github.com/nizartuanku/topolight/internal/integ"
	"github.com/nizartuanku/topolight/internal/license"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/notify"
	"github.com/nizartuanku/topolight/internal/poller"
	"github.com/nizartuanku/topolight/internal/probe"
	"github.com/nizartuanku/topolight/internal/profile"
	"github.com/nizartuanku/topolight/internal/report"
	"github.com/nizartuanku/topolight/internal/selfcert"
	"github.com/nizartuanku/topolight/internal/state"
	"github.com/nizartuanku/topolight/internal/store"
	"github.com/nizartuanku/topolight/internal/syslog"
	"github.com/nizartuanku/topolight/internal/topology"
	"github.com/nizartuanku/topolight/internal/trap"
	"github.com/nizartuanku/topolight/internal/tsdb"
	"github.com/nizartuanku/topolight/internal/version"
	"github.com/nizartuanku/topolight/internal/webui"
)

func main() {
	var (
		listen        = flag.String("listen", fmt.Sprintf("127.0.0.1:%d", version.Port), "address of the console (use 0.0.0.0:8433 to serve the network)")
		dataDir       = flag.String("data", defaultDataDir(), "directory for state, metrics and logs")
		memory        = flag.Bool("memory", false, "keep everything in memory only; nothing is written to disk")
		licKey        = flag.String("license", os.Getenv("TOPOLIGHT_LICENSE"), "licence key (or TOPOLIGHT_LICENSE, or <data>/license.key)")
		syslogAddr    = flag.String("syslog-listen", ":514", "syslog UDP+TCP listen address (\"\" disables)")
		trapAddr      = flag.String("trap-listen", ":162", "SNMP trap UDP listen address (\"\" disables)")
		trapComm      = flag.String("trap-community", "", "require this community on incoming v2c traps (default: accept any)")
		flowAddr      = flag.String("flow-listen", ":2055", "NetFlow v5/v9 + IPFIX UDP listen address (\"\" disables)")
		sflowAddr     = flag.String("sflow-listen", ":6343", "sFlow v5 UDP listen address (\"\" disables)")
		noICMP        = flag.Bool("no-icmp", false, "disable ICMP probes (SNMP reachability only)")
		workers       = flag.Int("workers", 48, "concurrent device polls")
		rawDays       = flag.Int("raw-days", 7, "days of 60-second samples to keep before only 5-minute rollups remain")
		tlsCert       = flag.String("tls-cert", "", "PEM certificate to serve HTTPS")
		tlsKey        = flag.String("tls-key", "", "PEM key for -tls-cert")
		syslogTLS     = flag.String("syslog-tls-listen", ":6514", "syslog over TLS (RFC 5425) listen address (\"\" disables)")
		syslogTLSCert = flag.String("syslog-tls-cert", "", "PEM certificate for syslog TLS (default: -tls-cert, else a self-signed one in <data>/syslog-tls.crt)")
		syslogTLSKey  = flag.String("syslog-tls-key", "", "PEM key for -syslog-tls-cert")
		syslogTLSCA   = flag.String("syslog-tls-client-ca", "", "PEM CA bundle; when set, senders must present a client certificate signed by it")
		consoleURL    = flag.String("console-url", "", "base URL used in notification links")
		smtpHost      = flag.String("smtp-host", "", "SMTP host for e-mail notifications")
		smtpPort      = flag.Int("smtp-port", 587, "SMTP port")
		smtpUser      = flag.String("smtp-user", "", "SMTP username")
		smtpPass      = flag.String("smtp-pass", os.Getenv("TOPOLIGHT_SMTP_PASS"), "SMTP password (or TOPOLIGHT_SMTP_PASS)")
		smtpFrom      = flag.String("smtp-from", "", "From address for notifications")
		smtpTLS       = flag.Bool("smtp-starttls", true, "upgrade the SMTP connection with STARTTLS")
		tgToken       = flag.String("telegram-token", os.Getenv("TOPOLIGHT_TELEGRAM_TOKEN"), "Telegram bot token (or TOPOLIGHT_TELEGRAM_TOKEN)")
		whSecret      = flag.String("webhook-secret", os.Getenv("TOPOLIGHT_WEBHOOK_SECRET"), "HMAC secret for signed webhooks (or TOPOLIGHT_WEBHOOK_SECRET)")
		showVer       = flag.Bool("version", false, "print version and exit")
		clListen      = flag.String("cluster-listen", ":8434", "cluster (node-to-node, mTLS) listen address")
		clAdvertise   = flag.String("cluster-advertise", "", "URL other nodes use to reach this node's cluster port (default https://<detected ip>:8434)")
		clConsole     = flag.String("cluster-console", "", "URL other nodes use to reach this node's console (default http://<detected ip>:8433)")
		clJoin        = flag.String("join", "", "join an existing cluster: cluster URL of any full node, e.g. https://node1:8434 (first start only)")
		clToken       = flag.String("join-token", os.Getenv("TOPOLIGHT_JOIN_TOKEN"), "join token from Admin → Cluster on the existing node (or TOPOLIGHT_JOIN_TOKEN)")
		clRole        = flag.String("node-role", "full", "role when joining: full (data copy, can lead) or collector (poll + forward only)")
		clName        = flag.String("node-name", "", "node name shown in the cluster (default: hostname)")
		clPromote     = flag.Bool("promote", false, "force this standby node to become leader (2-node clusters, or when the majority is lost for good), then exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Printf("%s %s\n", version.Product, version.Version)
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("topolight: ")

	dir := *dataDir
	if *memory {
		dir = ""
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("data dir %s: %v", dir, err)
	}

	// cluster role decides which program this process runs
	ident, mode := prepareCluster(dir, clusterOpts{listen: *clListen, advertise: *clAdvertise, console: *clConsole, join: *clJoin, token: *clToken, role: *clRole, name: *clName})
	if *clPromote {
		if ident == nil || !ident.Enabled {
			log.Fatalf("-promote: this node is not part of a cluster")
		}
		ident.WasLeader = true
		ident.Term++
		if err := ident.Save(); err != nil {
			log.Fatal(err)
		}
		fmt.Println("node marked as leader; start the service and it will claim leadership (make sure the old leader is really down — two leaders corrupt nothing but alert twice)")
		return
	}
	if mode == "standby" || mode == "collector" {
		runStandby(dir, mode, ident, clusterOpts{listen: *clListen}, *listen, *syslogAddr, *trapAddr, *flowAddr, *sflowAddr, *noICMP, *workers)
		return
	}

	// licence
	key := strings.TrimSpace(*licKey)
	if key == "" && dir != "" {
		key = webui.ReadLicenseKey(dir)
	}
	lic := license.Resolve(key)
	licState := &lic
	log.Printf("%s %s — %s", version.Product, version.Version, lic.Notice)

	st, err := store.Open(dir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	tsOpts := tsdb.Options{RawDays: *rawDays, RetentionDays: lic.Caps.RetentionDays}
	if ident != nil && ident.Enabled {
		tsOpts.CheckpointMinutes = 1 // a standby's copy is at most a minute behind on metrics
	}
	if tsOpts.RawDays > tsOpts.RetentionDays {
		tsOpts.RawDays = tsOpts.RetentionDays
	}
	tsDir := ""
	if dir != "" {
		tsDir = filepath.Join(dir, "tsdb")
	}
	db, err := tsdb.Open(tsDir, tsOpts)
	if err != nil {
		log.Fatalf("tsdb: %v", err)
	}
	defer db.Close()

	logDir := ""
	if dir != "" {
		logDir = filepath.Join(dir, "logs")
	}
	logs, err := syslog.OpenLogStore(logDir)
	if err != nil {
		log.Fatalf("logs: %v", err)
	}
	defer logs.Close()

	profDir := ""
	if dir != "" {
		profDir = filepath.Join(dir, "profiles")
		_ = os.MkdirAll(profDir, 0o700)
	}
	lib := profile.Load(profDir)

	flowDir := ""
	if dir != "" {
		flowDir = filepath.Join(dir, "flow")
	}
	flows, err := flow.NewAggregator(flowDir)
	if err != nil {
		log.Fatalf("flow: %v", err)
	}
	defer flows.Close()
	eps, err := endpoint.Open(dir)
	if err != nil {
		log.Fatalf("endpoints: %v", err)
	}
	defer eps.Close()

	var ping *icmp.Pinger
	icmpErr := ""
	if !*noICMP {
		ping, err = icmp.New()
		if err != nil {
			icmpErr = err.Error()
			log.Printf("WARNING: %v — continuing with SNMP-only reachability", err)
		}
	} else {
		icmpErr = "disabled with -no-icmp"
	}

	caps := func() license.Caps { return licState.Caps }
	pl := poller.New(st, db, lib, ping)
	pl.Workers = *workers
	pl.Endpoints = eps
	disc := discovery.New(st, lib, ping, caps)
	topo := topology.New(st, lib, pl)
	eng := state.New(st)
	sys := syslog.New(st, logs)
	tr := trap.New(st, logs)
	tr.Community = *trapComm
	{
		// SNMPv3 engine identity for informs: persistent id, boots++ per start
		set := st.Settings()
		if set.EngineID == "" {
			b := make([]byte, 8)
			if _, err := crand.Read(b); err == nil {
				set.EngineID = "80001f8805" + hex.EncodeToString(b) // format 5 (octets) under 8072
			}
		}
		set.EngineBoots++
		st.SetSettings(set)
		if id, err := hex.DecodeString(set.EngineID); err == nil {
			tr.V3.SetEngine(id, set.EngineBoots)
		}
	}
	fc := flow.New(st, flows)
	cfgStore, err := backup.Open(dir)
	if err != nil {
		log.Fatalf("configs: %v", err)
	}
	bk := backup.New(st, cfgStore)
	pr := probe.New(st, db, ping)
	if tracer, err := probe.NewTracer(); err == nil {
		pr.Traceroute = tracer
	} else {
		log.Printf("traceroute probes unavailable: %v", err)
	}
	tr.PollNow = pl.PollNow
	ig := integ.New(st, db, pl.DeviceSamples)
	ig.Caps = func() (int, int) { _, mon := st.DeviceCount(); return licState.Caps.MaxDevices, mon }
	pl.Caps = ig.Caps
	disp := notify.New(st, caps)
	disp.SMTP = notify.SMTP{Host: *smtpHost, Port: *smtpPort, User: *smtpUser, Pass: *smtpPass, From: *smtpFrom, StartTLS: *smtpTLS}
	disp.Telegram = notify.Telegram{Token: *tgToken}
	rp := report.NewRunner(report.Deps{Store: st, DB: db, Backup: cfgStore, Flow: flows, Endpoints: eps, Probes: pr, Instance: st.Settings().InstanceName}, disp, dir)
	disp.WebhookSecret = *whSecret
	disp.ConsoleURL = *consoleURL
	if disp.ConsoleURL == "" {
		disp.ConsoleURL = st.Settings().ConsoleURL
	}

	// follow CDP-learned neighbours into discovery
	topo.OnNewNeighbor = func(ip, name, siteID string) {
		if net.ParseIP(ip) == nil {
			return
		}
		if _, exists := st.DeviceByIP(ip); exists {
			return
		}
		go func() {
			for _, cred := range st.Creds() {
				c := poller.NewClient(ip, cred)
				c.Timeout = time.Second
				c.Retries = 0
				vbs, err := c.Get(profile.OIDSysName, profile.OIDSysDescr, profile.OIDSysObjectID)
				c.Close()
				if err == nil && len(vbs) == 3 {
					disc.Register(siteID, ip, vbs[0].Value.String(), vbs[1].Value.String(), vbs[2].Value.OID, cred.ID, "neighbor", nil)
					return
				}
			}
		}()
	}

	eng.Devices = pl.DeviceSamples
	eng.Interfaces = pl.InterfaceSamples
	eng.Events = []<-chan model.Event{pl.Events, sys.Events, tr.Events, topo.Events, disc.Events, disp.Events, pr.Events, bk.Events, ig.Events}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pl.Run(ctx)
	go pr.Run(ctx)
	go bk.Run(ctx)
	go rp.Run(ctx)
	go ig.Run(ctx)
	go eng.Run(ctx)
	// a "config changed" syslog line schedules a backup a couple of minutes later
	go func() {
		ch := eng.Subscribe()
		defer eng.Unsubscribe(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-ch:
				if c.Type != "event" {
					continue
				}
				if ev, ok := c.Data.(model.Event); ok && ev.Kind == "config_changed" && ev.DeviceID != "" {
					bk.Trigger(ev.DeviceID)
				}
			}
		}
	}()
	go disp.Run(ctx, eng.Notify)
	if *syslogAddr != "" {
		go func() {
			if err := sys.ListenAndServe(ctx, *syslogAddr); err != nil {
				log.Printf("WARNING: syslog listener on %s: %v — run as root, `setcap cap_net_bind_service+ep topolight`, or use -syslog-listen :5514", *syslogAddr, err)
			}
		}()
	}
	if *syslogTLS != "" {
		cert, key := *syslogTLSCert, *syslogTLSKey
		if cert == "" {
			cert, key = *tlsCert, *tlsKey
		}
		var tcfg *tls.Config
		if cert != "" {
			if c, err := tls.LoadX509KeyPair(cert, key); err == nil {
				tcfg = &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12}
			} else {
				log.Printf("WARNING: syslog TLS certificate: %v — falling back to a self-signed one", err)
			}
		}
		if tcfg == nil && dir != "" {
			host, _ := os.Hostname()
			if c, fp, created, err := selfcert.Load(dir, "syslog-tls", host); err == nil {
				tcfg = &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12}
				if created {
					log.Printf("syslog TLS: created self-signed certificate %s/syslog-tls.crt", dir)
				}
				log.Printf("syslog TLS: certificate SHA-256 fingerprint %s — pin it on senders, or replace with -syslog-tls-cert/-key", fp)
			} else {
				log.Printf("WARNING: syslog TLS: %v", err)
			}
		}
		if tcfg != nil {
			if *syslogTLSCA != "" {
				pool := x509.NewCertPool()
				if pemb, err := os.ReadFile(*syslogTLSCA); err == nil && pool.AppendCertsFromPEM(pemb) {
					tcfg.ClientCAs = pool
					tcfg.ClientAuth = tls.RequireAndVerifyClientCert
				} else {
					log.Printf("WARNING: -syslog-tls-client-ca %s unreadable or empty — client certificates NOT required", *syslogTLSCA)
				}
			}
			go func() {
				if err := sys.ListenAndServeTLS(ctx, *syslogTLS, tcfg); err != nil {
					log.Printf("WARNING: syslog TLS listener on %s: %v", *syslogTLS, err)
				}
			}()
		}
	}
	if *trapAddr != "" {
		go func() {
			if err := tr.ListenAndServe(ctx, *trapAddr); err != nil {
				log.Printf("WARNING: trap listener on %s: %v — run as root, `setcap cap_net_bind_service+ep topolight`, or use -trap-listen :1162", *trapAddr, err)
			}
		}()
	}
	if *flowAddr != "" || *sflowAddr != "" {
		go func() {
			if err := fc.ListenAndServe(ctx, *flowAddr, *sflowAddr); err != nil {
				log.Printf("WARNING: flow listener (%s / %s): %v — use -flow-listen/-sflow-listen to pick other ports", *flowAddr, *sflowAddr, err)
			}
		}()
	}
	// periodic discovery, topology, retention
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		lastDisc, lastTopo, lastPrune := time.Now(), time.Time{}, time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				set := st.Settings()
				if set.TopologyEvery > 0 && now.Sub(lastTopo) >= time.Duration(set.TopologyEvery)*time.Minute {
					lastTopo = now
					topo.Collect(ctx)
					eng.InvalidateTopology()
					eng.Broadcast(state.Change{Type: "topology", Data: map[string]any{}})
				}
				if set.DiscoveryEvery > 0 && now.Sub(lastDisc) >= time.Duration(set.DiscoveryEvery)*time.Minute {
					lastDisc = now
					for _, s := range st.Sites() {
						if !s.Disabled && len(s.Subnets) > 0 {
							if p, err := disc.Sweep(ctx, s.ID); err == nil {
								eng.Broadcast(state.Change{Type: "discovery", Data: p})
							}
						}
					}
				}
				if now.Sub(lastPrune) >= 6*time.Hour {
					lastPrune = now
					keep := time.Duration(licState.Caps.RetentionDays) * 24 * time.Hour
					logs.Prune(keep)
					st.PruneJournals(keep)
					flows.Prune(keep)
					if keep < 90*24*time.Hour {
						keep = 90 * 24 * time.Hour // endpoints are cheap; keep the "where was this MAC" answer longer
					}
					eps.Prune(keep, now)
				}
				if err := eps.Flush(false); err != nil {
					log.Printf("endpoints: %v", err)
				}
			}
		}
	}()
	// first topology pass shortly after start, once polls have run
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
			topo.Collect(ctx)
			eng.InvalidateTopology()
			eng.Broadcast(state.Change{Type: "topology", Data: map[string]any{}})
		}
	}()

	// cluster: this process is the leader (or a standalone that may enable clustering later)
	var node *cluster.Node
	startNode := func() error {
		hooks := leaderHooks(leaderDeps{st: st, pl: pl, db: db, sys: sys, tr: tr, fc: fc, dir: dir, stop: func() {
			cancel()
			st.FlushEvents()
			_ = db.Close()
			_ = st.Close()
			logs.Close()
		}})
		n, err := cluster.New(ident, "leader", hooks)
		if err != nil {
			return err
		}
		node = n
		go func() {
			if err := n.Serve(ctx, *clListen); err != nil {
				log.Printf("WARNING: cluster listener on %s: %v", *clListen, err)
			}
		}()
		return nil
	}
	if ident != nil && ident.Enabled {
		pl.InventoryAll = true
		if err := startNode(); err != nil {
			log.Fatalf("cluster: %v", err)
		}
	}
	ctl := &webui.ClusterCtl{
		Ident: ident,
		Node:  func() *cluster.Node { return node },
		Enable: func() error {
			if ident == nil {
				return fmt.Errorf("clustering needs a data directory")
			}
			if ident.Enabled {
				return nil
			}
			if err := ident.InitCA(); err != nil {
				return err
			}
			ident.WasLeader = true
			ident.Upsert(ident.Self())
			if err := ident.Save(); err != nil {
				return err
			}
			pl.InventoryAll = true
			return startNode()
		},
	}

	srv := webui.New(webui.Deps{Store: st, DB: db, Logs: logs, Poller: pl, Discovery: disc, Topology: topo, Engine: eng, Notify: disp, Profiles: lib, Cluster: ctl,
		Syslog: sys, Trap: tr, Flow: fc, FlowAddr: *flowAddr, SFlowAddr: *sflowAddr, Endpoints: eps, Probes: pr, Backup: bk, Reports: rp, Integ: ig, DataDir: dir, Started: time.Now(), Listen: *listen, SyslogAddr: *syslogAddr, SyslogTLSAddr: *syslogTLS, TrapAddr: *trapAddr, ICMPError: icmpErr,
		License: func() license.State { return *licState },
		SetLicense: func(k string) license.State {
			s := license.Resolve(k)
			if s.Valid || k == "" {
				_ = webui.WriteLicenseKey(dir, k)
			}
			*licState = s
			log.Printf("licence updated — %s", s.Notice)
			return s
		}})
	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		var err error
		scheme := "http"
		if *tlsCert != "" {
			scheme = "https"
		}
		log.Printf("console listening on %s://%s", scheme, *listen)
		if *tlsCert != "" {
			err = httpSrv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("console: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Print("shutting down")
	cancel()
	shutCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	_ = httpSrv.Shutdown(shutCtx)
	st.FlushEvents()
}

func defaultDataDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".topolight")
	}
	return ".topolight"
}
