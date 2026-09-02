package state

import "github.com/nizartuanku/topolight/internal/model"

// DefaultRules are seeded into the store on first run. Admins edit them in
// the UI; the ids are stable so upgrades can add new defaults without
// touching customised ones.
var DefaultRules = []model.Rule{
	{ID: "device_down", Object: "device", ForCycles: 3, Severity: model.SevMajor, Enabled: true,
		Description: "Device does not answer ICMP (and SNMP) for 3 consecutive cycles. Critical for core, router and firewall roles.", Runbook: "device-down"},
	{ID: "snmp_unreachable", Object: "device", ForCycles: 3, Severity: model.SevMinor, Enabled: true,
		Description: "Device answers ping but not SNMP for 3 cycles — credentials, ACL or agent problem."},
	{ID: "icmp_loss", Object: "device", Metric: "icmp_loss_pct", Enter: 20, Exit: 5, ForCycles: 2, Severity: model.SevMinor, Enabled: true,
		Description: "Packet loss to the device above 20% (clears below 5%)."},
	{ID: "icmp_latency", Object: "device", Metric: "icmp_rtt_ms", Enter: 150, Exit: 100, ForCycles: 3, Severity: model.SevMinor, Enabled: true,
		Description: "Round-trip time above 150 ms for 3 cycles (clears below 100 ms)."},
	{ID: "device_cpu_high", Object: "device", Metric: "cpu_pct", Enter: 85, Exit: 70, ForCycles: 5, Severity: model.SevMajor, Enabled: true,
		Description: "CPU above 85% for 5 cycles (clears below 70%)."},
	{ID: "device_mem_high", Object: "device", Metric: "mem_pct", Enter: 90, Exit: 80, ForCycles: 5, Severity: model.SevMinor, Enabled: true,
		Description: "Memory above 90% for 5 cycles (clears below 80%)."},
	{ID: "device_temp_high", Object: "device", Metric: "temp_c", Enter: 70, Exit: 60, ForCycles: 2, Severity: model.SevMajor, Enabled: true,
		Description: "Temperature sensor above 70 °C (clears below 60 °C)."},
	{ID: "interface_down", Object: "interface", ForCycles: 1, Severity: model.SevMajor, OnlyImport: true, Enabled: true,
		Description: "An important interface (uplink, trunk, LAG, link to another infrastructure device) went down. Access ports only log an event.", Runbook: "interface-down"},
	{ID: "interface_util_high", Object: "interface", Metric: "util_pct", Enter: 85, Exit: 70, ForCycles: 5, Severity: model.SevMinor, Escalate: 95, OnlyImport: true, Enabled: true,
		Description: "Utilisation above 85% for 5 minutes on an important interface; Major above 95%."},
	{ID: "interface_errors", Object: "interface", Metric: "err_rate", Enter: 1, Exit: 0.1, ForCycles: 3, Severity: model.SevMinor, OnlyImport: true, Enabled: true,
		Description: "More than 1 error per second for 3 cycles on an important interface."},
	{ID: "flapping", Object: "any", ForCycles: 5, Severity: model.SevMajor, Enabled: true,
		Description: "More than 5 state changes in 10 minutes. Individual transitions are folded into one alert."},
	{ID: "site_down", Object: "site", Enter: 80, Severity: model.SevCritical, Enabled: true,
		Description: "80% or more of a site's monitored devices are down — most likely WAN or power. Device alerts are folded under it."},
	{ID: "device_rebooted", Object: "event", Severity: model.SevMajor, ForCycles: 30, Enabled: true,
		Description: "sysUpTime went backwards or a coldStart/warmStart trap arrived. Auto-resolves after 30 minutes."},
	{ID: "config_changed", Object: "event", Severity: model.SevMinor, ForCycles: 30, Enabled: true,
		Description: "Configuration changed (syslog). Auto-resolves after 30 minutes; use maintenance windows for planned work."},
	{ID: "bgp_neighbor_down", Object: "event", Severity: model.SevMajor, Enabled: true,
		Description: "BGP neighbour left Established. Resolves when the neighbour comes back."},
	{ID: "ospf_adjacency_down", Object: "event", Severity: model.SevMajor, Enabled: true, Description: "OSPF adjacency lost."},
	{ID: "ha_state_change", Object: "event", Severity: model.SevMajor, ForCycles: 60, Enabled: true, Description: "Firewall/cluster HA state changed. Auto-resolves after 60 minutes."},
	{ID: "environment_fault", Object: "event", Severity: model.SevMajor, ForCycles: 120, Enabled: true, Description: "Fan, PSU or temperature notification. Auto-resolves after 2 hours."},
	{ID: "critical_log", Object: "event", Severity: model.SevMajor, ForCycles: 60, Enabled: true, Description: "A syslog message with severity emergency/alert/critical."},
	{ID: "auth_failure", Object: "event", Severity: model.SevMinor, Enter: 20, ForCycles: 5, Enabled: true, Description: "More than 20 authentication failures within 5 minutes on one device."},
	{ID: "log_flood", Object: "event", Severity: model.SevMinor, ForCycles: 30, Enabled: true, Description: "A source exceeds the per-source log rate; excess lines are dropped."},
	{ID: "vpn_tunnel_change", Object: "event", Severity: model.SevMinor, ForCycles: 30, Enabled: true, Description: "VPN tunnel state changed."},
	{ID: "device_over_cap", Object: "event", Severity: model.SevMinor, ForCycles: 1440, Enabled: true, Description: "Devices were discovered beyond the licence limit and are not monitored."},
	{ID: "neighbor_changed", Object: "event", Severity: model.SevMinor, ForCycles: 60, Enabled: true, Description: "An important port now sees a different neighbour than before."},
}
