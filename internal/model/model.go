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
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Subnets  []string  `json:"subnets"` // CIDRs swept by discovery
	Lat      float64   `json:"lat,omitempty"`
	Lon      float64   `json:"lon,omitempty"`
	Created  time.Time `json:"created"`
	CredID   string    `json:"cred_id,omitempty"` // default credential for discovery
	Disabled bool      `json:"disabled,omitempty"`
}

// Credential is an SNMP credential set. Secrets are stored in the data dir
// only; the API never returns them.
type Credential struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version"` // "2c" | "3"
	Community string `json:"community,omitempty"`
	User      string `json:"user,omitempty"`
	AuthProto string `json:"auth_proto,omitempty"` // sha | sha256 | md5 | ""
	AuthPass  string `json:"auth_pass,omitempty"`
	PrivProto string `json:"priv_proto,omitempty"` // aes | aes256 | des | ""
	PrivPass  string `json:"priv_pass,omitempty"`
}

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
	return c
}

// Device is a monitored node.
type Device struct {
	ID          string    `json:"id"`
	SiteID      string    `json:"site_id"`
	Name        string    `json:"name"`
	IP          string    `json:"ip"`
	Domain      Domain    `json:"domain"`
	Role        Role      `json:"role"`
	RoleLocked  bool      `json:"role_locked,omitempty"`
	Vendor      string    `json:"vendor,omitempty"`
	Model       string    `json:"model,omitempty"`
	OSVersion   string    `json:"os_version,omitempty"`
	Serial      string    `json:"serial,omitempty"`
	SysObjectID string    `json:"sys_object_id,omitempty"`
	SysDescr    string    `json:"sys_descr,omitempty"`
	Location    string    `json:"location,omitempty"`
	ChassisMAC  string    `json:"chassis_mac,omitempty"`
	ProfileID   string    `json:"profile_id,omitempty"`
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
