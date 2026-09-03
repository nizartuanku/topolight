# TopoLight — User guide

## 1. First run: the wizard

Open the console (`http://<host>:8432`). Until an admin exists every request lands on the wizard:

1. **Admin** — instance name, admin user, password (10+ characters). Create this immediately after install; the console is unprotected until it exists.
2. **Site** — a name and the subnets or ranges to discover, one per line (`10.20.0.0/24`, `192.168.10.1-192.168.10.50`; largest range per line is /20).
3. **SNMP** — one credential. SNMPv3 with SHA + AES is recommended; v2c read-only community works too. Read-only is all TopoLight ever needs.
4. **Discovery** — runs immediately; devices appear as they answer, with vendor and model. Devices beyond your licence cap are listed as *not monitored*.
5. **Done** — open the Overview.

Everything the wizard did is editable later under **Admin**.

## 2. Getting devices to talk

Devices need three things: answer SNMP from the TopoLight host, run LLDP (or CDP on Cisco) so topology can be drawn, and send syslog/traps to TopoLight so failures are seen in seconds rather than at the next poll. **Device → Config snippets** generates the lines for Cisco IOS/NX-OS, Aruba, FortiGate, Juniper and MikroTik with your collector's address filled in. Copy, paste, done.

Discovery re-runs on the schedule in **Admin → Settings** (off by default) or on demand from **Admin → Sites → Discover**. Neighbours learned over LLDP/CDP that are not in the inventory are probed with your credentials and added automatically when they answer.

## 3. Overview

Four tiles (estate health, down/unreachable, degraded, open alerts), the per-site status ribbon, recent events and the open root-cause alerts. The top bar is always live: UP / DOWN / DEGRADED / ALERTS counters update over a server-sent event stream; the green dot pulses while the stream is connected. `/` opens the command palette (jump to a device, an alert, a page).

## 4. Topology

Each site is drawn as stacked discs: **edge** (routers, firewalls) on top, then **core**, **distribution**, **access and everything else** at the bottom. Roles are inferred from the graph — the best-connected switches (a redundant pair counts as two) are core, switches hanging off the core with their own downstream switches are distribution, the rest access. Set a role by hand in **Device → Edit** and it stays.

- **3D / 2D** — drag to orbit (3D), shift-drag to pan, scroll to zoom, double-click to reset. 2D lays the same graph flat.
- **Status / Utilisation** overlay — colour by state, or by link utilisation and device CPU.
- **Low-confidence links** — links seen from only one side (confidence 0.8) or to nodes outside the inventory (0.6, drawn dashed) are hidden unless you tick the box. Links seen from both ends are 1.0.
- **Hover a node** for its health card: status and cause, CPU / memory / temperature, RTT and loss, interface counts (up · down · shut · total, uplinks down called out), aggregate traffic in and out as bit/s and packets/s, packet drops (ifInDiscards/ifOutDiscards) and errors per second, the three busiest ports by utilisation (with their own drop/error rate), and the list of ports that are admin-up but operationally down (uplinks first). The card is the last poll's values straight from the store — nothing is estimated — and refreshes live while you hover.
- Click a node for its panel (the same health card, links, alerts, open device). A device that is *unreachable* is drawn dashed: its own state is unknown because an upstream device is down — the cause is named in the panel.
- **Rebuild now** re-walks LLDP/CDP on every device (also runs every 30 minutes by default). Links that disappear are kept greyed for 7 days, then dropped. Manual links (Device → Links) are never dropped.

## 5. Alerts

The alert list shows root causes by default; untick **Root causes only** to see the folded ones. Keyboard: `j`/`k` move, `a` acknowledge, `r` resolve, `Enter` open. The detail panel shows state, site, device, impact (downstream devices), evidence (the polls/traps/log lines that opened and re-opened it), the rule, and the last hour of RTT/loss.

How alerts open and close:

- **Device down** — no ICMP *and* no SNMP for 3 consecutive cycles (Admin → Rules → `device_down`). Critical for core/router/firewall roles, major otherwise. Clears after 2 good cycles.
- **Unreachable (suppressed)** — a device that stopped answering while its upstream (on the LLDP path to the site's core) was already down. Minor, folded under the upstream's alert, never notified separately.
- **SNMP unreachable** — ping works, SNMP does not, for 3 cycles: credentials, ACL or agent problem.
- **Thresholds** — CPU, memory, temperature, ICMP loss/latency, interface utilisation and errors, each with *enter* and *exit* values so a value hovering at the line does not flap. Utilisation escalates to major above 95%.
- **Link down** — from a linkDown trap or a syslog line: the interface is marked down at once, a confirming poll is requested, and the alert clears when the device reports the port up again. Only *important* interfaces (uplinks, LLDP peers, anything you star) raise an alert; other ports just log an event.
- **Flapping** — more than 5 state changes in 10 minutes (the `flapping` rule's cycles value); the device is held in *flapping* until it settles.
- **Site down** — more than 80% of a site's devices down at once collapses into one site alert.
- **Event rules** — reboot, configuration change, BGP/OSPF neighbour, HA state change, environment fault, authentication failures (20 within 5 minutes), log flood, VPN tunnel change: opened from syslog/traps, auto-resolve after the minutes shown in the rule.

An alert that resolves and re-triggers within 30 minutes is re-opened with its history rather than duplicated.

**Maintenance** (Pro/Team): Alert → *Maintenance 2h* or Admin → Maintenance. Devices in a window show as *maintenance*, open nothing, and notify nobody.

## 6. Devices

The table filters by status, site and free text. A device page shows CPU, memory, RTT and loss over the last hour, interfaces with live rates (bit/s and packets/s), utilisation bars, drop and error rates, links and neighbours (LLDP/CDP observations, including unresolved ones — useful when a neighbour is not in the inventory yet), alerts and events, and config snippets. Star an interface (★) to make it *important*: it gets 60-second history and a real alert when it goes down. Uplinks and LLDP peers are starred automatically.

**Add device** by hand when discovery cannot reach it. **Edit** to lock role, domain (network/security), site, poll interval (15–3600 s) or to stop monitoring it. **Delete** removes its history too.

## 7. Logs

Syslog and traps from every device, newest first, with a histogram of the window. Filter by device, minimum severity, time window and source. A line is matched to a device by the sender's IP; lines from unknown senders are kept (with a counter in Admin → System) so you can find devices that talk but were never discovered.

Traps are decoded with the well-known OIDs (link up/down, cold/warm start, authentication failure, BGP, entity) and vendor-agnostic fallbacks; the raw varbinds are kept in the log line.

## 8. Notifications

**Admin → Notifications**: e-mail recipients, Telegram chat id (Pro/Team), webhook URL (Pro/Team), minimum severity, grouping window (alerts that open within N seconds go out as one message), whether to send resolutions, quiet hours, and *critical always* to punch through quiet hours. **Test** sends a sample to every configured channel and reports what happened.

Webhook payload: JSON `{version, sent_at, alerts:[…]}` with header `X-TopoLight-Signature: sha256=<HMAC-SHA256 of the body using -webhook-secret>`. Verify it before trusting the payload.

E-mail uses the SMTP settings from the command line (`-smtp-host`, `-smtp-from`, …); Telegram needs `-telegram-token` from @BotFather and the chat id of the group or user the bot may write to.

## 9. Alert rules

**Admin → Alert rules** lists every rule with enter/exit thresholds, confirmation cycles, severity, *important interfaces only*, and an on/off switch. Changes apply on the next cycle. Defaults are conservative: CPU 85/70, memory 90/80, temperature 70/60 °C, loss 20/5 %, latency 150/100 ms, utilisation 85/70 %, errors 1/0.1 per second.

## 10. Vendor profiles

Built-in profiles match `sysObjectID` (or `sysDescr`) to a vendor, a default role and domain, CPU/memory/temperature OIDs and whether to walk LLDP/CDP. Add your own as JSON files in `<data>/profiles/` — the same fields as the built-ins (Admin → System lists what is loaded, with an example file). A user profile with a higher `priority` wins over a built-in.

## 11. Users and roles

Free: one admin. Pro: three admins. Team: unlimited users with **admin** (everything), **operator** (acknowledge/resolve, discovery, maintenance) and **viewer** (read-only) roles. Passwords are stored as PBKDF2-SHA256; sessions last 7 days and end on logout.

## 12. Licence

**Admin → Licence** shows the tier, caps and what the key allows; paste a key there or start with `-license` / `TOPOLIGHT_LICENSE`. Keys are Ed25519-signed and checked offline — a bad or expired key runs as Free with a plain-language notice, never an error. Dropping from a higher tier keeps the oldest devices monitored up to the cap and marks the rest *not monitored*; nothing is deleted.

## 13. Export

`GET /api/export.json` (Pro/Team) returns sites, devices, interfaces, links and alerts — the same data the console shows — for scripts and inventories. Authenticate with the session cookie; an API-token option is planned.

## 14. Troubleshooting

- **ICMP is off** banner on Overview: the binary lacks `cap_net_raw` and the host does not allow unprivileged ping. `sudo setcap cap_net_raw+ep /usr/local/bin/topolight` or `sysctl -w net.ipv4.ping_group_range="0 2147483647"`.
- **Devices discovered but topology empty**: LLDP is not enabled on the devices (see snippets), or they only run CDP (enabled on Cisco profiles) — wait for the next topology pass or press *Rebuild now*.
- **Syslog arrives from unknown hosts** (Admin → System counter): the device sends from an address that is not its inventory IP (a loopback or management VRF). Add that address as the device IP, or add the device by that address.
- **Console shows "Reconnecting…"**: a proxy is buffering the event stream; set `proxy_buffering off` (nginx) or `flush_interval -1` (Caddy).
- **`402 Payment Required`** from the API: the action needs a higher tier; the message says which.

Logs: `journalctl -u topolight -f`. The Admin → System block (version, licence, collectors, counters, disk) is what to paste into a bug report.
