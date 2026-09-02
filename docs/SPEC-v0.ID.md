# TopoLight — Spec v0 (Bahasa Indonesia; versi kanonis: SPEC-v0.md)

*2 Sep 2026 · lini Hexward · product id `topolight` · port 8432 · lingkup v0.1.0*

## Satu kalimat

Pemantauan jaringan self-hosted untuk 25–1.500 perangkat: discovery, kesehatan SNMP/ICMP, peta topologi LLDP hidup (2D dan 3D), penerimaan syslog dan trap, alerting berbasis state dengan penekanan root-cause — satu biner Go statis yang hanya memakai pustaka standar, tanpa server database, tanpa agen, tanpa telemetri.

## Untuk siapa

NOC satu sampai tiga orang di perusahaan kecil–menengah (250–1.500 perangkat) yang perlu tahu *apa yang mati, di mana, dan kenapa* dalam tiga detik, tanpa mengelola tumpukan Zabbix/LibreNMS atau membayar lisensi per-elemen kelas enterprise.

## Lingkup v0.1.0

| Area | Masuk | Belum (v0.2+) |
|---|---|---|
| Discovery | sapuan subnet/rentang IP (ICMP + SNMP), tindak lanjut tetangga LLDP/CDP, tambah manual | API controller (Meraki, Catalyst Center, vManage, FMC) |
| Kesehatan | ICMP RTT/loss; SNMP v2c/v3 (counter HC IF-MIB, HOST-RESOURCES, ENTITY sysinfo, CPU/temperatur vendor via profil) | gNMI/telemetri streaming, IP SLA/TWAMP |
| Topologi | tabel tetangga LLDP-MIB + CDP-MIB → link dengan skor kepercayaan; petunjuk ifAlias; peran core/dist/access; layout sisi server; diff berversi | penempatan endpoint via ARP/FDB, lapisan L3/BGP, overlay flow |
| Penerimaan | syslog UDP/TCP (RFC 3164/5424) dengan pemetaan mnemonic; trap SNMP v2c | syslog TLS, trap v3, NetFlow/IPFIX/sFlow |
| State & alert | mesin state per objek (UP/DEGRADED/DOWN/FLAPPING/UNKNOWN/MAINTENANCE), histeresis, siklus konfirmasi, deteksi flap, penekanan dependensi (jalur topologi), kolaps site-down, dedup, ack/resolve, jendela maintenance | baseline anomali, pohon dependensi layanan |
| Notifikasi | e-mail (SMTP), bot Telegram, webhook bertanda tangan | konektor ITSM, rotasi on-call |
| Penyimpanan | tertanam: snapshot JSON untuk inventaris/topologi/alert, jurnal JSONL untuk event/log (harian, gzip), TSDB khusus (chunk ter-deflate; raw 60 detik 7 hari, rollup 5 menit sampai retensi; antarmuka non-uplink 5 menit sejak awal) | TSDB eksternal/ClickHouse |
| UI | Overview, Topologi 2D/3D (proyeksi perspektif kanvas, tanpa WebGL, tanpa pustaka), konsol Alert, Perangkat & detail, Log, Admin (site, kredensial, aturan, pengguna, lisensi); gelap/terang; keyboard; pembaruan langsung via SSE | rotasi NOC-wall, laporan PDF |
| Backup konfigurasi | — | backup & diff konfigurasi via SSH/NETCONF |

## Tier

| | Free (GitHub) | Pro $49 | Team $149 |
|---|---|---|---|
| Perangkat | 25 | 500 | 1.500 |
| Site | 1 | 3 | tak terbatas |
| Retensi metrik/log | 7 hari | 6 bulan | 12 bulan |
| Pengguna | 1 | 3 | tak terbatas, peran (admin/operator/viewer) |
| SNMPv3, ICMP, topologi LLDP 2D+3D, syslog/trap, mesin state, e-mail | ya | ya | ya |
| Telegram + webhook | — | ya | ya |
| API ekspor (JSON), jendela maintenance | — | ya | ya |
| Remote collector (v0.2) | — | — | ya |

Batas dijawab HTTP 402 dengan pesan yang bisa dibaca; tidak ada pemotongan diam-diam — UI menampilkan "25 dari 40 perangkat yang ditemukan dipantau (batas Free)".

## Arsitektur (satu proses)

```
discovery ─┐                       ┌─ tsdb (metrik)
poller  ───┼─► sampel/event ─► mesin state ─► mesin alert ─► notifikasi
syslog  ───┤                       └─ store (snapshot JSON + jurnal)
trap    ───┘                                   └─► API web + SSE ─► UI
```

Semuanya berjalan sebagai goroutine dalam satu biner; kolektor menerbitkan ke bus dalam-proses (channel ber-buffer dengan fan-out terbatas). Tanpa proses eksternal, tanpa cgo.

## Bukan tujuan (permanen)

Tidak ada push konfigurasi ke perangkat (read-only by design). Tidak ada eksploitasi, tidak ada brute force kredensial. Tidak ada telemetri pulang.

## Batasan jujur (dicetak di README)

Metrik: resolusi 60 detik selama 7 hari (uplink dan metrik perangkat), rollup 5 menit sesudahnya; terukur ≈4,5 KB per seri-hari raw dan ≈1,8 KB per seri-hari rollup. Log bisa dicari per perangkat/severity/teks dalam jendela waktu; bukan SIEM. Topologi digambar dari apa yang dilaporkan LLDP/CDP; perangkat yang tidak berbicara LLDP hanya muncul lewat link manual atau petunjuk ifAlias di v0.1. Trap v2c saja. ICMP butuh CAP_NET_RAW atau rentang ping group tanpa hak istimewa di Linux; build macOS/Windows berjalan dalam mode jangkauan SNMP-saja. Target yang didukung: Linux amd64/arm64.

## Gerbang verifikasi

`go vet`, `go test -race` hijau; lab snmpsim dengan ≥5 perangkat simulasi (v2c + v3 authPriv) menjalankan discovery → poll → topologi LLDP → trap link-down → alert → pemulihan; screenshot gelap + terang; axe AA; gating tier (free → 402; kunci palsu → notice, tanpa crash); pemasangan dari tarball di host bersih < 10 menit.

## Status verifikasi 2 Sep 2026

Semua gerbang terpenuhi: lab 6 perangkat (2 core NX-OS, dist IOS, access Aruba CX, FortiGate, MikroTik) dengan LLDP → 9 link kepercayaan 1,0, peran benar; uji beban 1.500 perangkat (poll 60 detik) ≈0,25 core CPU, ≈110 MB RSS; deadlock mesin state saat perangkat pertama kali mati ditemukan oleh lab dan diperbaiki (tes regresi `TestDownstreamSuppression`); axe AA 0 pelanggaran di 18 kombinasi halaman/tema.
