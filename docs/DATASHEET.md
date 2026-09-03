# TopoLight 0.4 — Technical datasheet

Self-hosted network monitoring with a live LLDP/CDP topology map. One static binary, embedded storage, offline licence. This sheet lists exactly what TopoLight collects, what it shows, what it needs, and what it does not do yet — so you can compare it with other tools on facts.

*Edition covered: 0.4.x (Free, Pro, Team share one binary). Numbers marked "measured" come from the 1,500-device simulated estate used for release testing; nothing else is estimated.*

## 1. Data sources and protocols

| Source | What TopoLight collects | Notes and limits |
|---|---|---|
| **ICMP echo** | Round-trip time, packet loss, jitter per device, every poll cycle (default 60 s); ping-only devices (no SNMP) for servers, printers and appliances | Raw sockets on Linux (`cap_net_raw` set by the installer, or the unprivileged ping sysctl). macOS/Windows builds run SNMP-only reachability. |
| **SNMP v2c** | Read-only community polling and discovery | Community stored on disk (mode 0600), never returned by the API. |
| **SNMP v3** | authPriv / authNoPriv / noAuthNoPriv; auth SHA-1, SHA-256, MD5; priv AES-128, DES | Own BER codec and USM implementation, tested against net-snmp. |
| **SNMPv2-MIB / HOST-RESOURCES** | sysDescr, sysObjectID, sysUpTime (reboot detection), sysName, sysLocation; hrProcessorLoad and hrStorage as CPU/memory fallback | Reboot = uptime went backwards *and* new uptime < 24 h (no false alarms on counter wrap). |
| **IF-MIB / ifXTable** | Per interface: name, alias, type, speed, MAC, admin/oper status; 64-bit octet counters → bit/s and utilisation; unicast packet counters → packets/s; ifInErrors/ifOutErrors → errors/s; ifInDiscards/ifOutDiscards → drops/s | 32-bit counters used when the agent has no ifXTable, with wrap handling. Interface kinds (physical, LAG, VLAN, tunnel, loopback) classified from ifType. |
| **ENTITY-MIB** | Chassis serial number and model | Used for inventory and for LLDP chassis matching. |
| **Vendor MIBs (built-in profiles)** | CPU, memory, temperature, sessions: Cisco IOS/IOS-XE, NX-OS, ASA/Firepower; Fortinet FortiGate; Palo Alto; Juniper; Aruba AOS-S and AOS-CX; MikroTik; Huawei; Ubiquiti; net-snmp hosts | Profiles are JSON; drop your own in `<data>/profiles/` to add a vendor without touching the poller. |
| **LLDP-MIB** | Local port table, remote chassis ID/port ID/system name/description | Every observation is kept with a confidence score; a link seen from both ends scores 1.0, one end 0.8, an unknown neighbour 0.6. |
| **Cisco CDP (CISCO-CDP-MIB)** | Neighbour device ID, port, platform, address | Enabled by the Cisco profiles; feeds the same link builder as LLDP. |
| **Syslog UDP + TCP (514), TLS (6514)** | RFC 3164 and RFC 5424 messages; RFC 6587/5425 octet-counting and newline framing; TLS with the console or a self-signed certificate, optional client-certificate requirement | Lines are matched to devices by source address; unknown senders are counted so you can find devices that talk but were never discovered. |
| **SNMP traps and informs (v2c + v3, port 162)** | linkDown/linkUp, coldStart/warmStart, authenticationFailure, BGP and ENTITY notifications, plus a vendor-agnostic fallback that keeps raw varbinds; v3 authenticated and decrypted with the saved credentials (keys localised per sender engine ID) | Informs are acknowledged (v3 ones with an authenticated response). |
| **NetFlow v5 / v9, IPFIX (UDP 2055)** | Flow records → per-exporter per-minute and 5-minute summaries: top talkers, targets, conversations, applications (protocol + port), interface in/out | Own decoder; v9/IPFIX templates cached per exporter + observation domain (1 h TTL). Exporters are matched to devices by source IP; unknown exporters still show by address. |
| **sFlow v5 (UDP 6343)** | Flow samples with raw Ethernet headers (802.1Q/QinQ aware), scaled by the sampling rate; expanded samples supported | Counter samples are ignored — SNMP already provides interface counters. |
| **BRIDGE-MIB / Q-BRIDGE-MIB** | MAC forwarding tables, per VLAN; Cisco IOS through community indexing (`community@vlan`) or the `vlan-N` SNMPv3 context | Every 5 minutes; bridge ports mapped to ifIndex; uplinks (LLDP/CDP, topology links, other devices' chassis MACs) excluded from placement. |
| **ipNetToMediaTable / ipNetToPhysicalTable** | ARP and IPv6 neighbour tables from routers, firewalls and L3 switches | Joined with the forwarding tables to give MAC ↔ IP ↔ switch port; vendor from the embedded IEEE OUI registry (53k prefixes, MA-L/M/S). |
| **BGP4-MIB / CISCO-BGP4-MIB / OSPF-MIB** | BGP peers (state, remote AS, uptime, last notification decoded, accepted prefixes), OSPF neighbours and router ID, route count | Every 5 minutes; events for session and adjacency changes and for prefix drops. |
| **Q-BRIDGE (VLANs) / BRIDGE-MIB (STP) / IEEE8023-LAG-MIB** | VLANs with member ports, root bridge, root port and cost, forwarding/blocking ports, topology-change counter, LAG bundles and member state | Root changes and member loss raise alerts. |
| **UniFi Network API / Cisco Meraki Dashboard API** | Access points, switches, gateways with clients per radio and SSID, channel, width, tx power, utilisation, firmware, satisfaction; UniFi WAN1/WAN2 state; Meraki MX uplinks with loss and latency | Read-only user / API key; imported devices are managed by the controller (not polled) and count towards the cap. |
| **AIRESPACE-WIRELESS-MIB / WLSX-WLAN-MIB** | Cisco WLC (AireOS, Catalyst 9800) and Aruba mobility controller AP tables: name, IP, model, serial, status, radios with channel, power, clients, utilisation; clients per SSID | APs become managed devices under the controller; no extra credentials. |
| **FORTINET-FORTIGATE-MIB SD-WAN** | Health-check members: state, latency, jitter, packet loss per interface | `sdwan_link_down` / `sdwan_degraded` alerts; latency/jitter/loss history. |
| **gNMI (OpenConfig) — beta** | `/system/state`, `/system/cpus`, `/system/memory/state`, `/interfaces` over gRPC (TLS or h2c), JSON_IETF: hostname, uptime, version, CPU, memory, interface status, speed, MAC, 64-bit counters | Standard-library HTTP/2 + hand-coded protobuf (no gRPC runtime); Get only, no streaming; tested against grpcio. |
| **SSH (configuration backup)** | Running configuration pulled from Cisco IOS/IOS-XE, NX-OS, ASA, FortiGate, PAN-OS, Junos, Aruba AOS-S/CX, MikroTik, Huawei, EdgeOS and Linux hosts on a schedule and after "config changed" syslog lines; versions with normalised comparison, side-by-side line diff, download; `config_backup_changed` / `config_backup_failed` events | Password or private key, optional enable; read commands only; gzipped history, 50 versions per device. |
| **Synthetic probes** | TCP connect, HTTP(S) status/body, DNS answer, TLS handshake and certificate expiry, ICMP ping, traceroute | Run from the TopoLight host on a schedule; latency series, `probe_failed` / `tls_expiring` alerts, path-change events; tied to devices optionally. |
| **Reports** | Availability/SLA, alerts and MTTR, utilisation and load, inventory, configuration changes, flow, endpoints, probes — HTML (print to PDF) and CSV; daily/weekly/monthly by e-mail | Built from the stored history; scheduled copies kept on disk. |
| **API (bearer tokens)** | Every console endpoint, JSON, with per-token role | Tokens hashed at rest; created and revoked in Admin → Users. |
| **Outbound** | SMTP with STARTTLS; Telegram Bot API; webhook with `X-TopoLight-Signature: sha256=<HMAC>` | Grouping window, quiet hours, minimum severity, "critical always" punch-through. |

## 2. What it does with the data

**Discovery.** Sweeps subnets and ranges (largest /20 per line) with ICMP then SNMP using every saved credential; follows LLDP/CDP neighbours automatically; manual add for anything else. Devices beyond the licence cap are listed as *not monitored*, never silently dropped.

**Topology.** Links are synthesised from both ends of every LLDP/CDP observation. Roles (core, distribution, access, router, firewall) are inferred from the graph — a redundant core pair counts as two — and can be locked by hand. The map is a 3D stacked-disc view drawn on a plain canvas (no WebGL, no JavaScript framework) with a 2D mode, orbit, zoom, status and utilisation overlays, and live updates over a server-sent event stream. Links that disappear are kept greyed for 7 days, then dropped; manual links are never dropped.

**Health at a glance.** Hovering a node shows a health card: status and cause, CPU, memory, temperature, RTT and loss, interface counts (up / down / shut, uplinks down), aggregate traffic in and out as bit/s and packets/s, drops and errors per second, the three busiest ports, and the ports that are admin-up but operationally down. The same card is in the click-through panel and the device page.

**Flow.** Who talks to whom: a Flow page and a per-device Flow tab with a throughput chart and top-N tables for talkers, targets, applications, exporter interfaces (named from the SNMP inventory) and conversations, over 5 minutes to 24 hours. Bounded by design — top 100 / 200 / 50 per bucket, ≈2 MB per exporter per day of history once gzipped (≈26 KB per 5-minute summary while the day is open) — so it never becomes a storage problem. Config snippets include the export commands for each vendor.

**Endpoints.** Where is 10.10.20.15 plugged in? Every MAC seen on the network, with its IPs, vendor, switch, access port, VLAN, first/last seen and port moves — searchable by any of those, grouped per port on the device page, with `endpoint_moved` events. Bounded to 200k MACs / 90 days.

**State engine.** Per-device and per-interface state machines with confirmation cycles (default: down after 3 failed cycles, up after 2 good ones), separate enter/exit thresholds so a value hovering at the line does not flap, flap detection (more than 5 changes in 10 minutes), and **topology-aware suppression**: when a device goes down, everything whose only path runs through it becomes *unreachable* and its alert folds under the root cause. More than 80 % of a site down collapses into one site alert. Maintenance windows silence devices without deleting anything.

**Alert rules (all editable).** device down, SNMP unreachable, CPU, memory, temperature, ICMP loss and latency, interface utilisation and error rate, link down (trap/syslog first, confirmed by a poll), reboot, configuration change, BGP/OSPF neighbour, HA state change, environment fault, authentication failures, log flood, VPN tunnel change. Resolved alerts that re-trigger within 30 minutes re-open with their history instead of duplicating.

**Console.** Overview, topology, alerts with keyboard navigation (`j`/`k`/`a`/`r`), device pages with 24-hour graphs, log explorer with histogram, admin for sites, credentials, notifications, rules, maintenance, users and licence. Dark and light themes, WCAG AA (0 axe violations on 18 page/theme combinations at release), command palette on `/`.

**Security.** Login required from the first request; PBKDF2-SHA256 (600k) passwords; HttpOnly + SameSite=Strict sessions; same-origin enforcement on state-changing requests; CSP forbidding inline and remote scripts; credentials never leave the data directory. TopoLight is read-only towards devices — it never pushes configuration.

## 3. Editions

| | Free | Pro | Team |
|---|---|---|---|
| Monitored devices | 25 | 500 | 1,500 |
| Sites | 1 | 3 | unlimited |
| Metric and log retention | 7 days | 6 months | 12 months |
| Users | 3 | 5 | unlimited |
| Everything in §1–2, plus Telegram and signed webhook, maintenance windows, admin / operator / viewer roles, JSON export API | ✓ | ✓ | ✓ |
| Price | $0 (GitHub) | $99 / month | $249 / month |

One binary for all three and no feature gating — tiers differ only in capacity and history; an offline Ed25519 licence key raises the caps. Keys are issued for the installation's **Instance ID** (Admin → Licence; a cluster shares one) and are refused elsewhere. Over the cap, the newest devices are marked *not monitored* and the API answers `402` with a readable message — nothing breaks, nothing is deleted. A missing, expired or foreign-instance key runs as Free.

## 4. Server requirements

TopoLight is **one process on one host**. The binary contains discovery, the ICMP pinger, the SNMP poller, the syslog and trap receivers, the topology builder, the state and alert engine, the notifier, the embedded time-series and log stores, and the web console. Nothing is installed on the monitored devices; no database server, message bus, cache or agent is installed next to it.

**Measured** on a 1,500-device simulated estate polled every 60 s: about 0.25 CPU cores on average and ~110 MB RSS. Disk for history is the real cost: **≈4.5 KB per series per day at 60-second resolution** and **≈1.8 KB per series per day at 5-minute resolution** (worst case, noisy values). Raw 60-second samples are kept 7 days, then 5-minute avg/min/max to the retention limit; non-uplink ports are stored at 5-minute resolution from the start.

| Estate | Host | History on disk (worst case) |
|---|---|---|
| 100 devices, 24-port switches, 6 months | 1 vCPU · 1 GB RAM | ~2 GB |
| 500 devices, 48-port switches (Pro, 6 months) | 2 vCPU · 2 GB RAM | ~17 GB |
| 1,500 devices, 48-port switches (Team, 12 months) | 2 vCPU · 4 GB RAM | ~100 GB |

Idle access ports compress far better than the worst case; real estates typically land at a third of those figures. Admin → System shows the live number.

**Network.** Outbound UDP/161 to devices; inbound UDP/514 + TCP/514 (syslog), UDP/162 (traps), UDP/2055 (NetFlow/IPFIX) and UDP/6343 (sFlow) from devices; TCP/8433 for the console (put TLS or a reverse proxy in front, or use `-tls-cert/-tls-key`). Linux amd64/arm64 for production; macOS and Windows builds for trying it on a laptop.

### Deployment modes

| Mode | Status | What it means |
|---|---|---|
| **Standalone** | shipped | One host monitors the whole estate over routed IP. |
| **Cluster — standby nodes** | shipped | 2–5 servers joined with one command; every full node holds a complete mirrored copy (10-second refresh); one leader runs the engine, the others poll their share and forward. Three or more full nodes: automatic leader election in ~20 s. Two: manual promote. Any node's console works. |
| **Cluster — collectors** | shipped | Poll-and-forward nodes for branches behind NAT or slow WAN: pin a site to a collector; it polls locally and forwards samples, syslog, traps and flows. |
| **Warm standby (host-level)** | works, manual | Hypervisor HA or a copy of `/var/lib/topolight`; still the simplest answer for a single small site. |

Honest limits: one leader runs the one state engine, so a cluster raises polling throughput and survives a dead server — it does not turn TopoLight into a 50,000-device platform. Failover loses at most ~10 s of logs and ~1 minute of metrics. Nodes need TCP/8434 to each other.

### Sizing per node

| Scenario | Node | Notes |
|---|---|---|
| Standalone ≤ 500 devices | 2 vCPU · 2 GB · 40 GB SSD | measured on 0.1 (¼ core, 110 MB RSS at 1,500 devices) |
| Standalone ≤ 2,000 devices + flow | 4 vCPU · 4 GB · 250 GB SSD | flow summaries ≈2 MB/exporter/day; configs and endpoints are small |
| Cluster 3 full nodes ≤ 2,000 devices | 3 × (2 vCPU · 4 GB · 250 GB) | full copy on each node; automatic failover |
| Branch collector | 1 vCPU · 1 GB · 10 GB | holds only the inventory snapshot |

## 5. How it compares

Stated as differences, not verdicts — each of these tools is good at things TopoLight does not attempt.

| | TopoLight 0.4 | LibreNMS | Zabbix | PRTG |
|---|---|---|---|---|
| Install footprint | 1 static binary, embedded stores, one directory to back up | PHP + MySQL/MariaDB + RRD + poller workers | Server + database (PostgreSQL/MySQL) + frontend + agents/proxies | Windows core server + probes |
| Topology | Drawn from LLDP/CDP with confidence scores; roles inferred; 3D/2D live map | Discovery-based maps and plugins | Manual/network maps, discovery rules | Auto-discovery, sensor tree, maps |
| Root-cause handling | Graph-based suppression built into the state engine (unreachable ≠ down) | Dependency-based parent/child | Trigger dependencies, configured per trigger | Sensor dependencies |
| Traffic data | SNMP/gNMI counters (bps, pps, drops, errors), NetFlow/IPFIX/sFlow top-N | SNMP + NetFlow/sFlow via plugins | SNMP + collectors | SNMP, flow, packet sniffer sensors |
| Pricing model | Per instance: 25 devices free, $99 (500), $249 (1,500); offline key | Free (GPL) | Free (AGPL) + paid support | Per sensor |
| Breadth | Network devices only, on purpose | Very broad | Very broad (servers, apps, cloud) | Very broad |

## 6. Not yet — public roadmap

gNMI streaming subscriptions (Get only today) · BMP · Docker Hub image · more controller APIs (Aruba Central, Mist, Omada). Against a full enterprise NMS/observability catalogue (availability, health, flow, packet, L2 topology, routing, streaming telemetry, logging, identity, network services, wireless/SD-WAN, OAM, cloud) TopoLight covers availability (incl. synthetic checks), topology, device/interface performance, endpoints, flow, routing state, wireless/SD-WAN health and configuration backup; event logging and streaming telemetry partially; and none of the rest — that is the scope of a tool built for 25–2,000 network devices, not a platform claim.

## 7. Links

GitHub (free edition, source, issues): github.com/nizartuanku/topolight · Whop (Pro/Team, 14-day trial): whop.com/nizar-tuanku/topolight · Docs: `docs/INSTALL.md`, `docs/USER-GUIDE.md`, `CHANGELOG.md`. Part of the Hexward line of self-hosted tools.
