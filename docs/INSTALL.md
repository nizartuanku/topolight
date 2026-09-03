# TopoLight — Install

TopoLight is one static binary. It needs a Linux box (amd64 or arm64) that can reach your network devices over UDP/161, receive their syslog (514, or 6514 over TLS), traps (162) and flow exports (NetFlow/IPFIX 2055, sFlow 6343), and serve its console on TCP/8433. macOS and Windows builds exist for trying it out on a laptop; they run in SNMP-only reachability mode (no ICMP) — see *Honest limits* in the README.

## 1. Sizing

Measured on a 1,500-device simulated estate polled every 60 s (Team licence): about 0.25 CPU cores on average and ~110 MB RSS. CPU and memory are not the constraint; disk for history is, and it scales with the number of *stored series* — roughly 5 per device (RTT, loss, CPU, memory, temperature) plus 2 per interface (in/out bps; error series only while errors occur).

Measured storage cost (worst case, noisy values): **≈4.5 KB per series per day of 60-second history** and **≈1.8 KB per series per day of 5-minute history**. TopoLight keeps 60-second samples for 7 days (`-raw-days`) and 5-minute avg/min/max after that; interfaces that are not uplinks/LLDP peers/starred are stored at 5-minute resolution from the start. Flow summaries add ≈2 MB per exporter per day (gzipped; ≈7.5 MB for the current, uncompressed day) regardless of how many flows the exporter sends — the tables are top-N, not raw flows.

| Estate | Host | History on disk (worst case) |
|---|---|---|
| 100 devices, 24-port switches, 6 months | 1 vCPU · 1 GB RAM | ~2 GB |
| 500 devices, 48-port switches (Pro, 6 months) | 2 vCPU · 2 GB RAM | ~17 GB |
| 1,500 devices, 48-port switches (Team, 12 months) | 2 vCPU · 4 GB RAM | ~100 GB |

Idle access ports compress far better than the worst case, so real estates typically land at a third of those figures; the Overview header and Admin → System show the live number so you can watch the trend after a week and size the disk from that. Devices you do not care about can be set *not monitored* to keep them out of the budget.

## 1a. Deployment topology: what the server is, and what it is not

**One process, one host.** The `topolight` binary contains everything: discovery, the ICMP pinger, the SNMP poller, the syslog and trap receivers, the topology builder, the state/alert engine, the notifier, the embedded time-series and log stores, and the web console with its live event stream. There is no database server, message bus, cache or agent to install next to it, and no component runs on the monitored devices.

| Mode | Status | What it means |
|---|---|---|
| **Standalone** | shipped | One VM/host (table above) monitors the whole estate over routed IP. The default, and enough for most estates up to 2,000 devices. |
| **Warm standby (host-level)** | works, manual | Hypervisor HA or a scheduled copy of `/var/lib/topolight` (see §8 Backups) to a second host. The data directory is self-contained, so restoring it on another host with the same licence key brings the console back with its history. |
| **Cluster: standby nodes (HA)** | shipped | 2–5 servers joined with one command (§1b). Every *full* node keeps a complete, continuously mirrored copy of the data; one is the leader (state engine, alerts, notifications), the others poll their share of the devices and forward. With **three or more full nodes a new leader is elected automatically** about 20 seconds after the leader disappears; with two, promote the survivor by hand. Open any node's console — standbys proxy to the leader. |
| **Cluster: collectors** | shipped | A node joined with `--role collector` polls the devices assigned to it (pin a site to it), receives syslog/traps/flows locally and forwards everything to the leader — for branches behind NAT or slow WAN. No data copy, no vote. |

A 1,500-device estate fits on one host (¼ core, 110 MB RAM measured), so a cluster is about resilience and remote sites more than raw capacity: the leader still runs the one state engine. What a cluster gives you is polling throughput proportional to the node count and a console that survives a dead server.

## 1b. Cluster: join, fail over, upgrade

```sh
# node1: install as usual, finish the wizard, then Admin → Cluster → "Enable clustering"
# node2, node3: run the command shown by "+ Join token" — it looks like this:
curl -fsSL https://raw.githubusercontent.com/nizartuanku/topolight/main/install.sh | sudo sh -s -- \
  --join https://node1:8434 --token TL-JOIN-…          # add --role collector for a branch collector
```

The join token is single-use, valid 24 hours, and carries the fingerprint of the cluster CA, so the new node pins the right cluster before it sends anything. Nodes talk to each other on **TCP/8434 with mutual TLS** (`-cluster-listen`); the certificate authority lives in `/var/lib/topolight/cluster/node.json` on every full node, so any full node can admit new members after a failover. Set `-cluster-advertise https://<ip>:8434` and `-cluster-console http://<ip>:8433` when the auto-detected address is not the one peers should use.

What is replicated: `state.json` (inventory, alerts, users, settings), the event/alert/log journals, the metric store (checkpointed every minute in a cluster), flow summaries, endpoints, configuration backups and generated reports — every 10 seconds, whole files or journal tails. Per-node things (the node identity, the syslog TLS certificate) are not.

**Failover.** Leader lease of 10 s, heartbeats every 2 s, election after 10–15 s of silence; the new leader restarts itself into the leader role with its mirrored copy and is serving within about 20 s. Devices keep sending syslog/traps/flows to whichever node they were pointed at (standbys forward, and buffer up to 200k items while no leader answers). A returning old leader notices the new one and becomes a standby. Losses on failover: up to 10 s of logs/events, up to 1 minute of metrics, nothing of inventory or configuration history.

**Two full nodes** cannot elect (no majority): when the leader is gone for good, run `topolight -promote` on the survivor (as the service user, with the same `-data`), then start the service. Add a third full node to get automatic failover.

**Rolling upgrade.** Upgrade and restart the standbys first (re-run the installer), then the leader; the standbys elect one of themselves while the leader restarts, and the old leader rejoins as a standby. Mixed versions across one minor release are fine.

**Removing a node.** Admin → Cluster → Remove. The node is shut out immediately (its certificate is refused); reinstall it to join again.

## 2. Install with the script (systemd hosts)

```sh
curl -fsSL https://raw.githubusercontent.com/nizartuanku/topolight/main/install.sh | sudo sh
```

The script downloads the release tarball for your CPU, verifies it against `SHA256SUMS`, installs `/usr/local/bin/topolight`, creates a `topolight` system user, grants the binary `cap_net_raw` (ICMP) and `cap_net_bind_service` (ports 514/162), writes `/etc/topolight/topolight.env` and a hardened systemd unit, and starts the service. Re-running it upgrades the binary and keeps `/var/lib/topolight`.

Offline host: download the tarball on another machine and run `TOPOLIGHT_TARBALL=/path/topolight_0.4.1_linux_amd64.tar.gz sudo sh install.sh`.

Then open `http://<host>:8433` and follow the wizard. The first request creates the admin user — do this straight away.

## 3. Install by hand

```sh
tar -xzf topolight_0.4.1_linux_amd64.tar.gz && cd topolight_0.4.1_linux_amd64
sha256sum -c ../SHA256SUMS --ignore-missing
sudo install -m 0755 topolight /usr/local/bin/topolight
sudo setcap 'cap_net_raw,cap_net_bind_service=+ep' /usr/local/bin/topolight   # ICMP + ports < 1024 without root
topolight -listen 0.0.0.0:8433 -data /var/lib/topolight
```

Without `setcap`, either run as root (not recommended), allow unprivileged ICMP (`sysctl -w net.ipv4.ping_group_range="0 2147483647"`) and use high ports (`-syslog-listen :5514 -trap-listen :1162`), pointing devices at those.

`deploy/topolight.service` is the same unit the installer writes; `/etc/topolight/topolight.env` holds the flags in `TOPOLIGHT_OPTS` and optional `TOPOLIGHT_LICENSE`.

## 4. Docker

```sh
docker build -t topolight:0.4.1 .
docker run -d --name topolight --restart unless-stopped \
  -p 8433:8433 -p 514:514/udp -p 514:514 -p 162:162/udp -p 2055:2055/udp -p 6343:6343/udp -p 6514:6514 \
  -v topolight-data:/data topolight:0.4.1
```

or `docker compose up -d` with the included `docker-compose.yml`. The container runs as an unprivileged user with the two capabilities set on the binary. If your devices send syslog to the *host* IP, the port mappings above are all you need; with `--network host` the container sees devices' real source addresses, which is what TopoLight uses to match syslog/traps to devices — behind a NAT'd bridge they arrive from the Docker gateway and cannot be matched. **Use `--network host` when you can.**

## 5. Flags and environment

```
-listen 127.0.0.1:8433   console; 0.0.0.0:8433 to serve the network
-data ~/.topolight       state, metrics, logs, licence (0700)
-memory                  keep everything in RAM (demos)
-license KEY             or TOPOLIGHT_LICENSE, or <data>/license.key, or Admin → Licence
-syslog-listen :514      syslog UDP + TCP; "" disables
-trap-listen :162        SNMP trap UDP; "" disables
-trap-community NAME     require this community on incoming v2c traps
-flow-listen :2055       NetFlow v5/v9 + IPFIX UDP; "" disables
-sflow-listen :6343      sFlow v5 UDP; "" disables
-syslog-tls-listen :6514 syslog over TLS (RFC 5425); "" disables
-syslog-tls-cert/-key    certificate for it (default: -tls-cert, else a self-signed one created in <data>/syslog-tls.crt — the fingerprint is logged at start)
-syslog-tls-client-ca    PEM CA; when set, senders must present a client certificate
-cluster-listen :8434    node-to-node port (mutual TLS)
-cluster-advertise URL   how peers reach this node's cluster port (default https://<detected ip>:8434)
-cluster-console URL     how peers reach this node's console (default http://<detected ip>:8433)
-join URL -join-token T  join an existing cluster on first start (-node-role full|collector, -node-name)
-promote                 mark this standby as leader (2-node clusters) and exit
-no-icmp                 SNMP-only reachability
-workers 48              concurrent device polls (1,500 devices at 60 s need ~25)
-raw-days 7              days of 60-second samples before rollups only
-tls-cert / -tls-key     serve HTTPS directly
-console-url URL         base URL used in notification links
-smtp-host/-smtp-port/-smtp-user/-smtp-pass/-smtp-from/-smtp-starttls
-telegram-token TOKEN    or TOPOLIGHT_TELEGRAM_TOKEN
-webhook-secret SECRET   or TOPOLIGHT_WEBHOOK_SECRET (HMAC-SHA256 of the body in X-TopoLight-Signature)
-version
```

## 6. Ports and firewall

| Direction | Port | Purpose |
|---|---|---|
| TopoLight → devices | UDP/161 | SNMP polling |
| TopoLight → devices | ICMP echo | reachability, RTT |
| devices → TopoLight | UDP/514, TCP/514 | syslog |
| devices → TopoLight | UDP/162 | SNMP traps / informs |
| devices → TopoLight | TCP/6514 | syslog over TLS |
| devices → TopoLight | UDP/2055 | NetFlow v5/v9, IPFIX |
| devices → TopoLight | UDP/6343 | sFlow v5 |
| you → TopoLight | TCP/8433 | console (put TLS in front) |
| node ↔ node | TCP/8434 | cluster (mutual TLS) — only between TopoLight nodes |
| TopoLight → devices | TCP/22 | SSH configuration backup (only devices with an SSH credential) |
| TopoLight → devices | TCP/6030, 57400, … | gNMI (only devices with a gNMI credential; port per credential) |
| TopoLight → controllers | TCP/443, 8443 | UniFi controller / api.meraki.com (only when an integration is configured) |
| TopoLight → SMTP / api.telegram.org / your webhook | as configured | notifications |

Nothing else. TopoLight never contacts the internet on its own — the Meraki integration is the only outbound cloud API, and only if you add one.

## 6a. Building from source

`go build ./cmd/topolight` needs Go 1.24+ and network access for the one external module (golang.org/x/crypto for SSH backups, fetched through the GitHub mirror named in `go.mod`); everything else is the standard library.

## 7. TLS

Directly: `-tls-cert /etc/ssl/topolight.crt -tls-key /etc/ssl/topolight.key`. Or keep it on `127.0.0.1:8433` and proxy with nginx/Caddy — the app uses cookies with `SameSite=Strict` and needs the `Origin` header to match `Host`, so proxy with `proxy_set_header Host $host` and forward `Origin` unchanged. Server-sent events need `proxy_buffering off`.

## 8. Backup and restore

Everything lives under the data directory: `state.json` (inventory, topology, alerts, users, rules — rewritten atomically every 15 s when something changed), `events/`, `logs/`, `tsdb/`, `profiles/`, `license.key`, `instance.id` (the Instance ID your licence is bound to — keep it with the data). Stop the service or just copy the directory — a copy taken mid-write is still consistent because files are replaced by rename. Restore = put it back and start.

## 9. Upgrade

Re-run the installer (or replace the binary and `systemctl restart topolight`). Data formats are forward-compatible within 0.x; the changelog says when a release needs anything special.

## 10. Uninstall

```sh
sudo systemctl disable --now topolight
sudo rm -f /usr/local/bin/topolight /etc/systemd/system/topolight.service
sudo rm -rf /etc/topolight /var/lib/topolight     # your data — keep it if in doubt
sudo userdel topolight
```
