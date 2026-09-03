# TopoLight 0.1 — Technical datasheet

Self-hosted network monitoring with a live LLDP/CDP topology map. One static binary, embedded storage, offline licence. This sheet lists exactly what TopoLight collects, what it shows, what it needs, and what it does not do yet — so you can compare it with other tools on facts.

*Edition covered: 0.1.x (Free, Pro, Team share one binary). Numbers marked "measured" come from the 1,500-device simulated estate used for release testing; nothing else is estimated.*

## 1. Data sources and protocols

| Source | What TopoLight collects | Notes and limits |
|---|---|---|
| **ICMP echo** | Round-trip time, packet loss, jitter per device, every poll cycle (default 60 s) | Raw sockets on Linux (`cap_net_raw` set by the installer, or the unprivileged ping sysctl). macOS/Windows builds run SNMP-only reachability. |
| **SNMP v2c** | Read-only community polling and discovery | Community stored on disk (mode 0600), never returned by the API. |
| **SNMP v3** | authPriv / authNoPriv / noAuthNoPriv; auth SHA-1, SHA-256, MD5; priv AES-128, DES | Own BER codec and USM implementation, tested against net-snmp. |
| **SNMPv2-MIB / HOST-RESOURCES** | sysDescr, sysObjectID, sysUpTime (reboot detection), sysName, sysLocation; hrProcessorLoad and hrStorage as CPU/memory fallback | Reboot = uptime went backwards *and* new uptime < 24 h (no false alarms on counter wrap). |
| **IF-MIB / ifXTable** | Per interface: name, alias, type, speed, MAC, admin/oper status; 64-bit octet counters → bit/s and utilisation; unicast packet counters → packets/s; ifInErrors/ifOutErrors → errors/s; ifInDiscards/ifOutDiscards → drops/s | 32-bit counters used when the agent has no ifXTable, with wrap handling. Interface kinds (physical, LAG, VLAN, tunnel, loopback) classified from ifType. |
| **ENTITY-MIB** | Chassis serial number and model | Used for inventory and for LLDP chassis matching. |
| **Vendor MIBs (built-in profiles)** | CPU, memory, temperature, sessions: Cisco IOS/IOS-XE, NX-OS, ASA/Firepower; Fortinet FortiGate; Palo Alto; Juniper; Aruba AOS-S and AOS-CX; MikroTik; Huawei; Ubiquiti; net-snmp hosts | Profiles are JSON; drop your own in `<data>/profiles/` to add a vendor without touching the poller. |
| **LLDP-MIB** | Local port table, remote chassis ID/port ID/system name/description | Every observation is kept with a confidence score; a link seen from both ends scores 1.0, one end 0.8, an unknown neighbour 0.6. |
| **Cisco CDP (CISCO-CDP-MIB)** | Neighbour device ID, port, platform, address | Enabled by the Cisco profiles; feeds the same link builder as LLDP. |
| **Syslog UDP + TCP (port 514)** | RFC 3164 and RFC 5424 framing; Cisco `%FACILITY-SEV-MNEMONIC`; FortiGate `key=value` | Lines are matched to devices by source address; unknown senders are counted so you can find devices that talk but were never discovered. TLS syslog is not in 0.1. |
| **SNMP traps and informs (v2c, port 162)** | linkDown/linkUp, coldStart/warmStart, authenticationFailure, BGP and ENTITY notifications, plus a vendor-agnostic fallback that keeps raw varbinds | v3 traps are not in 0.1. |
| **Outbound** | SMTP with STARTTLS; Telegram Bot API; webhook with `X-TopoLight-Signature: sha256=<HMAC>` | Grouping window, quiet hours, minimum severity, "critical always" punch-through. |

## 2. What it does with the data

**Discovery.** Sweeps subnets and ranges (largest /20 per line) with ICMP then SNMP using every saved credential; follows LLDP/CDP neighbours automatically; manual add for anything else. Devices beyond the licence cap are listed as *not monitored*, never silently dropped.

**Topology.** Links are synthesised from both ends of every LLDP/CDP observation. Roles (core, distribution, access, router, firewall) are inferred from the graph — a redundant core pair counts as two — and can be locked by hand. The map is a 3D stacked-disc view drawn on a plain canvas (no WebGL, no JavaScript framework) with a 2D mode, orbit, zoom, status and utilisation overlays, and live updates over a server-sent event stream. Links that disappear are kept greyed for 7 days, then dropped; manual links are never dropped.

**Health at a glance.** Hovering a node shows a health card: status and cause, CPU, memory, temperature, RTT and loss, interface counts (up / down / shut, uplinks down), aggregate traffic in and out as bit/s and packets/s, drops and errors per second, the three busiest ports, and the ports that are admin-up but operationally down. The same card is in the click-through panel and the device page.

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
| Users | 1 admin | 3 admins | unlimited; admin / operator / viewer roles |
| Everything in §1–2 (ICMP, SNMP v2c/v3, LLDP/CDP topology 2D + 3D, syslog + traps, state engine, e-mail) | ✓ | ✓ | ✓ |
| Telegram and signed webhook | — | ✓ | ✓ |
| Maintenance windows, JSON export API | — | ✓ | ✓ |
| Price | $0 (GitHub) | $49 / month | $149 / month |

One binary for all three; an offline Ed25519 licence key unlocks the caps. Over the cap, the newest devices are marked *not monitored* and the API answers `402` with a readable message — nothing breaks, nothing is deleted. A missing or expired key runs as Free.

## 4. Server requirements

TopoLight is **one process on one host**. The binary contains discovery, the ICMP pinger, the SNMP poller, the syslog and trap receivers, the topology builder, the state and alert engine, the notifier, the embedded time-series and log stores, and the web console. Nothing is installed on the monitored devices; no database server, message bus, cache or agent is installed next to it.

**Measured** on a 1,500-device simulated estate polled every 60 s: about 0.25 CPU cores on average and ~110 MB RSS. Disk for history is the real cost: **≈4.5 KB per series per day at 60-second resolution** and **≈1.8 KB per series per day at 5-minute resolution** (worst case, noisy values). Raw 60-second samples are kept 7 days, then 5-minute avg/min/max to the retention limit; non-uplink ports are stored at 5-minute resolution from the start.

| Estate | Host | History on disk (worst case) |
|---|---|---|
| 100 devices, 24-port switches, 6 months | 1 vCPU · 1 GB RAM | ~2 GB |
| 500 devices, 48-port switches (Pro, 6 months) | 2 vCPU · 2 GB RAM | ~17 GB |
| 1,500 devices, 48-port switches (Team, 12 months) | 2 vCPU · 4 GB RAM | ~100 GB |

Idle access ports compress far better than the worst case; real estates typically land at a third of those figures. Admin → System shows the live number.

**Network.** Outbound UDP/161 to devices; inbound UDP/514 + TCP/514 (syslog) and UDP/162 (traps) from devices; TCP/8432 for the console (put TLS or a reverse proxy in front, or use `-tls-cert/-tls-key`). Linux amd64/arm64 for production; macOS and Windows builds for trying it on a laptop.

### Deployment modes

| Mode | Status in 0.1 | What it means |
|---|---|---|
| **Standalone** | shipped | One host monitors the whole estate over routed IP. The only supported mode today. |
| **Warm standby** | works, manual | Hypervisor HA or a scheduled copy of `/var/lib/topolight` to a second host; the data directory is self-contained, so a restore with the same key brings history back. Point syslog/trap targets at a movable VIP or DNS name. |
| **Active/active HA** | not yet (0.2) | Two instances today would double SNMP load and duplicate alerts; no shared state or leader election in 0.1. |
| **Remote collectors / cluster** | not yet (0.2) | A lightweight collector per site for many-site, NAT'd or >1,500-device estates. |

## 5. How it compares

Stated as differences, not verdicts — each of these tools is good at things TopoLight does not attempt.

| | TopoLight 0.1 | LibreNMS | Zabbix | PRTG |
|---|---|---|---|---|
| Install footprint | 1 static binary, embedded stores, one directory to back up | PHP + MySQL/MariaDB + RRD + poller workers | Server + database (PostgreSQL/MySQL) + frontend + agents/proxies | Windows core server + probes |
| Topology | Drawn from LLDP/CDP with confidence scores; roles inferred; 3D/2D live map | Discovery-based maps and plugins | Manual/network maps, discovery rules | Auto-discovery, sensor tree, maps |
| Root-cause handling | Graph-based suppression built into the state engine (unreachable ≠ down) | Dependency-based parent/child | Trigger dependencies, configured per trigger | Sensor dependencies |
| Traffic data | SNMP counters (bps, pps, drops, errors) | SNMP + NetFlow/sFlow via plugins | SNMP + collectors | SNMP, flow, packet sniffer sensors |
| Pricing model | Per instance: 25 devices free, $49 (500), $149 (1,500); offline key | Free (GPL) | Free (AGPL) + paid support | Per sensor |
| Breadth | Network devices only, on purpose | Very broad | Very broad (servers, apps, cloud) | Very broad |

## 6. Not in 0.1 — public roadmap

NetFlow/IPFIX/sFlow collection · gNMI/OpenConfig streaming · SNMPv3 traps · syslog over TLS · MAC/ARP endpoint placement · TCP/HTTP/DNS/TLS synthetic probes · BGP/BMP peering · configuration backup · active/active HA and remote collectors · API tokens · Docker Hub image. Against a full enterprise NMS/observability catalogue (availability, health, flow, packet, L2 topology, routing, streaming telemetry, logging, identity, network services, wireless/SD-WAN, OAM, cloud) TopoLight 0.1 covers availability, topology and device/interface performance well, event logging partially, and none of the rest — that is the scope of a tool built for 25–1,500 network devices, not a platform claim.

## 7. Links

GitHub (free edition, source, issues): github.com/nizartuanku/topolight · Whop (Pro/Team, 14-day trial): whop.com/nizar-tuanku/topolight · Docs: `docs/INSTALL.md`, `docs/USER-GUIDE.md`, `CHANGELOG.md`. Part of the Hexward line of self-hosted tools.
