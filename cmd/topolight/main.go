// Command topolight is the whole product: collectors, state engine, storage
// and console in one static binary.
package main

import (
	"context"
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

	"github.com/nizartuanku/topolight/internal/discovery"
	"github.com/nizartuanku/topolight/internal/icmp"
	"github.com/nizartuanku/topolight/internal/license"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/notify"
	"github.com/nizartuanku/topolight/internal/poller"
	"github.com/nizartuanku/topolight/internal/profile"
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
		listen     = flag.String("listen", fmt.Sprintf("127.0.0.1:%d", version.Port), "address of the console (use 0.0.0.0:8432 to serve the network)")
		dataDir    = flag.String("data", defaultDataDir(), "directory for state, metrics and logs")
		memory     = flag.Bool("memory", false, "keep everything in memory only; nothing is written to disk")
		licKey     = flag.String("license", os.Getenv("TOPOLIGHT_LICENSE"), "licence key (or TOPOLIGHT_LICENSE, or <data>/license.key)")
		syslogAddr = flag.String("syslog-listen", ":514", "syslog UDP+TCP listen address (\"\" disables)")
		trapAddr   = flag.String("trap-listen", ":162", "SNMP trap UDP listen address (\"\" disables)")
		trapComm   = flag.String("trap-community", "", "require this community on incoming v2c traps (default: accept any)")
		noICMP     = flag.Bool("no-icmp", false, "disable ICMP probes (SNMP reachability only)")
		workers    = flag.Int("workers", 48, "concurrent device polls")
		rawDays    = flag.Int("raw-days", 7, "days of 60-second samples to keep before only 5-minute rollups remain")
		tlsCert    = flag.String("tls-cert", "", "PEM certificate to serve HTTPS")
		tlsKey     = flag.String("tls-key", "", "PEM key for -tls-cert")
		consoleURL = flag.String("console-url", "", "base URL used in notification links")
		smtpHost   = flag.String("smtp-host", "", "SMTP host for e-mail notifications")
		smtpPort   = flag.Int("smtp-port", 587, "SMTP port")
		smtpUser   = flag.String("smtp-user", "", "SMTP username")
		smtpPass   = flag.String("smtp-pass", os.Getenv("TOPOLIGHT_SMTP_PASS"), "SMTP password (or TOPOLIGHT_SMTP_PASS)")
		smtpFrom   = flag.String("smtp-from", "", "From address for notifications")
		smtpTLS    = flag.Bool("smtp-starttls", true, "upgrade the SMTP connection with STARTTLS")
		tgToken    = flag.String("telegram-token", os.Getenv("TOPOLIGHT_TELEGRAM_TOKEN"), "Telegram bot token (or TOPOLIGHT_TELEGRAM_TOKEN)")
		whSecret   = flag.String("webhook-secret", os.Getenv("TOPOLIGHT_WEBHOOK_SECRET"), "HMAC secret for signed webhooks (or TOPOLIGHT_WEBHOOK_SECRET)")
		showVer    = flag.Bool("version", false, "print version and exit")
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
	disc := discovery.New(st, lib, ping, caps)
	topo := topology.New(st, lib, pl)
	eng := state.New(st)
	sys := syslog.New(st, logs)
	tr := trap.New(st, logs)
	tr.Community = *trapComm
	tr.PollNow = pl.PollNow
	disp := notify.New(st, caps)
	disp.SMTP = notify.SMTP{Host: *smtpHost, Port: *smtpPort, User: *smtpUser, Pass: *smtpPass, From: *smtpFrom, StartTLS: *smtpTLS}
	disp.Telegram = notify.Telegram{Token: *tgToken}
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
	eng.Events = []<-chan model.Event{pl.Events, sys.Events, tr.Events, topo.Events, disc.Events, disp.Events}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pl.Run(ctx)
	go eng.Run(ctx)
	go disp.Run(ctx, eng.Notify)
	if *syslogAddr != "" {
		go func() {
			if err := sys.ListenAndServe(ctx, *syslogAddr); err != nil {
				log.Printf("WARNING: syslog listener on %s: %v — run as root, `setcap cap_net_bind_service+ep topolight`, or use -syslog-listen :5514", *syslogAddr, err)
			}
		}()
	}
	if *trapAddr != "" {
		go func() {
			if err := tr.ListenAndServe(ctx, *trapAddr); err != nil {
				log.Printf("WARNING: trap listener on %s: %v — run as root, `setcap cap_net_bind_service+ep topolight`, or use -trap-listen :1162", *trapAddr, err)
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

	srv := webui.New(webui.Deps{Store: st, DB: db, Logs: logs, Poller: pl, Discovery: disc, Topology: topo, Engine: eng, Notify: disp, Profiles: lib,
		Syslog: sys, Trap: tr, DataDir: dir, Started: time.Now(), Listen: *listen, SyslogAddr: *syslogAddr, TrapAddr: *trapAddr, ICMPError: icmpErr,
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
