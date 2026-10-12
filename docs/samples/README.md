# Sample run

This is real TopoLight output, not a mockup. A lab instance of the free
edition (0.4.4) was pointed at one real device over SNMP and four real
synthetic probes, and the results were exported exactly as the API
returned them — nothing in [`sample-export.json`](sample-export.json) or
[`sample-probes.json`](sample-probes.json) was hand-edited.

**Source:** a lab instance of TopoLight 0.4.4, free edition, with AI Assist
turned off so this sample shows the state engine and probe engine on their
own, not a model's narration (the same choice made in the other Hexward
products' samples).

## What was monitored

| Target | Kind | What it demonstrates |
|---|---|---|
| `192.168.1.111` (DevNet, the lab VM this instance runs on) | SNMP v2c device | real device discovery and polling — sysDescr, sysLocation, sysObjectID identified from a live `net-snmp` agent, not a fixture |
| `https://badssl.com/` | HTTP probe | a synthetic check that passes |
| `badssl.com:443` | TCP probe | a synthetic check that passes |
| `expired.badssl.com:443` | TLS-expiry probe | [badssl.com](https://badssl.com)'s permanently-expired test certificate |
| `hexward-sample-nxdomain.invalid` | DNS probe | a name under the IANA-reserved `.invalid` TLD ([RFC 2606](https://www.rfc-editor.org/rfc/rfc2606)), guaranteed to never resolve |

No customer device or deployment is reproduced here. `192.168.1.111` is
Hexward's own lab VM, monitored with a stock `net-snmp` agent and the
default read-only `public` community most SNMP tutorials use — nothing
proprietary is read or shown.

## What TopoLight found

- **Device `DevNet (Hexward lab VM)` → up**, identified over SNMP alone
  (`"snmp_ok": true`): vendor "Linux (net-snmp)", `sysObjectID`
  `1.3.6.1.4.1.8072.3.2.10`, uptime matching the VM's own `uptime` command
  within a few seconds. `device_discovered` and `device_status` events are
  in `sample-export.json`.
- **`badssl-https` (HTTP) and `badssl-tcp443` (TCP) — passing**, 100%
  uptime in the sample window, real round-trip times.
- **`expired-cert-check` (TLS-expiry) — failing, two alerts fired:**
  a `tls_expiring` alert (minor) the moment the probe saw the certificate
  was outside the warning window, followed by a `probe_failed` alert
  (major) once two consecutive runs failed. The evidence block shows the
  actual leaf certificate: issuer "COMODO RSA Domain Validation Secure
  Server CA", subject `*.badssl.com`, expired 2015-04-12 — real parsed
  fields, not a label.
- **`nxdomain-check` (DNS) — failing, one `probe_failed` alert (major):**
  the real resolver error, `lookup hexward-sample-nxdomain.invalid on
  127.0.0.53:53: no such host`.

Three open alerts in total: 2 major (`probe_failed`), 1 minor
(`tls_expiring`) — see `sample-export.json`'s `alerts` array. `sample-probes.json`
carries each probe's running tally (`fails`, `uptime_pct`, `avg_ms`) and the
last raw result, including the chain-verification error TopoLight computed
for the expired certificate.

## What this sample does not show

This is one VM with one SNMP agent and no LLDP/CDP-speaking neighbours, so
`links`, `interfaces` and the 3D topology map are empty in the export —
honestly, not hidden. ICMP reachability is also off in this sample: the
container/VM user has no `CAP_NET_RAW`, so TopoLight logged the exact
warning a free-edition user would see and fell back to SNMP-only
reachability (documented in the main README's Honest limits). Syslog and
trap intake were disabled for this test run (`-syslog-listen "" -trap-listen
""`) to avoid binding privileged ports on a shared lab host, so no syslog-
or trap-sourced events appear here either. NetFlow/IPFIX/sFlow, config
backup, wireless/SD-WAN, gNMI and clustering are real features covered by
the engine's own tests (see the repo's `_test.go` files); this sample is one
real run against one real device and four real probes, not a tour of every
feature.
