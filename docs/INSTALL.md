# TopoLight — Install

TopoLight is one static binary. It needs a Linux box (amd64 or arm64) that can reach your network devices over UDP/161, receive their syslog (514) and traps (162), and serve its console on TCP/8432. macOS and Windows builds exist for trying it out on a laptop; they run in SNMP-only reachability mode (no ICMP) — see *Honest limits* in the README.

## 1. Sizing

Measured on a 1,500-device simulated estate polled every 60 s (Team licence): about 0.25 CPU cores on average and ~110 MB RSS. CPU and memory are not the constraint; disk for history is, and it scales with the number of *stored series* — roughly 5 per device (RTT, loss, CPU, memory, temperature) plus 2 per interface (in/out bps; error series only while errors occur).

Measured storage cost (worst case, noisy values): **≈4.5 KB per series per day of 60-second history** and **≈1.8 KB per series per day of 5-minute history**. TopoLight keeps 60-second samples for 7 days (`-raw-days`) and 5-minute avg/min/max after that; interfaces that are not uplinks/LLDP peers/starred are stored at 5-minute resolution from the start.

| Estate | Host | History on disk (worst case) |
|---|---|---|
| 100 devices, 24-port switches, 6 months | 1 vCPU · 1 GB RAM | ~2 GB |
| 500 devices, 48-port switches (Pro, 6 months) | 2 vCPU · 2 GB RAM | ~17 GB |
| 1,500 devices, 48-port switches (Team, 12 months) | 2 vCPU · 4 GB RAM | ~100 GB |

Idle access ports compress far better than the worst case, so real estates typically land at a third of those figures; the Overview header and Admin → System show the live number so you can watch the trend after a week and size the disk from that. Devices you do not care about can be set *not monitored* to keep them out of the budget.

## 1a. Deployment topology: what the server is, and what it is not

**One process, one host.** The `topolight` binary contains everything: discovery, the ICMP pinger, the SNMP poller, the syslog and trap receivers, the topology builder, the state/alert engine, the notifier, the embedded time-series and log stores, and the web console with its live event stream. There is no database server, message bus, cache or agent to install next to it, and no component runs on the monitored devices.

| Mode | Status in 0.1 | What it means |
|---|---|---|
| **Standalone** | shipped | One VM/host (table above) monitors the whole estate over routed IP. This is the only supported mode today. |
| **Warm standby (host-level)** | works, manual | Run TopoLight on a VM and use your hypervisor's HA or a scheduled copy of `/var/lib/topolight` (see §8 Backups) to a second host. The data directory is self-contained, so restoring it on another host with the same licence key brings the console back with its history. Point devices' syslog/trap targets at a VIP or DNS name you can move. |
| **Active/active HA** | not yet | Two instances polling the same devices would double the SNMP load and raise duplicate alerts; there is no shared state or leader election in 0.1. Planned for 0.2 together with remote collectors. |
| **Cluster / remote collectors** | not yet | 0.2 work: a lightweight collector per site (SNMP/ICMP/syslog locally, results forwarded to the central engine) for estates with many sites, NAT'd branches or >1,500 devices. |

A 1,500-device estate fits comfortably on one host (¼ core, 110 MB RAM measured), so for the Free/Pro/Team tiers the limiting factor is disk for history, not compute or resilience. If losing the console for the minutes it takes to restore a VM is unacceptable for you today, wait for 0.2 or run the warm-standby pattern above.

## 2. Install with the script (systemd hosts)

```sh
curl -fsSL https://raw.githubusercontent.com/nizartuanku/topolight/main/install.sh | sudo sh
```

The script downloads the release tarball for your CPU, verifies it against `SHA256SUMS`, installs `/usr/local/bin/topolight`, creates a `topolight` system user, grants the binary `cap_net_raw` (ICMP) and `cap_net_bind_service` (ports 514/162), writes `/etc/topolight/topolight.env` and a hardened systemd unit, and starts the service. Re-running it upgrades the binary and keeps `/var/lib/topolight`.

Offline host: download the tarball on another machine and run `TOPOLIGHT_TARBALL=/path/topolight_0.1.0_linux_amd64.tar.gz sudo sh install.sh`.

Then open `http://<host>:8432` and follow the wizard. The first request creates the admin user — do this straight away.

## 3. Install by hand

```sh
tar -xzf topolight_0.1.0_linux_amd64.tar.gz && cd topolight_0.1.0_linux_amd64
sha256sum -c ../SHA256SUMS --ignore-missing
sudo install -m 0755 topolight /usr/local/bin/topolight
sudo setcap 'cap_net_raw,cap_net_bind_service=+ep' /usr/local/bin/topolight   # ICMP + ports < 1024 without root
topolight -listen 0.0.0.0:8432 -data /var/lib/topolight
```

Without `setcap`, either run as root (not recommended), allow unprivileged ICMP (`sysctl -w net.ipv4.ping_group_range="0 2147483647"`) and use high ports (`-syslog-listen :5514 -trap-listen :1162`), pointing devices at those.

`deploy/topolight.service` is the same unit the installer writes; `/etc/topolight/topolight.env` holds the flags in `TOPOLIGHT_OPTS` and optional `TOPOLIGHT_LICENSE`.

## 4. Docker

```sh
docker build -t topolight:0.1.0 .
docker run -d --name topolight --restart unless-stopped \
  -p 8432:8432 -p 514:514/udp -p 514:514 -p 162:162/udp \
  -v topolight-data:/data topolight:0.1.0
```

or `docker compose up -d` with the included `docker-compose.yml`. The container runs as an unprivileged user with the two capabilities set on the binary. If your devices send syslog to the *host* IP, the port mappings above are all you need; with `--network host` the container sees devices' real source addresses, which is what TopoLight uses to match syslog/traps to devices — behind a NAT'd bridge they arrive from the Docker gateway and cannot be matched. **Use `--network host` when you can.**

## 5. Flags and environment

```
-listen 127.0.0.1:8432   console; 0.0.0.0:8432 to serve the network
-data ~/.topolight       state, metrics, logs, licence (0700)
-memory                  keep everything in RAM (demos)
-license KEY             or TOPOLIGHT_LICENSE, or <data>/license.key, or Admin → Licence
-syslog-listen :514      syslog UDP + TCP; "" disables
-trap-listen :162        SNMP trap UDP; "" disables
-trap-community NAME     require this community on incoming v2c traps
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
| you → TopoLight | TCP/8432 | console (put TLS in front) |
| TopoLight → SMTP / api.telegram.org / your webhook | as configured | notifications |

Nothing else. TopoLight never contacts the internet on its own.

## 7. TLS

Directly: `-tls-cert /etc/ssl/topolight.crt -tls-key /etc/ssl/topolight.key`. Or keep it on `127.0.0.1:8432` and proxy with nginx/Caddy — the app uses cookies with `SameSite=Strict` and needs the `Origin` header to match `Host`, so proxy with `proxy_set_header Host $host` and forward `Origin` unchanged. Server-sent events need `proxy_buffering off`.

## 8. Backup and restore

Everything lives under the data directory: `state.json` (inventory, topology, alerts, users, rules — rewritten atomically every 15 s when something changed), `events/`, `logs/`, `tsdb/`, `profiles/`, `license.key`. Stop the service or just copy the directory — a copy taken mid-write is still consistent because files are replaced by rename. Restore = put it back and start.

## 9. Upgrade

Re-run the installer (or replace the binary and `systemctl restart topolight`). Data formats are forward-compatible within 0.x; the changelog says when a release needs anything special.

## 10. Uninstall

```sh
sudo systemctl disable --now topolight
sudo rm -f /usr/local/bin/topolight /etc/systemd/system/topolight.service
sudo rm -rf /etc/topolight /var/lib/topolight     # your data — keep it if in doubt
sudo userdel topolight
```
