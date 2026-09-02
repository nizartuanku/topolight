// Package profile maps a device (sysObjectID / sysDescr) to the vendor OIDs
// worth polling beyond the standard MIBs. Profiles are declarative data so
// adding a vendor never means changing the poller. Users can add their own in
// <data>/profiles/*.json with the same shape.
package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nizartuanku/topolight/internal/model"
)

// Metric describes how to fetch one value.
type Metric struct {
	// OID is polled with GET when Walk is false, otherwise the subtree is
	// walked and the values combined with Agg ("avg" | "max" | "sum" | "first").
	OID   string  `json:"oid"`
	Walk  bool    `json:"walk,omitempty"`
	Agg   string  `json:"agg,omitempty"`
	Scale float64 `json:"scale,omitempty"` // multiply (e.g. 0.1)
}

// Profile is one vendor/platform definition.
type Profile struct {
	ID         string       `json:"id"`
	Vendor     string       `json:"vendor"`
	Match      []string     `json:"match"`                 // sysObjectID prefixes
	DescrMatch string       `json:"descr_match,omitempty"` // regexp on sysDescr (either match suffices)
	Role       model.Role   `json:"role,omitempty"`
	Domain     model.Domain `json:"domain,omitempty"`
	CPU        *Metric      `json:"cpu,omitempty"`      // percent
	MemUsed    *Metric      `json:"mem_used,omitempty"` // bytes or percent (see MemIsPct)
	MemFree    *Metric      `json:"mem_free,omitempty"`
	MemIsPct   bool         `json:"mem_is_pct,omitempty"`
	Temp       *Metric      `json:"temp,omitempty"` // celsius
	Sessions   *Metric      `json:"sessions,omitempty"`
	LLDP       bool         `json:"lldp"`
	CDP        bool         `json:"cdp"`
	Priority   int          `json:"priority,omitempty"` // higher wins when several match
	descrRe    *regexp.Regexp
}

// Standard OIDs every device gets.
const (
	OIDSysDescr    = "1.3.6.1.2.1.1.1.0"
	OIDSysObjectID = "1.3.6.1.2.1.1.2.0"
	OIDSysUpTime   = "1.3.6.1.2.1.1.3.0"
	OIDSysContact  = "1.3.6.1.2.1.1.4.0"
	OIDSysName     = "1.3.6.1.2.1.1.5.0"
	OIDSysLocation = "1.3.6.1.2.1.1.6.0"

	OIDIfDescr       = "1.3.6.1.2.1.2.2.1.2"
	OIDIfType        = "1.3.6.1.2.1.2.2.1.3"
	OIDIfPhysAddress = "1.3.6.1.2.1.2.2.1.6"
	OIDIfAdminStatus = "1.3.6.1.2.1.2.2.1.7"
	OIDIfOperStatus  = "1.3.6.1.2.1.2.2.1.8"
	OIDIfInOctets    = "1.3.6.1.2.1.2.2.1.10"
	OIDIfInDiscards  = "1.3.6.1.2.1.2.2.1.13"
	OIDIfInErrors    = "1.3.6.1.2.1.2.2.1.14"
	OIDIfOutOctets   = "1.3.6.1.2.1.2.2.1.16"
	OIDIfOutErrors   = "1.3.6.1.2.1.2.2.1.20"
	OIDIfName        = "1.3.6.1.2.1.31.1.1.1.1"
	OIDIfHCInOctets  = "1.3.6.1.2.1.31.1.1.1.6"
	OIDIfHCOutOctets = "1.3.6.1.2.1.31.1.1.1.10"
	OIDIfHighSpeed   = "1.3.6.1.2.1.31.1.1.1.15"
	OIDIfAlias       = "1.3.6.1.2.1.31.1.1.1.18"

	OIDHrProcessorLoad = "1.3.6.1.2.1.25.3.3.1.2"
	OIDHrStorageType   = "1.3.6.1.2.1.25.2.3.1.2"
	OIDHrStorageAlloc  = "1.3.6.1.2.1.25.2.3.1.4"
	OIDHrStorageSize   = "1.3.6.1.2.1.25.2.3.1.5"
	OIDHrStorageUsed   = "1.3.6.1.2.1.25.2.3.1.6"
	OIDHrStorageRAM    = "1.3.6.1.2.1.25.2.1.2"

	OIDEntPhysicalClass  = "1.3.6.1.2.1.47.1.1.1.1.5"
	OIDEntPhysicalSerial = "1.3.6.1.2.1.47.1.1.1.1.11"
	OIDEntPhysicalModel  = "1.3.6.1.2.1.47.1.1.1.1.13"
	OIDDot1dBaseBridge   = "1.3.6.1.2.1.17.1.1.0"

	OIDLldpLocPortID     = "1.0.8802.1.1.2.1.3.7.1.3"
	OIDLldpLocPortDesc   = "1.0.8802.1.1.2.1.3.7.1.4"
	OIDLldpLocChassisID  = "1.0.8802.1.1.2.1.3.2.0"
	OIDLldpLocSysName    = "1.0.8802.1.1.2.1.3.3.0"
	OIDLldpRemChassisSub = "1.0.8802.1.1.2.1.4.1.1.4"
	OIDLldpRemChassisID  = "1.0.8802.1.1.2.1.4.1.1.5"
	OIDLldpRemPortSub    = "1.0.8802.1.1.2.1.4.1.1.6"
	OIDLldpRemPortID     = "1.0.8802.1.1.2.1.4.1.1.7"
	OIDLldpRemPortDesc   = "1.0.8802.1.1.2.1.4.1.1.8"
	OIDLldpRemSysName    = "1.0.8802.1.1.2.1.4.1.1.9"
	OIDLldpRemSysDesc    = "1.0.8802.1.1.2.1.4.1.1.10"

	OIDCdpCacheAddress  = "1.3.6.1.4.1.9.9.23.1.2.1.1.4"
	OIDCdpCacheDeviceID = "1.3.6.1.4.1.9.9.23.1.2.1.1.6"
	OIDCdpCachePort     = "1.3.6.1.4.1.9.9.23.1.2.1.1.7"
	OIDCdpCachePlatform = "1.3.6.1.4.1.9.9.23.1.2.1.1.8"
)

var builtin = []Profile{
	{ID: "cisco-ios", Vendor: "Cisco", Match: []string{"1.3.6.1.4.1.9.1"}, DescrMatch: `(?i)cisco ios|ios-xe|catalyst`, Role: model.RoleAccess,
		CPU:     &Metric{OID: "1.3.6.1.4.1.9.9.109.1.1.1.1.7", Walk: true, Agg: "avg"},
		MemUsed: &Metric{OID: "1.3.6.1.4.1.9.9.48.1.1.1.5", Walk: true, Agg: "sum"}, MemFree: &Metric{OID: "1.3.6.1.4.1.9.9.48.1.1.1.6", Walk: true, Agg: "sum"},
		Temp: &Metric{OID: "1.3.6.1.4.1.9.9.13.1.3.1.3", Walk: true, Agg: "max"}, LLDP: true, CDP: true, Priority: 5},
	{ID: "cisco-nxos", Vendor: "Cisco", Match: []string{"1.3.6.1.4.1.9.12.3.1.3"}, DescrMatch: `(?i)nx-os|nexus`, Role: model.RoleCore,
		CPU:     &Metric{OID: "1.3.6.1.4.1.9.9.109.1.1.1.1.7", Walk: true, Agg: "avg"},
		MemUsed: &Metric{OID: "1.3.6.1.4.1.9.9.48.1.1.1.5", Walk: true, Agg: "sum"}, MemFree: &Metric{OID: "1.3.6.1.4.1.9.9.48.1.1.1.6", Walk: true, Agg: "sum"},
		Temp: &Metric{OID: "1.3.6.1.4.1.9.9.13.1.3.1.3", Walk: true, Agg: "max"}, LLDP: true, CDP: true, Priority: 6},
	{ID: "cisco-asa", Vendor: "Cisco", Match: nil, DescrMatch: `(?i)adaptive security appliance|firepower|asa`, Role: model.RoleFirewall, Domain: model.DomainSecurity,
		CPU:     &Metric{OID: "1.3.6.1.4.1.9.9.109.1.1.1.1.7", Walk: true, Agg: "avg"},
		MemUsed: &Metric{OID: "1.3.6.1.4.1.9.9.48.1.1.1.5", Walk: true, Agg: "sum"}, MemFree: &Metric{OID: "1.3.6.1.4.1.9.9.48.1.1.1.6", Walk: true, Agg: "sum"},
		Sessions: &Metric{OID: "1.3.6.1.4.1.9.9.147.1.2.2.2.1.5.40.6"}, LLDP: false, CDP: true, Priority: 9},
	{ID: "fortinet-fortigate", Vendor: "Fortinet", Match: []string{"1.3.6.1.4.1.12356.101"}, Role: model.RoleFirewall, Domain: model.DomainSecurity,
		CPU: &Metric{OID: "1.3.6.1.4.1.12356.101.4.1.3.0"}, MemUsed: &Metric{OID: "1.3.6.1.4.1.12356.101.4.1.4.0"}, MemIsPct: true,
		Sessions: &Metric{OID: "1.3.6.1.4.1.12356.101.4.1.8.0"}, LLDP: true, Priority: 8},
	{ID: "paloalto", Vendor: "Palo Alto Networks", Match: []string{"1.3.6.1.4.1.25461"}, Role: model.RoleFirewall, Domain: model.DomainSecurity,
		CPU: &Metric{OID: OIDHrProcessorLoad, Walk: true, Agg: "avg"}, Sessions: &Metric{OID: "1.3.6.1.4.1.25461.2.1.2.3.3.0"}, LLDP: true, Priority: 8},
	{ID: "juniper", Vendor: "Juniper", Match: []string{"1.3.6.1.4.1.2636"}, Role: model.RoleRouter,
		CPU: &Metric{OID: "1.3.6.1.4.1.2636.3.1.13.1.8", Walk: true, Agg: "avg"}, Temp: &Metric{OID: "1.3.6.1.4.1.2636.3.1.13.1.7", Walk: true, Agg: "max"},
		MemUsed: &Metric{OID: "1.3.6.1.4.1.2636.3.1.13.1.11", Walk: true, Agg: "avg"}, MemIsPct: true, LLDP: true, Priority: 5},
	{ID: "aruba-aos-s", Vendor: "Aruba/HPE", Match: []string{"1.3.6.1.4.1.11.2.3.7.11"}, Role: model.RoleAccess,
		CPU: &Metric{OID: "1.3.6.1.4.1.11.2.14.11.5.1.9.6.1.0"}, LLDP: true, CDP: true, Priority: 5},
	{ID: "aruba-aos-cx", Vendor: "Aruba/HPE", Match: []string{"1.3.6.1.4.1.47196"}, Role: model.RoleAccess,
		CPU: &Metric{OID: OIDHrProcessorLoad, Walk: true, Agg: "avg"}, LLDP: true, Priority: 5},
	{ID: "mikrotik", Vendor: "MikroTik", Match: []string{"1.3.6.1.4.1.14988"}, Role: model.RoleRouter,
		CPU: &Metric{OID: OIDHrProcessorLoad, Walk: true, Agg: "avg"}, Temp: &Metric{OID: "1.3.6.1.4.1.14988.1.1.3.10.0", Scale: 0.1}, LLDP: true, Priority: 5},
	{ID: "huawei", Vendor: "Huawei", Match: []string{"1.3.6.1.4.1.2011"}, Role: model.RoleAccess,
		CPU: &Metric{OID: "1.3.6.1.4.1.2011.5.25.31.1.1.1.1.5", Walk: true, Agg: "avg"}, MemUsed: &Metric{OID: "1.3.6.1.4.1.2011.5.25.31.1.1.1.1.7", Walk: true, Agg: "avg"}, MemIsPct: true,
		Temp: &Metric{OID: "1.3.6.1.4.1.2011.5.25.31.1.1.1.1.11", Walk: true, Agg: "max"}, LLDP: true, Priority: 5},
	{ID: "ubiquiti", Vendor: "Ubiquiti", Match: []string{"1.3.6.1.4.1.41112"}, Role: model.RoleAP,
		CPU: &Metric{OID: OIDHrProcessorLoad, Walk: true, Agg: "avg"}, LLDP: true, Priority: 5},
	{ID: "net-snmp", Vendor: "Linux (net-snmp)", Match: []string{"1.3.6.1.4.1.8072"}, Role: model.RoleServer,
		CPU: &Metric{OID: OIDHrProcessorLoad, Walk: true, Agg: "avg"}, LLDP: true, Priority: 3},
	{ID: "generic", Vendor: "", Match: []string{"1"}, Role: model.RoleOther,
		CPU: &Metric{OID: OIDHrProcessorLoad, Walk: true, Agg: "avg"}, LLDP: true, CDP: false, Priority: 0},
}

// Library holds the compiled profiles.
type Library struct {
	profiles []Profile
}

// Load returns the built-in profiles plus any *.json in dir (optional).
func Load(dir string) *Library {
	lib := &Library{}
	all := append([]Profile(nil), builtin...)
	if dir != "" {
		names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, n := range names {
			b, err := os.ReadFile(n)
			if err != nil {
				continue
			}
			var p Profile
			if json.Unmarshal(b, &p) == nil && p.ID != "" {
				if p.Priority == 0 {
					p.Priority = 10 // user profiles win by default
				}
				all = append(all, p)
			}
		}
	}
	for i := range all {
		if all[i].DescrMatch != "" {
			all[i].descrRe, _ = regexp.Compile(all[i].DescrMatch)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Priority > all[j].Priority })
	lib.profiles = all
	return lib
}

// Match picks the best profile for a device.
func (l *Library) Match(sysObjectID, sysDescr string) Profile {
	sysObjectID = strings.TrimPrefix(sysObjectID, ".")
	var best *Profile
	bestLen := -1
	for i := range l.profiles {
		p := &l.profiles[i]
		matched := false
		mlen := 0
		for _, m := range p.Match {
			if sysObjectID == m || strings.HasPrefix(sysObjectID, m+".") {
				matched = true
				if len(m) > mlen {
					mlen = len(m)
				}
			}
		}
		if p.descrRe != nil && p.descrRe.MatchString(sysDescr) {
			matched = true
			mlen += 1000 // descr matches are deliberate and specific
		}
		if !matched {
			continue
		}
		if best == nil || p.Priority > best.Priority || (p.Priority == best.Priority && mlen > bestLen) {
			best = p
			bestLen = mlen
		}
	}
	if best == nil {
		return l.profiles[len(l.profiles)-1]
	}
	return *best
}

// All returns the profiles (for the admin UI).
func (l *Library) All() []Profile { return append([]Profile(nil), l.profiles...) }
