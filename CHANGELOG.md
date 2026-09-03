# Changelog

## Unreleased

- Topology: hovering a node shows a health card — status/cause, CPU, memory, temperature, RTT/loss, interface counts (up/down/shut, uplinks down), aggregate in/out bit/s and packets/s, packet drops and errors per second, the three busiest ports, and the ports that are admin-up but down. The same card sits in the click-through side panel. New endpoint `GET /api/devices/{id}/health` (viewer), computed from the last samples in the store.
- Polling: IF-MIB unicast packet counters (ifHCIn/OutUcastPkts, 32-bit fallback) and ifIn/OutDiscards are now collected; every interface carries `in_pps`, `out_pps`, `in_drop_rate`, `out_drop_rate` in the API. History: packet rates are stored for important interfaces only, drop series only while drops occur — the documented per-port storage cost is unchanged.

## 0.1.0 — 2026-09-02

First release.

- Discovery: subnet/range sweep (ICMP + SNMP v2c/v3), LLDP/CDP neighbour follow-up, manual add; licence caps reported honestly (over-cap devices listed as *not monitored*).
- Polling: ICMP RTT/loss/jitter; SNMP sysUpTime, IF-MIB with 64-bit counters, HOST-RESOURCES, ENTITY-MIB; vendor profiles for Cisco IOS/NX-OS/ASA, FortiGate, Palo Alto, Juniper, Aruba AOS-S/CX, MikroTik, Huawei, Ubiquiti, net-snmp + user JSON profiles; reboot detection.
- Topology: links from both ends of LLDP/CDP observations with confidence, role inference (core pair / distribution / access), stacked-disc 3D map on a plain canvas with 2D mode, status and utilisation overlays, live updates.
- Intake: syslog UDP/TCP (RFC 3164/5424, Cisco mnemonics, FortiGate key=value) with per-source flood control; SNMP v2c traps and informs with storm limiting.
- State engine: confirmation cycles, hysteresis, flap detection, topology-aware suppression with alert folding, site-down collapse, maintenance windows, dedup and 30-minute re-open, event rules with auto-resolve.
- Notifications: e-mail (SMTP/STARTTLS), Telegram, HMAC-signed webhook; grouping, quiet hours, minimum severity, test button.
- Storage: JSON snapshot + JSONL journals + a compact time-series store (deflated chunks, 60-s raw for 7 days, 5-minute rollups to retention); measured ≈4.5 KB/series/day raw, ≈1.8 KB/series/day rollup.
- Console: setup wizard, overview, topology, alerts with keyboard, devices and device detail, logs with histogram, admin; dark/light; WCAG AA (axe clean); CSP, same-origin checks, PBKDF2 passwords.
- Packaging: static binaries for linux/amd64, linux/arm64, darwin, windows; installer with capabilities and hardened systemd unit; Dockerfile; Apache-2.0.

Known limits are listed in the README under *Honest limits*.
