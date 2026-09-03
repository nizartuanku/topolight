// Package model holds the object model shared by every collector, the state
// engine and the UI. No protocol names leak into it: whatever the source, the
// result is a device, an interface, a link, an event or an alert.
package model

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Status is the evaluated condition of an object.
type Status string

const (
	StatusUnknown     Status = "unknown"
	StatusUp          Status = "up"
	StatusDegraded    Status = "degraded"
	StatusDown        Status = "down"
	StatusFlapping    Status = "flapping"
	StatusUnreachable Status = "unreachable" // suppressed: an upstream object is down
	StatusMaintenance Status = "maintenance"
)

// Rank orders statuses worst-first for aggregation.
func (s Status) Rank() int {
	switch s {
	case StatusDown:
		return 6
	case StatusFlapping:
		return 5
	case StatusUnreachable:
		return 4
	case StatusDegraded:
		return 3
	case StatusUnknown:
		return 2
	case StatusMaintenance:
		return 1
	default:
		return 0
	}
}

// Severity of an alert.
type Severity string

const (
	SevCritical Severity = "critical"
	SevMajor    Severity = "major"
	SevMinor    Severity = "minor"
	SevInfo     Severity = "info"
)

// Rank orders severities worst-first.
func (s Severity) Rank() int {
	switch s {
	case SevCritical:
		return 4
	case SevMajor:
		return 3
	case SevMinor:
		return 2
	case SevInfo:
		return 1
	}
	return 0
}

// Domain separates network from security objects (RBAC, retention, colour).
type Domain string

const (
	DomainNetwork  Domain = "network"
	DomainSecurity Domain = "security"
)

// Role of a device in the topology.
type Role string

const (
	RoleCore     Role = "core"
	RoleDist     Role = "distribution"
	RoleAccess   Role = "access"
	RoleRouter   Role = "router"
	RoleFirewall Role = "firewall"
	RoleAP       Role = "ap"
	RoleServer   Role = "server"
	RoleOther    Role = "other"
)

// Site groups devices.
type Site struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Subnets []string  `json:"subnets"` // CIDRs swept by discovery
	Lat     float64   `json:"lat,omitempty"`
	Lon     float64   `json:"lon,omitempty"`
	Created time.Time `json:"created"`
	CredID  string    `json:"cred_id,omitempty"` // default credential for discovery
	// SSHCredID is the default SSH credential for configuration backups.
	SSHCredID string `json:"ssh_cred_id,omitempty"`
	// AddPingOnly makes discovery keep hosts that answer ICMP but no SNMP
	// credential, as ping-only devices (they count toward the device cap).
	AddPingOnly bool `json:"add_ping_only,omitempty"`
	Disabled    bool `json:"disabled,omitempty"`
}

// Credential is an SNMP credential set. Secrets are stored in the data dir
// only; the API never returns them.
type Credential struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"` // "" (snmp) | "ssh" | "gnmi"
	Version   string `json:"version"`        // "2c" | "3"
	Community string `json:"community,omitempty"`
	User      string `json:"user,omitempty"`
	AuthProto string `json:"auth_proto,omitempty"` // sha | sha256 | md5 | ""
	AuthPass  string `json:"auth_pass,omitempty"`
	PrivProto string `json:"priv_proto,omitempty"` // aes | aes256 | des | ""
	PrivPass  string `json:"priv_pass,omitempty"`
	// SSH (Kind == "ssh"): password and/or private key, optional enable password and port.
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	EnablePass string `json:"enable_pass,omitempty"`
	Port       int    `json:"port,omitempty"`
	// gNMI (Kind == "gnmi"): User/Password as gRPC metadata, Port (default 6030),
	// TLS on by default, SkipVerify for self-signed device certificates.
	PlainText  bool `json:"plaintext,omitempty"` // h2c, no TLS
	SkipVerify bool `json:"skip_verify,omitempty"`
}

// IsSSH reports whether the credential is for SSH rather than SNMP.
func (c Credential) IsSSH() bool { return c.Kind == "ssh" }

// IsGNMI reports whether the credential is for gNMI (OpenConfig over gRPC).
func (c Credential) IsGNMI() bool { return c.Kind == "gnmi" }

// IsSNMP reports whether the credential is an SNMP one.
func (c Credential) IsSNMP() bool { return c.Kind == "" }

// Redacted returns a copy safe for the API.
func (c Credential) Redacted() Credential {
	if c.Community != "" {
		c.Community = "••••"
	}
	if c.AuthPass != "" {
		c.AuthPass = "••••"
	}
	if c.PrivPass != "" {
		c.PrivPass = "••••"
	}
	if c.Password != "" {
		c.Password = "••••"
	}
	if c.PrivateKey != "" {
		c.PrivateKey = "••••"
	}
	if c.EnablePass != "" {
		c.EnablePass = "••••"
	}
	return c
}

// Device is a monitored node.
type Device struct {
	ID          string `json:"id"`
	SiteID      string `json:"site_id"`
	Name        string `json:"name"`
	IP          string `json:"ip"`
	Domain      Domain `json:"domain"`
	Role        Role   `json:"role"`
	RoleLocked  bool   `json:"role_locked,omitempty"`
	Vendor      string `json:"vendor,omitempty"`
	Model       string `json:"model,omitempty"`
	OSVersion   string `json:"os_version,omitempty"`
	Serial      string `json:"serial,omitempty"`
	SysObjectID string `json:"sys_object_id,omitempty"`
	SysDescr    string `json:"sys_descr,omitempty"`
	Location    string `json:"location,omitempty"`
	ChassisMAC  string `json:"chassis_mac,omitempty"`
	ProfileID   string `json:"profile_id,omitempty"`
	// SSHCredID selects the SSH credential for configuration backups
	// (empty: the site's default). BackupEvery is hours between backups
	// (0: the global default; -1: never for this device).
	SSHCredID   string `json:"ssh_cred_id,omitempty"`
	BackupEvery int    `json:"backup_every,omitempty"`
	// Managed marks a device that an integration owns ("unifi:<id>", "meraki:<id>",
	// "wlc:<controller device id>"): its status comes from the controller, not from SNMP.
	Managed string `json:"managed,omitempty"`
	// PingOnly devices are watched with ICMP alone: no SNMP, no interfaces,
	// no inventory — reachability, RTT, loss and jitter only.
	PingOnly    bool      `json:"ping_only,omitempty"`
	CredID      string    `json:"cred_id,omitempty"`
	PollEvery   int       `json:"poll_every"` // seconds
	Monitored   bool      `json:"monitored"`  // false when over tier cap or disabled
	Status      Status    `json:"status"`
	StatusSince time.Time `json:"status_since"`
	LastPoll    time.Time `json:"last_poll,omitempty"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
	Uptime      int64     `json:"uptime_s,omitempty"`
	Created     time.Time `json:"created"`
	Notes       string    `json:"notes,omitempty"`
	// Cause is the id of the upstream object responsible when Status is unreachable.
	Cause string `json:"cause,omitempty"`
	// SNMPOK tells whether the last SNMP poll succeeded (ICMP may still be fine).
	SNMPOK bool `json:"snmp_ok"`
	// Metrics is the last sample set for quick display (cpu, mem, temp...).
	Metrics map[string]float64 `json:"metrics,omitempty"`
}

// Interface is a port of a device.
type Interface struct {
	ID          string    `json:"id"`
	DeviceID    string    `json:"device_id"`
	Index       int       `json:"ifindex"`
	Name        string    `json:"name"`
	Alias       string    `json:"alias,omitempty"`
	SpeedMbps   int64     `json:"speed_mbps"`
	MAC         string    `json:"mac,omitempty"`
	Kind        string    `json:"kind"` // phys|lag|vlan|tunnel|loopback|other
	Important   bool      `json:"important"`
	AdminUp     bool      `json:"admin_up"`
	OperUp      bool      `json:"oper_up"`
	Status      Status    `json:"status"`
	StatusSince time.Time `json:"status_since"`
	InBps       float64   `json:"in_bps"`
	OutBps      float64   `json:"out_bps"`
	InUtil      float64   `json:"in_util_pct"`
	OutUtil     float64   `json:"out_util_pct"`
	InErrRate   float64   `json:"in_err_rate"` // errors per second
	OutErrRate  float64   `json:"out_err_rate"`
	InPps       float64   `json:"in_pps"` // unicast packets per second
	OutPps      float64   `json:"out_pps"`
	InDropRate  float64   `json:"in_drop_rate"` // ifInDiscards per second
	OutDropRate float64   `json:"out_drop_rate"`
	LastChange  time.Time `json:"last_change,omitempty"`
}

// NeighborObs is one observation of an adjacency from one side.
type NeighborObs struct {
	DeviceID   string    `json:"device_id"`
	LocalIf    string    `json:"local_if"` // interface name on DeviceID
	Source     string    `json:"source"`   // lldp|cdp|alias|manual
	RemoteName string    `json:"remote_name,omitempty"`
	RemoteMAC  string    `json:"remote_mac,omitempty"`
	RemoteIP   string    `json:"remote_ip,omitempty"`
	RemotePort string    `json:"remote_port,omitempty"`
	RemoteDesc string    `json:"remote_desc,omitempty"`
	Confidence float64   `json:"confidence"`
	SeenAt     time.Time `json:"seen_at"`
}

// Link joins two interfaces.
type Link struct {
	ID         string    `json:"id"`
	ADevice    string    `json:"a_device"`
	AIf        string    `json:"a_if"` // interface name
	BDevice    string    `json:"b_device"`
	BIf        string    `json:"b_if"`
	Layer      string    `json:"layer"` // L2
	Confidence float64   `json:"confidence"`
	Sources    []string  `json:"sources"`
	Status     Status    `json:"status"`
	SpeedMbps  int64     `json:"speed_mbps"`
	Util       float64   `json:"util_pct"` // max of both directions
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Stale      bool      `json:"stale,omitempty"`
	Manual     bool      `json:"manual,omitempty"`
	// External marks a link to a node outside the inventory (unknown neighbour).
	External     bool   `json:"external,omitempty"`
	ExternalName string `json:"external_name,omitempty"`
}

// Clone returns a deep copy (slices included).
func (l Link) Clone() Link {
	l.Sources = append([]string(nil), l.Sources...)
	return l
}

// Event is a normalised, deduplicated occurrence.
type Event struct {
	ID       string            `json:"id"`
	TS       time.Time         `json:"ts"`
	Domain   Domain            `json:"domain"`
	Kind     string            `json:"kind"` // link_down, device_rebooted, config_changed, ...
	DeviceID string            `json:"device_id,omitempty"`
	Object   string            `json:"object,omitempty"` // interface id or other object id
	Source   string            `json:"source"`           // trap|syslog|snmp|icmp|discovery|user
	Severity Severity          `json:"severity"`
	Message  string            `json:"message"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	DedupKey string            `json:"dedup_key,omitempty"`
}

// Clone deep-copies the event.
func (e Event) Clone() Event {
	if e.Attrs != nil {
		m := make(map[string]string, len(e.Attrs))
		for k, v := range e.Attrs {
			m[k] = v
		}
		e.Attrs = m
	}
	return e
}

// AlertState is the lifecycle state of an alert.
type AlertState string

const (
	AlertOpen     AlertState = "open"
	AlertAcked    AlertState = "acked"
	AlertResolved AlertState = "resolved"
)

// Alert is raised on a state transition and lives until resolved.
type Alert struct {
	ID          string     `json:"id"`
	Rule        string     `json:"rule"`
	Severity    Severity   `json:"severity"`
	State       AlertState `json:"state"`
	Domain      Domain     `json:"domain"`
	SiteID      string     `json:"site_id"`
	DeviceID    string     `json:"device_id"`
	Object      string     `json:"object,omitempty"`
	Title       string     `json:"title"`
	Detail      string     `json:"detail,omitempty"`
	OpenedAt    time.Time  `json:"opened_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ResolvedAt  time.Time  `json:"resolved_at,omitempty"`
	AckedBy     string     `json:"acked_by,omitempty"`
	AckNote     string     `json:"ack_note,omitempty"`
	Occurrences int        `json:"occurrences"`
	RootCause   string     `json:"root_cause,omitempty"` // alert id of the parent alert
	Children    int        `json:"children"`
	Evidence    []string   `json:"evidence"` // "trap 07:31:06", "syslog 07:31:06"
	DedupKey    string     `json:"dedup_key"`
	Notified    bool       `json:"notified"`
	Impact      string     `json:"impact,omitempty"`
}

// Clone deep-copies the alert.
func (a Alert) Clone() Alert {
	a.Evidence = append([]string(nil), a.Evidence...)
	return a
}

// Maintenance silences objects for a window.
type Maintenance struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	SiteID   string    `json:"site_id,omitempty"`
	Devices  []string  `json:"devices,omitempty"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Reason   string    `json:"reason,omitempty"`
	Creator  string    `json:"creator,omitempty"`
	Created  time.Time `json:"created"`
	Disabled bool      `json:"disabled,omitempty"`
}

// Clone deep-copies.
func (m Maintenance) Clone() Maintenance {
	m.Devices = append([]string(nil), m.Devices...)
	return m
}

// Active reports whether the window covers device d at time t.
func (m Maintenance) Active(t time.Time, siteID, deviceID string) bool {
	if m.Disabled || t.Before(m.From) || t.After(m.To) {
		return false
	}
	if len(m.Devices) > 0 {
		for _, d := range m.Devices {
			if d == deviceID {
				return true
			}
		}
		return false
	}
	return m.SiteID == "" || m.SiteID == siteID
}

// User of the console.
type User struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Role     string    `json:"role"` // admin|operator|viewer
	Hash     string    `json:"hash"` // pbkdf2
	Salt     string    `json:"salt"`
	Created  time.Time `json:"created"`
	Disabled bool      `json:"disabled,omitempty"`
}

// Probe is a synthetic check run from the TopoLight host: is the service
// there, how fast, and (TLS) for how long is the certificate still good.
type Probe struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Type     string    `json:"type"`   // tcp|http|dns|tls|ping|traceroute
	Target   string    `json:"target"` // host:port | URL | name | host
	Every    int       `json:"every"`  // seconds
	Timeout  int       `json:"timeout"`
	Expect   string    `json:"expect,omitempty"`   // http: "200-299" or "body:text"; dns: expected address; tls: min days
	Resolver string    `json:"resolver,omitempty"` // dns: server host[:port]
	DeviceID string    `json:"device_id,omitempty"`
	SiteID   string    `json:"site_id,omitempty"`
	Enabled  bool      `json:"enabled"`
	Created  time.Time `json:"created"`
}

// ProbeResult is one run.
type ProbeResult struct {
	ProbeID string            `json:"probe_id"`
	TS      time.Time         `json:"ts"`
	OK      bool              `json:"ok"`
	Ms      float64           `json:"ms"`
	Detail  string            `json:"detail,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Report is a saved report definition, optionally scheduled by e-mail.
type Report struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Sections []string  `json:"sections"` // availability|alerts|utilisation|inventory|changes|flow|endpoints|probes
	Period   string    `json:"period"`   // 24h|7d|30d
	SiteID   string    `json:"site_id,omitempty"`
	Schedule string    `json:"schedule,omitempty"` // ""|daily|weekly|monthly
	Hour     int       `json:"hour"`               // local hour for the schedule
	EmailTo  []string  `json:"email_to,omitempty"`
	Enabled  bool      `json:"enabled"`
	Created  time.Time `json:"created"`
	LastRun  time.Time `json:"last_run,omitempty"`
	LastErr  string    `json:"last_error,omitempty"`
}

// Integration is a controller or cloud API TopoLight reads (wireless, SD-WAN).
type Integration struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"` // unifi|meraki
	Name     string    `json:"name"`
	URL      string    `json:"url,omitempty"`      // unifi: https://controller:8443 ; meraki: default https://api.meraki.com
	Site     string    `json:"site,omitempty"`     // unifi site name (default "default"); meraki organisation id ("" = all)
	Username string    `json:"username,omitempty"` // unifi local user (read-only is enough)
	Password string    `json:"password,omitempty"`
	APIKey   string    `json:"api_key,omitempty"` // meraki
	Insecure bool      `json:"insecure,omitempty"`
	SiteID   string    `json:"site_id,omitempty"` // TopoLight site the imported devices belong to
	Every    int       `json:"every"`             // seconds (default 60)
	Enabled  bool      `json:"enabled"`
	Created  time.Time `json:"created"`
	LastRun  time.Time `json:"last_run,omitempty"`
	LastErr  string    `json:"last_error,omitempty"`
	Devices  int       `json:"devices,omitempty"`
	Clients  int       `json:"clients,omitempty"`
}

// Redacted hides secrets.
func (i Integration) Redacted() Integration {
	if i.Password != "" {
		i.Password = "••••"
	}
	if i.APIKey != "" {
		i.APIKey = "••••"
	}
	return i
}

// Wireless is the per-access-point (or per-controller) wireless state.
type Wireless struct {
	DeviceID   string         `json:"device_id"`
	TS         time.Time      `json:"ts"`
	Clients    int            `json:"clients"`
	Radios     []Radio        `json:"radios,omitempty"`
	SSIDs      map[string]int `json:"ssids,omitempty"` // clients per SSID
	Controller string         `json:"controller,omitempty"`
	Model      string         `json:"model,omitempty"`
	Version    string         `json:"version,omitempty"`
	Upgradable bool           `json:"upgradable,omitempty"`
	Satisf     int            `json:"satisfaction,omitempty"` // 0–100 (UniFi)
	APs        int            `json:"aps,omitempty"`          // controllers: managed APs
	APsUp      int            `json:"aps_up,omitempty"`
}

// Radio is one radio of an access point.
type Radio struct {
	Name    string  `json:"name"` // ng, na, 6e
	Channel int     `json:"channel"`
	Width   int     `json:"width,omitempty"`
	TxPower int     `json:"tx_power,omitempty"` // dBm
	TxLevel int     `json:"tx_level,omitempty"` // Cisco power level 1 (max) … 8 when dBm is not reported
	Clients int     `json:"clients"`
	Util    float64 `json:"util_pct,omitempty"` // channel utilisation
}

// SDWANChanges compares two path lists and returns the paths that went down
// (including paths seen down for the first time) and the ones that recovered.
func SDWANChanges(prev, cur []SDWANLink) (down, up []SDWANLink) {
	was := map[string]bool{}
	had := map[string]bool{}
	for _, l := range prev {
		was[l.Name+"|"+l.Interface], had[l.Name+"|"+l.Interface] = l.Up, true
	}
	for _, l := range cur {
		k := l.Name + "|" + l.Interface
		switch {
		case !l.Up && (!had[k] || was[k]):
			down = append(down, l)
		case l.Up && had[k] && !was[k]:
			up = append(up, l)
		}
	}
	return
}

// SDWANLink is one WAN path with its health.
type SDWANLink struct {
	DeviceID  string    `json:"device_id"`
	Name      string    `json:"name"` // health-check / uplink name
	Interface string    `json:"interface"`
	Up        bool      `json:"up"`
	State     string    `json:"state"`
	LatencyMs float64   `json:"latency_ms"`
	JitterMs  float64   `json:"jitter_ms"`
	LossPct   float64   `json:"loss_pct"`
	IP        string    `json:"ip,omitempty"`
	TS        time.Time `json:"ts"`
}

// Routing and layer-2 state of a device, refreshed every 5 minutes.
type Routing struct {
	DeviceID string    `json:"device_id"`
	TS       time.Time `json:"ts"`
	Routes   int       `json:"routes,omitempty"` // ipCidrRouteNumber / inetCidrRouteNumber
	BGP      []BGPPeer `json:"bgp,omitempty"`
	OSPF     []OSPFNbr `json:"ospf,omitempty"`
	VLANs    []VLAN    `json:"vlans,omitempty"`
	STP      *STP      `json:"stp,omitempty"`
	LAGs     []LAG     `json:"lags,omitempty"`
	LocalAS  int64     `json:"local_as,omitempty"`
	RouterID string    `json:"router_id,omitempty"`
}

// BGPPeer is one row of bgpPeerTable (+ Cisco prefix counts when available).
type BGPPeer struct {
	Peer      string `json:"peer"`
	RemoteAS  int64  `json:"remote_as"`
	State     string `json:"state"` // idle|connect|active|opensent|openconfirm|established
	Up        bool   `json:"up"`
	UptimeS   int64  `json:"uptime_s"`
	Prefixes  int64  `json:"prefixes,omitempty"` // accepted prefixes (Cisco/Juniper/Arista extensions)
	LastError string `json:"last_error,omitempty"`
}

// OSPFNbr is one row of ospfNbrTable.
type OSPFNbr struct {
	Neighbor string `json:"neighbor"` // ip
	RouterID string `json:"router_id"`
	State    string `json:"state"` // down|attempt|init|twoWay|exchangeStart|exchange|loading|full
	Full     bool   `json:"full"`
	Priority int    `json:"priority"`
}

// VLAN is one row of dot1qVlanStaticTable.
type VLAN struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"`
	Ports []string `json:"ports,omitempty"` // interface names (egress members), capped
	NPort int      `json:"nport"`
}

// STP is the bridge's spanning-tree summary (dot1dStp).
type STP struct {
	Protocol     string   `json:"protocol,omitempty"`
	BridgeID     string   `json:"bridge_id,omitempty"`
	RootID       string   `json:"root_id,omitempty"`
	IsRoot       bool     `json:"is_root"`
	RootPort     string   `json:"root_port,omitempty"`
	RootCost     int64    `json:"root_cost"`
	TopChanges   int64    `json:"top_changes"`
	LastChangeS  int64    `json:"last_change_s"` // seconds since the last topology change
	Forwarding   int      `json:"forwarding"`
	Blocking     int      `json:"blocking"`
	BlockedPorts []string `json:"blocked_ports,omitempty"`
}

// LAG is one aggregate with its members (IEEE8023-LAG-MIB).
type LAG struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
	Up      int      `json:"up"` // members that are attached and oper-up
}

// APIToken is a bearer token for scripts (Authorization: Bearer tl_…). Only
// the SHA-256 of the secret is stored; the secret is shown once at creation.
type APIToken struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Role     string    `json:"role"` // viewer|operator|admin — never above the creator's role
	Hash     string    `json:"hash"` // hex sha256 of the secret
	Prefix   string    `json:"prefix"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitempty"`
	Creator  string    `json:"creator,omitempty"`
}

// Rule is an alert rule. Defaults ship embedded; admins override per object.
type Rule struct {
	ID          string   `json:"id"`
	Object      string   `json:"object"` // device|interface|link|site
	Metric      string   `json:"metric,omitempty"`
	Enter       float64  `json:"enter,omitempty"` // threshold to enter degraded
	Exit        float64  `json:"exit,omitempty"`  // threshold to leave
	ForCycles   int      `json:"for_cycles"`      // consecutive cycles required
	Severity    Severity `json:"severity"`
	Escalate    float64  `json:"escalate,omitempty"` // value that raises severity one level
	OnlyImport  bool     `json:"only_important,omitempty"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description"`
	Runbook     string   `json:"runbook,omitempty"`
}

// Notification channels configuration.
type Notify struct {
	EmailTo        []string `json:"email_to,omitempty"`
	TelegramChat   string   `json:"telegram_chat,omitempty"`
	WebhookURL     string   `json:"webhook_url,omitempty"`
	MinSeverity    Severity `json:"min_severity"`
	GroupSeconds   int      `json:"group_seconds"`
	ResolvedToo    bool     `json:"resolved_too"`
	QuietFrom      string   `json:"quiet_from,omitempty"` // "22:00"
	QuietTo        string   `json:"quiet_to,omitempty"`
	CriticalAlways bool     `json:"critical_always"`
}

// Clone deep-copies.
func (n Notify) Clone() Notify {
	n.EmailTo = append([]string(nil), n.EmailTo...)
	return n
}

// Settings is the small set of instance-wide options editable in the UI.
type Settings struct {
	InstanceName   string `json:"instance_name"`
	ConsoleURL     string `json:"console_url,omitempty"`
	DefaultPoll    int    `json:"default_poll"`    // seconds
	DiscoveryEvery int    `json:"discovery_every"` // minutes; 0 disables periodic sweeps
	TopologyEvery  int    `json:"topology_every"`  // minutes
	SetupDone      bool   `json:"setup_done"`
	Timezone       string `json:"timezone,omitempty"`
	// SNMPv3 engine identity of this receiver (informs): hex engine id and
	// the boots counter, incremented at every start.
	EngineID    string `json:"engine_id,omitempty"`
	EngineBoots int32  `json:"engine_boots,omitempty"`
	// BackupEveryHours is how often configurations are pulled over SSH (default 24; 0 disables).
	BackupEveryHours int `json:"backup_every_hours"`
}

var idCounter uint64

// NewID returns a sortable, collision-free id: prefix + unix-ms + counter.
func NewID(prefix string) string {
	n := atomic.AddUint64(&idCounter, 1)
	return fmt.Sprintf("%s_%x%03x", prefix, time.Now().UnixMilli(), n%4096)
}

// IfID composes the interface id for a device/ifindex pair.
func IfID(deviceID string, ifindex int) string { return fmt.Sprintf("%s:%d", deviceID, ifindex) }

// DeviceSample is what a poll cycle learned about a device.
type DeviceSample struct {
	DeviceID  string
	TS        time.Time
	Reachable bool // ICMP answered
	SNMPOK    bool
	RTTms     float64
	LossPct   float64
	Jitterms  float64
	Uptime    int64 // seconds; -1 when unknown
	Rebooted  bool
	CPU       float64 // -1 when unknown
	MemPct    float64
	TempC     float64
	Sessions  float64
	Err       string
}

// InterfaceSample is one interface's condition after a poll cycle.
type InterfaceSample struct {
	IfID        string
	DeviceID    string
	Name        string
	TS          time.Time
	OperUp      bool
	AdminUp     bool
	Important   bool
	SpeedMbps   int64
	InBps       float64
	OutBps      float64
	InUtil      float64
	OutUtil     float64
	InErrRate   float64
	OutErrRate  float64
	InPps       float64
	OutPps      float64
	InDropRate  float64
	OutDropRate float64
	HaveRates   bool
}

// LogEntry is one syslog line or trap, normalised for storage and search.
type LogEntry struct {
	TS       time.Time `json:"ts"`   // device timestamp when parseable, else Recv
	Recv     time.Time `json:"recv"` // arrival time
	DeviceID string    `json:"device_id,omitempty"`
	Host     string    `json:"host"` // source IP
	Facility int       `json:"facility"`
	Severity int       `json:"severity"` // syslog 0..7
	Source   string    `json:"source"`   // syslog|trap
	Mnemonic string    `json:"mnemonic,omitempty"`
	Message  string    `json:"message"`
}

// SeverityName maps syslog severity numbers to names.
func SeverityName(s int) string {
	names := []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}
	if s >= 0 && s < len(names) {
		return names[s]
	}
	return "unknown"
}
