# TopoLight — User guide

## 1. First run: the wizard

Open the console (`http://<host>:8433`). Until an admin exists every request lands on the wizard:

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

**Maintenance**: Alert → *Maintenance 2h* or Admin → Maintenance. Devices in a window show as *maintenance*, open nothing, and notify nobody.

## 6. Devices

The table filters by status, site and free text. A device page shows CPU, memory, RTT and loss over the last hour, interfaces with live rates (bit/s and packets/s), utilisation bars, drop and error rates, links and neighbours (LLDP/CDP observations, including unresolved ones — useful when a neighbour is not in the inventory yet), alerts and events, and config snippets. Star an interface (★) to make it *important*: it gets 60-second history and a real alert when it goes down. Uplinks and LLDP peers are starred automatically.

**Add device** by hand when discovery cannot reach it. **Edit** to lock role, domain (network/security), site, poll interval (15–3600 s) or to stop monitoring it. **Delete** removes its history too.

## 7. Logs

Syslog and traps from every device, newest first, with a histogram of the window. Filter by device, minimum severity, time window and source. A line is matched to a device by the sender's IP; lines from unknown senders are kept (with a counter in Admin → System) so you can find devices that talk but were never discovered.

Traps are decoded with the well-known OIDs (link up/down, cold/warm start, authentication failure, BGP, entity) and vendor-agnostic fallbacks; the raw varbinds are kept in the log line.

**Syslog over TLS** (RFC 5425) listens on TCP/6514 with the console certificate, or — when there is none — a self-signed certificate TopoLight creates in `<data>/syslog-tls.crt` and whose SHA-256 fingerprint it logs at start; pin that on the sender or give it a real certificate with `-syslog-tls-cert/-key`. Add `-syslog-tls-client-ca` to require client certificates. Both octet-counting and newline framing are accepted on TCP and TLS. Admin → System counts failed handshakes and shows the last error.

**SNMPv3 traps and informs** are authenticated with the v3 credentials saved under Admin → Credentials: the user name in the trap selects the credential, the keys are localised to the sender's engine ID, the digest is verified and the payload decrypted when the credential says authPriv. Informs get an authenticated acknowledgement; for informs the device must know TopoLight's engine id (Admin → System, and in the Cisco snippet as `snmp-server engineID remote …`) — engine discovery is answered too, for senders that do it themselves. Rejected v3 traps are counted in Admin → System with the last reason (unknown user, wrong password, missing auth). Traps that arrive with a lower security level than the credential specifies are rejected — configure the device with the same auth/priv as the credential.

## 7b. Flow (NetFlow, IPFIX, sFlow)

Who is talking to whom, and how much. TopoLight receives **NetFlow v5 and v9** and **IPFIX** on UDP/2055 and **sFlow v5** on UDP/6343, matches the exporter to a device by source address, and keeps per-minute summaries for the last hour and 5-minute summaries for 24 hours (persisted daily as JSONL, older days gzipped, pruned with the licence retention).

**Flow page** (`g f`): pick an exporter (or all), a window (5 min … 24 h), and read the throughput chart plus five tables — top talkers (by bytes sent), top targets (by bytes received), applications (protocol + service port, named for the common ones), the exporter's interfaces as they appear in the records (named when the exporter is a monitored device), and conversations (src → dst on the service port). Hosts that are monitored devices show their name. Click a host that is itself an exporter to pivot to its view. *Average rate* is bytes over the seconds that actually hold data, so a fresh exporter is not diluted by an empty window.

**Device → Flow tab** shows the same for one device. **Config snippets** now include the export commands (IOS/IOS-XE and NX-OS Flexible NetFlow, FortiGate, Junos IPFIX, MikroTik Traffic Flow, Aruba sFlow) — apply the monitor to the WAN or uplink interfaces only; that is where the interesting traffic crosses and it keeps the record rate low.

What it keeps per exporter per bucket: top 100 talkers and targets, 200 conversations, 50 applications, every interface — not every flow. That bounds memory and disk (measured with full top-N tables: ≈26 KB per exporter per 5-minute summary while the day is open, ≈7 KB once the day is gzipped — about 2 MB per exporter per day of history) and is what an SMB NOC actually looks at. Full flow retention for forensics is not a goal.

Sampled sFlow is scaled by the sampling rate; NetFlow v9/IPFIX records that arrive before their template are counted in Admin → System as *waiting for templates* (routers resend templates every minute or so). Exporters that are not monitored devices still show by IP.

## 7c. Endpoints (who is plugged in where)

Every 5 minutes TopoLight walks the **MAC forwarding table** of each switch (Q-BRIDGE-MIB, BRIDGE-MIB, and on Cisco IOS the per-VLAN tables through `community@vlan` or the `vlan-N` SNMPv3 context — the IOS v3 snippet includes the `context vlan- match prefix` line that allows it) and the **ARP/ND table** of each router and firewall (ipNetToMediaTable, ipNetToPhysicalTable incl. IPv6). The result is one row per MAC: vendor (IEEE OUI registry, embedded — randomised Wi-Fi MACs show as *locally administered*), IP addresses, the switch and **access port** it lives on, VLAN, first/last seen, and how many times it changed port.

Placement: a MAC is seen on the access port of its switch *and* on the uplinks of every switch above it. Ports with an LLDP/CDP neighbour, a topology link, or another monitored device's chassis MAC are uplinks and never count; among the remaining ports the one with the fewest learned MACs wins, so an undetected trunk still loses to the real edge port. A port change is recorded as an `endpoint_moved` event only when the old port no longer sees the MAC — a refinement from trunk to edge is not a move.

**Endpoints page** (`g e`): type part of a MAC (`aabb.cc`, `aa:bb:cc`), an IP prefix, a vendor or a port name; filter by device. **Device → Endpoints** groups what is plugged into each port, and lists what that device resolved by ARP without hosting it. A router or firewall usually shows the second list only.

Bounds: 200,000 MACs, entries silent for 90 days are dropped; snapshot in `<data>/endpoints.json` (a few hundred bytes per MAC). The forwarding-table walk is the heaviest SNMP operation TopoLight does — a 48-port switch with 500 MACs answers in about a second; a core switch holding 10,000 MACs takes ~10 s once every 5 minutes.

## 7d. Ping-only devices

A server, printer, camera or appliance without SNMP can still be watched: **Devices → + Device → Ping only**, or tick *Keep hosts that answer ping but no SNMP* on a site so discovery adds them (careful on an office LAN — that is every PC, and they count toward the device cap). Ping-only devices get reachability, RTT, loss and jitter, the same down/latency/loss alerts, and no interfaces or inventory. Untick *Ping only* in the device editor once it has a working SNMP credential.

## 7e. Probes (synthetic checks)

**Probes** (`g p`) run from the TopoLight host on a schedule and answer "is the service there, how fast, and for how long": **tcp** connect, **http(s)** with status range and body text checks (and certificate days on https), **dns** with an expected answer and an optional resolver to ask, **tls** handshake with subject/issuer/expiry/chain and an expiry threshold, **ping** (5 echoes, loss and jitter), and **traceroute** (path every 5 minutes, event when it changes; needs CAP_NET_RAW, which the installer grants). Each probe can be tied to a device so its alerts show on that device.

Two consecutive failures open a `probe_failed` alert (Major by default, editable in Admin → Alert rules); the first success resolves it. A TLS certificate inside the threshold (14 days, or the number in *Expect*) opens `tls_expiring` until it is renewed. Latency is kept as a series (`probe_ms|<id>`) with the same retention as device metrics; the probe page shows the 24-hour chart, the last 40 runs with details, and the last path for traceroutes. *Run now* forces a run.

## 7f. Configuration backup and diff

Add an **SSH credential** (Admin → Credentials → type *SSH*: user, password and/or private key, optional enable password, port) and pick it on a site (default for its devices) or on a device (Edit). From then on TopoLight logs in over SSH every 24 hours (Admin → Settings; per device in Edit; -1 disables), runs the vendor's "show running-config" (IOS/IOS-XE with enable, NX-OS, ASA, FortiGate `show`, PAN-OS set format, Junos `display set`, Aruba AOS-S/CX, MikroTik `/export`, Huawei, EdgeOS, Linux hosts), strips pagers and prompts, and stores the text gzipped under `<data>/configs/<device>/`. A "configuration changed" syslog line schedules an extra backup two minutes later, so the version history follows real changes, not just the clock.

Volatile lines (timestamps, "Last configuration change", RouterOS export headers…) are ignored when comparing, so only real changes create a new version; up to 50 versions per device are kept. **Device → Config backup** lists the versions with +/− line counts, shows any version, downloads it, and diffs any two (pick A and B) with line numbers and context. Events: `config_backup_changed` (info, with the counts) and `config_backup_failed` (Minor alert until the next success — wrong credential, unreachable, or a CLI the recipe does not understand; the message quotes what the device answered).

TopoLight only ever runs read commands; a read-only user is enough, except IOS/ASA where `show running-config` needs privilege 15 (use the enable password or a user with `privilege 15`). Host keys are not verified in this release.

## 7g. Reports

**Reports** (`g r`) turn the stored history into something you can hand to a manager or an auditor: availability / SLA per device (downtime from `device_down` alerts, maintenance windows excluded), alerts by rule and by device with mean/median time to resolve, busiest interfaces and device load from the metric history, inventory (vendor, model, OS, serial, uptime), configuration changes and reboot events, flow top talkers and applications (last 24 h), endpoints by vendor and per switch, and probe OK rates. Pick sections, a period (24 h, 7 or 30 days) and a site.

*Quick report* opens the full report for a period in a new tab (HTML — use the browser's Print → Save as PDF) or downloads every table as CSV. Saved reports can run **daily, weekly (Monday) or monthly (1st)** at a chosen hour and be mailed as HTML to a list of recipients (needs the SMTP settings); the last 12 generated copies per report are kept under `<data>/reports/` and listed on the page. `GET /api/reports/preview?period=7d&sections=availability,alerts&format=csv` gives scripts the same data.

## 7h. Routing & L2

Every 5 minutes the poller also walks the protocol tables a device offers: BGP peers (BGP4-MIB, with accepted prefixes on Cisco), OSPF neighbours, VLANs with their member ports, spanning tree (root bridge, root port, blocked ports, topology changes) and link-aggregation bundles. **Device → Routing & L2** shows them; the estate-wide summary is at `GET /api/routing`. Events raise alerts through the rules `bgp_neighbor_down` (Major, resolves when the session is back), `bgp_prefixes_dropped` (a peer now sends less than a quarter of what it sent five minutes ago), `ospf_adjacency_down`, `stp_root_changed` (Major — usually a mis-prioritised new switch), `stp_topology_change` and `lag_member_down` (resolves when every member is back).

## 7i. Wireless and SD-WAN

**Admin → Integrations** connects TopoLight to a controller or cloud API — read-only is enough:

- **UniFi Network**: the controller URL (`https://host:8443`, or the UniFi OS console on 443), a local read-only user, the site name from the URL (`default`). Access points, switches and gateways are imported with clients per radio and per SSID, channel, width, transmit power, channel utilisation, firmware (and whether an update waits), UniFi's satisfaction score, and the gateway's WAN1/WAN2 state.
- **Cisco Meraki**: an API key (Organization → Settings → Dashboard API access) and optionally an organisation id. Devices and statuses across the organisation, wireless clients per AP and SSID, and MX uplinks with the loss and latency Meraki measures.

Imported devices are *managed*: the controller's word is their status (a down AP raises the usual `device_down` alert), they are not polled directly, and they count towards the device limit like any other. **Test** checks the credentials; runs happen every 60 s by default. A failing integration raises `integration_failed` (Minor) until the next good run.

No integration is needed for **Cisco WLC** (AireOS and Catalyst 9800) or **Aruba mobility controllers**: add the controller as an SNMP device and its access points appear as managed devices with radios, clients and SSIDs from the controller MIB. **FortiGate SD-WAN** health checks (state, latency, jitter, loss per member) are read from any monitored FortiGate.

The **Wireless & WAN** page (`g w`) lists every access point (clients, channels with utilisation, firmware, satisfaction) and every WAN path (state, latency, jitter, loss). Device → **Wireless / WAN** has the radios, clients per SSID, the 24-hour client graph and the path latency graph. Rules: `sdwan_link_down` (Major, resolves when the path is healthy again) and `sdwan_degraded` (Minor, at 250 ms or 5 % loss).

## 7j. gNMI / OpenConfig (beta)

For devices that offer gNMI (Arista EOS, Nokia SR Linux, Cisco IOS-XR/NX-OS, Juniper, SONiC), add a credential of type **gNMI** (user, password, port — 6030 on Arista, 57400 on Nokia and IOS-XR — TLS on by default, "accept self-signed" for device certificates) and pick it when you **add the device**. TopoLight then reads OpenConfig state over gRPC instead of SNMP: hostname, uptime and software version (`/system/state`), CPU (`/system/cpus`), memory (`/system/memory/state`) and every interface with its description, status, port speed and 64-bit counters (`/interfaces`). Graphs, rates, interface alerts and reports are identical to the SNMP path. **Test** on the credential shows the target's gNMI version and models. Beta means: routing/L2 tables, endpoints, temperature and vendor health still need SNMP, and only `Get` is used (no streaming subscriptions yet).

## 8. Notifications

**Admin → Notifications**: e-mail recipients, Telegram chat id, webhook URL, minimum severity, grouping window (alerts that open within N seconds go out as one message), whether to send resolutions, quiet hours, and *critical always* to punch through quiet hours. **Test** sends a sample to every configured channel and reports what happened.

Webhook payload: JSON `{version, sent_at, alerts:[…]}` with header `X-TopoLight-Signature: sha256=<HMAC-SHA256 of the body using -webhook-secret>`. Verify it before trusting the payload.

E-mail uses the SMTP settings from the command line (`-smtp-host`, `-smtp-from`, …); Telegram needs `-telegram-token` from @BotFather and the chat id of the group or user the bot may write to.

## 9. Alert rules

**Admin → Alert rules** lists every rule with enter/exit thresholds, confirmation cycles, severity, *important interfaces only*, and an on/off switch. Changes apply on the next cycle. Defaults are conservative: CPU 85/70, memory 90/80, temperature 70/60 °C, loss 20/5 %, latency 150/100 ms, utilisation 85/70 %, errors 1/0.1 per second.

## 10. Vendor profiles

Built-in profiles match `sysObjectID` (or `sysDescr`) to a vendor, a default role and domain, CPU/memory/temperature OIDs and whether to walk LLDP/CDP. Add your own as JSON files in `<data>/profiles/` — the same fields as the built-ins (Admin → System lists what is loaded, with an example file). A user profile with a higher `priority` wins over a built-in.

## 11. Users and roles

Free: three users. Pro: five. Team: unlimited. Every tier has the **admin** (everything), **operator** (acknowledge/resolve, discovery, maintenance) and **viewer** (read-only) roles. Passwords are stored as PBKDF2-SHA256; sessions last 7 days and end on logout.

## 11a. API tokens

Admin → Users → **+ Token** creates a bearer token for scripts and integrations (Grafana, a CMDB sync, a backup job). Send it as `Authorization: Bearer tl_…`; every `/api/…` endpoint works exactly as for the console, with the token's role (viewer, operator or admin — never above the creator's), no cookie and no same-origin check. Only a SHA-256 of the secret is stored, so it is shown once; revoke from the same list. Last use is recorded per minute.

```sh
curl -s -H "Authorization: Bearer tl_…" https://topolight.example.net:8433/api/devices | jq '.[].name'
```

## 11b. Cluster

**Admin → Cluster → Enable clustering** turns this server into the first node of a cluster (it creates the cluster certificate authority and opens TCP/8434 for other nodes; nothing else changes). **+ Join token** gives you a one-line install command for the next server — full node (keeps a complete copy of the data, can become leader) or collector (polls and forwards only, for a branch). Tokens are single-use and expire after 24 hours.

The table shows every node with its role, heartbeat age and round-trip, how far behind its data copy is (standbys stay within about 10 seconds), how many devices it polls, and its version. **Pin sites to nodes** sends a site's devices to one node — a branch collector polls its own branch; everything else is spread by hash. **Remove** shuts a node out immediately.

Failover is automatic with three or more full nodes (a new leader within about 20 seconds; the old leader rejoins as a standby when it comes back). With two full nodes there is no majority, so promote the survivor from its command line with `topolight -promote` and start the service. Open any node's console — standbys proxy to the leader — and point devices' syslog, trap and flow targets at any node or at all of them. Details, ports and the upgrade order are in `docs/INSTALL.md` §1b.

## 12. Licence

**Admin → Licence** shows the tier, caps and what the key allows; paste a key there or start with `-license` / `TOPOLIGHT_LICENSE`. Keys are Ed25519-signed and checked offline — a bad or expired key runs as Free with a plain-language notice, never an error. Dropping from a higher tier keeps the oldest devices monitored up to the cap and marks the rest *not monitored*; nothing is deleted.

**Instance ID.** The same page shows this installation's Instance ID (`TL-XXXX-XXXX-XXXX`, stored in `<data>/instance.id`, created on first start). A licence key is issued for one Instance ID — enter it at checkout and the key you receive works only on that installation. A cluster shares one Instance ID (the file is mirrored to every node), so the key stays valid after a failover. If you rebuild the server, restore the data directory (the ID travels with it) or ask for the key to be re-issued for the new ID; a key bound to another instance is refused with a notice naming both IDs and the licence already in force is left unchanged.

## 13. Export

`GET /api/export.json` returns sites, devices, interfaces, links and alerts — the same data the console shows — for scripts and inventories. Authenticate with the session cookie; an API-token option is planned.

## 14. Troubleshooting

- **ICMP is off** banner on Overview: the binary lacks `cap_net_raw` and the host does not allow unprivileged ping. `sudo setcap cap_net_raw+ep /usr/local/bin/topolight` or `sysctl -w net.ipv4.ping_group_range="0 2147483647"`.
- **Devices discovered but topology empty**: LLDP is not enabled on the devices (see snippets), or they only run CDP (enabled on Cisco profiles) — wait for the next topology pass or press *Rebuild now*.
- **Syslog arrives from unknown hosts** (Admin → System counter): the device sends from an address that is not its inventory IP (a loopback or management VRF). Add that address as the device IP, or add the device by that address.
- **Console shows "Reconnecting…"**: a proxy is buffering the event stream; set `proxy_buffering off` (nginx) or `flush_interval -1` (Caddy).
- **`402 Payment Required`** from the API: the action needs a higher tier; the message says which.

Logs: `journalctl -u topolight -f`. The Admin → System block (version, licence, collectors, counters, disk) is what to paste into a bug report.
