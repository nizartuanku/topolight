# TopoLight — Concepts

What this product is, what problem it solves, and why it works the way it does — written for
someone meeting the problem for the first time. The command reference is in the README; this
is the reasoning behind it.

*Hexward Labs · Nizar Tuanku — Cybersecurity. · last reviewed 6 September 2026*

---

## The night the alerts were all correct and all useless
A distribution switch loses power. Behind it sit three access switches, forty access points and two hundred desks.
Your monitoring does its job: it notices that four devices stopped answering, and it sends four alerts. Then the access points go quiet, and it sends forty more. Every one of those alerts is technically correct. Not one of them tells you what happened.
The on-call engineer opens forty-four alerts and starts guessing which one is the cause and which forty-three are the consequences. That guessing is what monitoring was supposed to remove.
## Why most monitoring cannot tell cause from consequence
Most tools watch devices as a list: is device 17 up, is device 18 up. A list has no idea that device 18 can only be reached through device 17. So when 17 dies, the list truthfully reports that 18 died too.
To tell cause from consequence you need something the list does not have: the shape of the network — what is plugged into what. That shape is called the topology: not the logical addresses, but the physical wiring.
Drawing that map by hand is the job nobody finishes, because the network changes faster than the diagram.
## The network already knows its own shape
Here is the detail that makes TopoLight possible. Switches and routers announce themselves to their neighbours over a small protocol called LLDP (or Cisco's older CDP): "I am switch-core-1, and on my port 24 you will find switch-access-3."
Every device already holds a list of its neighbours. TopoLight reads those lists — from both ends of every link — and assembles the whole map without anyone drawing anything. When a link is confirmed from both sides it scores 1.0; from one side 0.8; that score is shown, so you know how sure the map is.
## What changes once the tool knows the shape
Now the same power failure looks different.
TopoLight sees the distribution switch stop answering. Before it sends anything, it walks the map from your core outward and asks: which devices can only be reached through the one that just died? Those devices are marked unreachable — not down — and their alerts are folded under the cause.
You get one alert: distribution-2 is down, 43 devices behind it are unreachable.
The difference between "down" and "unreachable" is the whole point. The access points are probably fine. Fix the one switch, and they come back on their own — and TopoLight clears their alerts on its own too.
## The second thing it refuses to do: cry wolf
A device that answers, then misses one ping, then answers again is not down. Monitoring that alerts on a single miss trains people to ignore it.
TopoLight declares a device down only after three consecutive failed cycles, and up again after two good ones. Thresholds — CPU, temperature, link utilisation — have separate enter and exit values, so a value hovering at the line does not flip on and off every minute. That behaviour is called flap detection, and it is the difference between a monitor people trust and a monitor people mute.
## Everything else it collects, briefly
Once the map exists, a great deal follows from it:
- Traffic — how much is flowing through every port, and where the busiest conversations go (NetFlow, IPFIX, sFlow)
- Endpoints — which laptop is plugged into which port on which switch, and when it moved
- Configuration backups — every device's running config, saved on a schedule and every time the device reports a change, with a line-by-line diff of what changed
- Logs and traps — the messages the devices send when something happens, tied to the device on the map
- Wireless — access points, clients per radio, channels, from UniFi, Meraki, Cisco and Aruba controllers
- Routing — BGP peers, OSPF neighbours, spanning-tree root, and alerts when any of them change
All of that runs from one program on one server, with nothing installed on the devices themselves.
## What TopoLight is not
Not a SIEM. Its log page is a searchable journal, not a correlation engine. If you want to know that a port scan and a brute force came from the same address, that is Loglight's job.
Not a configuration manager. It reads configs and backs them up; it never pushes configuration to a device. That is a deliberate boundary, not a missing feature.
Not a server or application monitor. It watches network devices only, on purpose. A tool that watches everything is good at nothing in particular.
## One limit you should know before judging it
The topology comes from LLDP and CDP. A device that speaks neither — many ISP-provided routers, unmanaged switches — cannot tell TopoLight who its neighbours are. It will still be monitored, but it appears at the edge of the map rather than wired into it. That is the honest boundary of the approach, and it is stated in the product rather than papered over.
## The part that is free
The free edition monitors 25 devices with every feature — the map, the suppression, flows, backups, all of it. Nothing is gated. What Pro and Team add is capacity (500 and 1,500 devices) and history (6 and 12 months).
We did it this way so you can try the whole product on a small network before deciding whether it deserves a big one.
## Try it
The quickest route on a Linux host:
```
curl -fsSL https://raw.githubusercontent.com/nizartuanku/topolight/main/install.sh | sudo sh
```
Or from the release tarball, verifying it first. TopoLight ships five platform builds and one SHA256SUMS covering all of them, so check the line for the file you actually downloaded:
```
curl -LO https://github.com/nizartuanku/topolight/releases/latest/download/topolight_0.4.1_linux_amd64.tar.gz
curl -LO https://github.com/nizartuanku/topolight/releases/latest/download/SHA256SUMS
grep topolight_0.4.1_linux_amd64.tar.gz SHA256SUMS | sha256sum -c -
tar xzf topolight_0.4.1_linux_amd64.tar.gz
cd topolight_0.4.1_linux_amd64
./topolight
```
Open port 8433 and follow the five-step wizard. The first devices are on the map in a few minutes. The free Apache-2.0 edition is on GitHub; Pro and Team are on Whop.
Nizar Tuanku — Cybersecurity. · github.com/nizartuanku/topolight

## Terms used above

- Topology — the map of which device connects to which, through which port. Not the logical addresses, but the physical wiring.
- LLDP / CDP — a protocol where every device tells its direct neighbours who it is and which port it is speaking through. Almost every managed switch, router, firewall and access point speaks one of the two.
