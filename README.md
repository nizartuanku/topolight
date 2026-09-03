# TopoLight

**Self-hosted network monitoring with a live LLDP topology map — one static binary, no database server, no agents, no telemetry.**

TopoLight watches your switches, routers, firewalls and access points over ICMP and SNMP (v2c/v3), draws the physical topology from what the devices themselves report over LLDP/CDP, listens to their syslog and traps, and tells you *what is down, where, and why* — with the downstream noise folded under the root cause.

![TopoLight demo — 3D topology, a distribution switch fails, the access switch behind it is suppressed, then everything recovers](docs/img/demo.gif)

> Free edition: 25 devices, 1 site, 7-day retention — every feature included, nothing gated.
> Pro (500 devices, 3 sites, 6 months) and Team (1,500 devices, unlimited sites, 12 months, roles) are on
> [Whop](https://whop.com/nizar-tuanku/topolight?utm_source=github) — same binary, a licence key unlocks the caps offline.

## What you get

- **Discovery** — sweep subnets or ranges (ICMP then SNMP with every credential you saved); LLDP/CDP neighbours are followed automatically; manual add for the odd device; ping-only devices for anything without SNMP.
- **Health** — ICMP RTT/loss/jitter; SNMP sysUpTime, IF-MIB 64-bit counters (bit/s, packets/s, utilisation, errors, drops), HOST-RESOURCES, ENTITY-MIB serial/model; vendor CPU/memory/temperature through built-in profiles (Cisco IOS/NX-OS/ASA, FortiGate, Palo Alto, Juniper, Aruba AOS-S/CX, MikroTik, Huawei, Ubiquiti, net-snmp) plus your own JSON profiles.
- **Topology** — links synthesised from both ends of every LLDP/CDP observation with a confidence score; roles (core / distribution / access) inferred from the graph; a **3D stacked-disc map** rendered on a plain canvas (no WebGL, no JavaScript framework) with a 2D mode, orbit, zoom, status and utilisation overlays, live updates.
- **Intake** — syslog UDP/TCP/TLS (RFC 3164 and 5424, Cisco mnemonics, FortiGate key=value) and SNMP v2c + v3 traps/informs; interesting lines become events (link down/up, config change, BGP/OSPF neighbour, HA state, environment, auth failure, log flood).
- **Flow** — NetFlow v5/v9, IPFIX and sFlow v5 collection with top talkers, targets, applications, conversations and exporter interfaces per device or for the whole estate, 5 minutes to 24 hours; bounded top-N summaries, so it never eats the disk.
- **Endpoints** — MAC forwarding tables (Q-BRIDGE/BRIDGE-MIB, Cisco per-VLAN) joined with ARP/ND tables: every MAC with its IP, vendor (embedded IEEE OUI), switch, access port, VLAN and move history — searchable, grouped per port on the device page.
- **Config backup** — running configurations over SSH (Cisco IOS/NX-OS/ASA, FortiGate, PAN-OS, Junos, Aruba, MikroTik, Huawei, EdgeOS, Linux) on a schedule and after "config changed" syslog, versioned with a real line diff.
- **Probes** — synthetic TCP / HTTP(S) / DNS / TLS-expiry / ping / traceroute checks from the TopoLight host, with latency history, alerts on failure or expiring certificates, and path-change events.
- **Reports** — availability/SLA, alerts & MTTR, utilisation, inventory, config changes, flow, endpoints, probes as HTML (print to PDF) or CSV, on demand or mailed daily/weekly/monthly.
- **Routing & L2** — BGP peers and prefixes, OSPF neighbours, VLANs and member ports, spanning-tree root and topology changes, LAG bundles — walked every 5 minutes, with alerts when a peer drops, the root moves or a bundle degrades.
- **Wireless & SD-WAN** — UniFi Network and Cisco Meraki through their APIs; Cisco WLC and Aruba controllers through SNMP: access points with clients per radio and SSID, channels, utilisation, firmware; Meraki MX uplinks and FortiGate SD-WAN health checks with latency, jitter and loss.
- **gNMI / OpenConfig (beta)** — devices with gNMI instead of SNMP (Arista, Nokia, IOS-XR, Juniper, SONiC): system, CPU, memory and interface counters over gRPC from a standard-library client — no gRPC or protobuf runtime in the binary.
- **Cluster** — join 2–5 servers with one command: every full node mirrors all data, polls a share of the devices and can take over as leader (automatic with 3+ nodes, ~20 s); branch collectors poll and forward from behind NAT. Mutual TLS, no external components.
- **State engine** — per-device and per-interface state machine with confirmation cycles, hysteresis, flap detection, **topology-aware suppression** (a device behind a dead uplink is *unreachable*, not *down*, and its alert folds under the root cause), site-down collapse, maintenance windows, dedup and re-open.
- **Notify** — e-mail (SMTP/STARTTLS), Telegram bot, HMAC-signed webhook; grouping, quiet hours, minimum severity.
- **Console** — overview, topology, alerts (keyboard: `j`/`k`/`a`/`r`), devices and device detail with 24-hour graphs, log explorer with histogram, flow and endpoint explorers, admin (sites, credentials, notifications, rules, maintenance, users, licence). Dark and light themes, WCAG AA, command palette on `/`; bearer API tokens for scripts.
- **Storage** — embedded: a JSON snapshot for inventory/topology/alerts, JSONL journals for events and logs (daily, gzip), and a small purpose-built time-series store (deflated chunks; 60-second raw for 7 days, 5-minute rollups to the retention limit). Nothing to install, nothing to tune.
- **Setup** — a five-step wizard gets the first devices on the map in a few minutes; per-vendor config snippets (SNMP, LLDP, syslog, trap) are generated for you.

## Quick start

Linux (amd64/arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/nizartuanku/topolight/main/install.sh | sudo sh
# → console on http://<host>:8433 — open it and follow the wizard
```

The installer downloads the release tarball, verifies it against `SHA256SUMS`, creates a `topolight` system user, grants the binary `cap_net_raw` + `cap_net_bind_service`, writes `/etc/topolight/topolight.env` and a hardened systemd unit, and starts it. Recorded on a clean Ubuntu 24.04 (systemd 255), unedited:

![install.sh on Ubuntu 24.04: download, checksum, user, capabilities, systemd unit, service active, HTTP 200](docs/img/install.gif)

Docker (build the image from this repo — a published `hexward/topolight` image follows once it has been through the same checks):

```sh
docker build -t topolight:0.4.1 .
docker run -d --name topolight \
  -p 8433:8433 -p 514:514/udp -p 514:514 -p 162:162/udp -p 2055:2055/udp -p 6343:6343/udp \
  -v topolight-data:/data \
  topolight:0.4.1
```

Or download the tarball from [Releases](https://github.com/nizartuanku/topolight/releases), verify `SHA256SUMS`, and run `./topolight` — everything is flags, `topolight -h` lists them. See [docs/INSTALL.md](docs/INSTALL.md) for systemd, TLS, ports and unprivileged ICMP.

Build from source (Go 1.24, no dependencies beyond the standard library):

```sh
git clone https://github.com/nizartuanku/topolight && cd topolight
CGO_ENABLED=0 go build -o topolight ./cmd/topolight
```

## How it decides something is down

1. Every poll cycle produces a sample: ICMP answered? SNMP answered? counters, CPU, memory.
2. A device becomes **DOWN** only after `N` consecutive failed cycles (default 3); it becomes **UP** again after 2 good ones. Threshold rules (CPU, memory, temperature, loss, latency, interface utilisation/errors) have separate enter and exit values so a value hovering at the line does not flap.
3. When a device goes down, TopoLight walks the LLDP graph from your core: every device whose only path runs through the failed one is marked **UNREACHABLE (suppressed)** and its alert is folded under the root cause. You get one alert with "3 downstream devices", not four.
4. Traps and syslog are treated as *hard* evidence: a linkDown trap marks the interface down immediately and triggers a confirming poll; if the device says the port is up again, the alert clears itself.
5. Everything is a rule you can edit (thresholds, cycles, severity, important-interfaces-only) — see the Admin → Alert rules page.

## Editions

| | Free (this repo) | Pro | Team |
|---|---|---|---|
| Monitored devices | 25 | 500 | 1,500 |
| Sites | 1 | 3 | unlimited |
| Metric and log retention | 7 days | 6 months | 12 months |
| Users | 3 | 5 | unlimited |
| Every feature — ICMP + SNMP v2c/v3, LLDP/CDP topology 2D + 3D, syslog + traps, NetFlow/IPFIX/sFlow, state engine, e-mail + Telegram + signed webhook, maintenance windows, admin/operator/viewer roles, export API | ✓ | ✓ | ✓ |
| Price | $0 | $49 | $149 |

Tiers differ only in capacity and history. Every feature works in Free, so you can try all of it on a small network before paying for a bigger one.

Caps are enforced honestly: over the limit, discovery still lists the device but marks it *not monitored* and the API answers `402` with a readable message. The licence is an offline Ed25519 key — no phone-home, no account — issued for the **Instance ID** shown under Admin → Licence (one per installation; a cluster shares one). [Get a key on Whop](https://whop.com/nizar-tuanku/topolight?utm_source=github) (14-day trial).

## Honest limits

Read these before you rely on it.

- **Topology comes from LLDP/CDP.** Devices that do not speak either (many ISP CPEs, unmanaged switches, most servers) appear only as *external* nodes or via a manual link. ARP/FDB-based endpoint placement is planned, not shipped.
- **Traps are SNMP v2c only** in this release (v3 traps and syslog over TLS are on the list). Polling supports v3 authPriv (SHA/SHA-256/MD5 + AES-128/DES).
- **Metrics are 60-second resolution** for 7 days (uplinks and device metrics; other ports at 5 minutes from the start), then 5-minute averages/min/max until the retention limit. Flow summaries are bounded top-N buckets (≈2 MB per exporter per day), not raw records.
- **Logs are a searchable journal, not a SIEM.** Filter by device, severity, text and time window; no correlation rules, no long-term analytics.
- **ICMP on Linux needs** either `CAP_NET_RAW` or the unprivileged ping group range (`sysctl net.ipv4.ping_group_range`); the installer sets the capability. On other operating systems TopoLight runs in SNMP-only reachability mode and says so on the Overview.
- **Ports 514 and 162 are privileged.** The installer grants `cap_net_bind_service`; otherwise use `-syslog-listen :5514 -trap-listen :1162` and point devices there.
- **Sizing** — measured, not guessed: a 1,500-device simulated estate polled every 60 s uses about a quarter of one CPU core and ~110 MB of RAM. History costs ≈4.5 KB per series per day at 60-second resolution and ≈1.8 KB per series per day at 5-minute resolution (worst case; idle ports compress far better) — see [docs/INSTALL.md](docs/INSTALL.md#1-sizing) for what that means for your estate. TopoLight runs as **one process per host**; standalone by default, or as a cluster of 2–5 nodes with mirrored data, sharded polling and automatic failover — see [docs/INSTALL.md §1a–1b](docs/INSTALL.md#1a-deployment-topology-what-the-server-is-and-what-it-is-not).
- **Read-only by design.** TopoLight never pushes configuration to a device.

## Configuration

Everything is a flag or an environment variable (`topolight -h`). The ones you will meet first:

```
-listen 127.0.0.1:8433      console address (0.0.0.0:8433 to serve the network; put TLS or a reverse proxy in front)
-data ~/.topolight          state, metrics, logs, licence
-syslog-listen :514         syslog UDP+TCP ("" disables)
-trap-listen :162           SNMP trap UDP ("" disables)
-license SNTL1-...          licence key (or TOPOLIGHT_LICENSE, or <data>/license.key, or paste it in Admin → Licence)
-smtp-host / -smtp-from     e-mail notifications;  -telegram-token, -webhook-secret for the others
-tls-cert / -tls-key        serve HTTPS directly
```

Security notes: the console needs a login from the first request; passwords are PBKDF2-SHA256 (600k); sessions are HttpOnly + SameSite=Strict; state-changing requests are same-origin only; CSP forbids inline and remote scripts; SNMP credentials are stored on disk under the data directory (mode 0600) and never returned by the API.

## Documentation

- [docs/DATASHEET.md](docs/DATASHEET.md) — protocols collected, features, editions, server requirements, deployment modes, comparison, roadmap ([PDF](docs/TopoLight-0.4-datasheet.pdf))
- [docs/INSTALL.md](docs/INSTALL.md) — install, upgrade, systemd, Docker, ports, TLS, backups
- [docs/USER-GUIDE.md](docs/USER-GUIDE.md) — wizard, discovery, topology, alerts, rules, notifications, profiles, licence
- [docs/SPEC-v0.md](docs/SPEC-v0.md) — scope and architecture of v0.1 ([Bahasa Indonesia](docs/SPEC-v0.ID.md))
- [CHANGELOG.md](CHANGELOG.md)

## Contributing

Bug reports with the `Admin → System` block pasted in are the most useful thing you can send. Pull requests are welcome; run `go vet ./... && go test -race ./...` first. The only hard rule: no new dependencies — the binary stays standard-library only.

## Licence

Apache License 2.0 — see [LICENSE](LICENSE).

---

Part of the **Hexward** line of self-hosted tools ·
[Whop](https://whop.com/nizar-tuanku/topolight?utm_source=github) ·
[Instagram](https://instagram.com/nizartuanku) ·
[YouTube](https://youtube.com/@nizartuanku) ·
[TikTok](https://tiktok.com/@nizartuanku)
