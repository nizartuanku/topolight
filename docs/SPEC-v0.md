# TopoLight — Spec v0 (canonical, EN)

*2 Sep 2026 · Hexward line · product id `topolight` · port 8432 · v0.1.0 scope*

> **Historical document.** This is the original build spec (v0.1.0 scope, console port 8432, launch pricing). It is kept for the record and is *not* current. For what TopoLight does and costs today, see [docs/DATASHEET.md](DATASHEET.md) and [CHANGELOG.md](../CHANGELOG.md).


## One sentence

Self-hosted network monitoring for 25–1,500 devices: discovery, SNMP/ICMP health, live LLDP topology map (2D and 3D), syslog and trap intake, state-based alerting with root-cause suppression — one static Go binary built on the standard library alone, no database server, no agent, no telemetry.

## Who it is for

The one- to three-person NOC of an SMB or mid-size company (250–1,500 devices) that needs to know *what is down, where, and why* in three seconds, without running a Zabbix/LibreNMS stack or paying per-element enterprise licences.

## Scope of v0.1.0

| Area | In | Out (v0.2+) |
|---|---|---|
| Discovery | subnet/IP-range sweep (ICMP + SNMP), LLDP-driven neighbour discovery, manual add | controller APIs (Meraki, Catalyst Center, vManage, FMC) |
| Health | ICMP RTT/loss; SNMP v2c/v3 (IF-MIB HC counters, HOST-RESOURCES, ENTITY sysinfo, vendor CPU/temp via profiles) | gNMI/streaming telemetry, IP SLA/TWAMP |
| Topology | LLDP-MIB + CDP-MIB neighbour tables → links with confidence; ifAlias hints; roles core/dist/access; server-side layout; versioned diff | ARP/FDB endpoint placement, BGP/L3 layer, flow overlay |
| Intake | syslog UDP/TCP (RFC 3164/5424) with mnemonic mapping; SNMP v2c traps | syslog TLS, v3 traps, NetFlow/IPFIX/sFlow |
| State & alerts | per-object state machine (UP/DEGRADED/DOWN/FLAPPING/UNKNOWN/MAINTENANCE), hysteresis, confirmation cycles, flap detection, dependency suppression (parent/topology path), site-down collapse, dedup, ack/resolve, maintenance windows | anomaly baselines, service dependency trees |
| Notify | e-mail (SMTP), Telegram bot, signed webhook | ITSM connectors, on-call rotation |
| Storage | embedded: JSON snapshot store for inventory/topology/alerts, JSONL journals for events/logs (daily, gzip), custom TSDB (deflated chunks; 60-s raw 7 days, 5-min rollups to retention; non-uplink interfaces at 5-min from the start) | external TSDB/ClickHouse |
| UI | Overview, Topology 2D/3D (canvas perspective projection, no WebGL, no library), Alerts console, Devices & device detail, Logs, Admin (sites, credentials, rules, users, licence); dark/light; keyboard; live updates via SSE | NOC-wall rotation, report PDF |
| Config backup | — | SSH/NETCONF config backup & diff |

## Tiers

| | Free (GitHub) | Pro $49 | Team $149 |
|---|---|---|---|
| Devices | 25 | 500 | 1,500 |
| Sites | 1 | 3 | unlimited |
| Metric/log retention | 7 days | 6 months | 12 months |
| Users | 1 | 3 | unlimited, roles (admin/operator/viewer) |
| SNMPv3, ICMP, LLDP topology 2D+3D, syslog/trap, state engine, e-mail | yes | yes | yes |
| Telegram + webhook | — | yes | yes |
| Export API (JSON), maintenance windows | — | yes | yes |
| Remote collector (v0.2) | — | — | yes |

Caps answer HTTP 402 with a readable message; nothing is silently truncated — the UI shows "25 of 40 discovered devices are monitored (Free limit)".

## Architecture (single process)

```
discovery ─┐                       ┌─ tsdb (metrics)
poller  ───┼─► samples/events ─► state engine ─► alert engine ─► notify
syslog  ───┤                       └─ store (JSON snapshot + journals)
trap    ───┘                                   └─► web API + SSE ─► UI
```

Everything runs as goroutines in one binary; collectors publish to an in-process bus (buffered channels with bounded fan-out). No external process, no cgo.

## Non-goals (permanent)

No configuration push to devices (read-only by design). No exploitation, no credential brute force. No telemetry home.

## Honest limits (to be printed in README)

Metrics: 60-s resolution for 7 days (uplinks and device metrics), 5-min rollups afterwards; measured ≈4.5 KB per series-day raw and ≈1.8 KB per series-day rollup. Logs are searchable by device/severity/text within a time window; not a SIEM. Topology is drawn from what LLDP/CDP report; devices that do not speak LLDP appear only via manual links or ifAlias hints in v0.1. v2c traps only. ICMP needs CAP_NET_RAW or the unprivileged ping group range on Linux; macOS/Windows builds run SNMP-only reachability. Linux amd64/arm64 are the supported targets.

## Verification gates

`go vet`, `go test -race` green; snmpsim lab with ≥5 simulated devices (v2c + v3 authPriv) exercising discovery → poll → LLDP topology → link-down trap → alert → recovery; screenshots dark + light; axe AA; tier gating (free → 402; forged key → notice, no crash); install from tarball on a clean host < 10 minutes.
