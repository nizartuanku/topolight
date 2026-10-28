// Package siemexport sends TopoLight alerts to a SIEM as Hexward Finding
// Schema events (github.com/nizartuanku/hexward-core), the shape every
// Hexward tool emits, so one Wazuh rule set or Splunk search covers them all.
//
// An alert that opens is sent as "opened" and one that clears as
// "resolved". It sees the same alerts the notification dispatcher sees —
// root causes only, so one switch going down is one event, not one per
// device behind it — but before quiet hours, minimum severity and grouping,
// which are for people, not for a SIEM.
package siemexport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/nizartuanku/hexward-core/hfs"
	"github.com/nizartuanku/hexward-core/sink"

	"github.com/nizartuanku/topolight/internal/model"
)

// Directory resolves the device and site an alert refers to. The store
// satisfies it.
type Directory interface {
	Device(id string) (model.Device, error)
	Site(id string) (model.Site, error)
	Interface(id string) (model.Interface, error)
	Probe(id string) (model.Probe, error)
}

// Exporter fans alerts out to every configured sink.
type Exporter struct {
	Sinks   []sink.Sink
	Meta    hfs.Meta
	Options hfs.Options
	Dir     Directory // optional

	// Batch and Every bound how long an alert waits: it leaves when Batch
	// alerts are queued or Every has passed, whichever comes first.
	Batch int           // default 50
	Every time.Duration // default 2s
}

// Severity maps TopoLight's four levels onto the schema's five.
func Severity(s model.Severity) string {
	switch s {
	case model.SevCritical:
		return "critical"
	case model.SevMajor:
		return "high"
	case model.SevMinor:
		return "medium"
	case model.SevInfo:
		return "info"
	}
	return "low"
}

// Event turns one alert into an HFS event.
func (x *Exporter) Event(a model.Alert, at time.Time) hfs.Event {
	kind := hfs.KindOpened
	if a.State == model.AlertResolved {
		kind = hfs.KindResolved
	}
	if at.IsZero() {
		at = a.UpdatedAt
	}
	return hfs.New(kind, x.Meta, x.Source(a), at, x.Options)
}

// Source maps an alert onto the schema's input. The dedup key names the
// condition (rule and object), so it is the fingerprint: a link that drops
// again next week is the same finding reopening. The alert ID is the
// instance.
func (x *Exporter) Source(a model.Alert) hfs.Source {
	fp := a.DedupKey
	if fp == "" {
		fp = a.ID
	}
	ev := map[string]any{
		"rule":        a.Rule,
		"domain":      string(a.Domain),
		"occurrences": a.Occurrences,
	}
	if a.Object != "" {
		ev["object"] = a.Object
	}
	if a.Impact != "" {
		ev["impact"] = a.Impact
	}
	if a.Children > 0 {
		ev["suppressed_children"] = a.Children
	}
	if len(a.Evidence) > 0 {
		ev["timeline"] = append([]string(nil), a.Evidence...)
	}
	if a.AckedBy != "" {
		ev["acked_by"] = a.AckedBy
	}

	target := a.DeviceID
	if x.Dir != nil && a.DeviceID != "" {
		if d, err := x.Dir.Device(a.DeviceID); err == nil {
			// device_ip, not src_ip: the device is what is affected, not
			// the source of an attack, and must not be correlated as one.
			ev["device_ip"] = d.IP
			ev["device"] = d.Name
			switch {
			case d.Name != "" && d.IP != "" && d.Name != d.IP:
				target = d.Name + " (" + d.IP + ")"
			case d.Name != "":
				target = d.Name
			case d.IP != "":
				target = d.IP
			}
			if d.Vendor != "" {
				ev["vendor"] = d.Vendor
			}
		}
	}
	if x.Dir != nil && a.Object != "" && a.Object != a.DeviceID {
		if p, err := x.Dir.Probe(a.Object); err == nil {
			ev["probe"] = p.Name
			ev["probe_target"] = p.Target
			ev["probe_type"] = p.Type
			if target == "" || target == a.DeviceID {
				target = p.Target
			}
		} else if i, err := x.Dir.Interface(a.Object); err == nil && i.Name != "" {
			ev["interface"] = i.Name
			if i.Alias != "" {
				ev["interface_alias"] = i.Alias
			}
			target += " " + i.Name
		}
	}
	if target == "" {
		target = a.Object
	}

	var group *string
	if a.SiteID != "" {
		site := a.SiteID
		if x.Dir != nil {
			if s, err := x.Dir.Site(a.SiteID); err == nil && s.Name != "" {
				ev["site"] = s.Name
			}
		}
		group = &site
	}

	status := string(a.State)
	if a.State == model.AlertAcked {
		status = "acknowledged"
	}
	src := hfs.Source{
		ID:          a.ID,
		Fingerprint: fp,
		Check:       "topolight." + a.Rule,
		Title:       a.Title,
		Target:      target,
		Severity:    Severity(a.Severity),
		Status:      status,
		Remediation: a.Detail,
		FirstSeen:   a.OpenedAt,
		LastSeen:    a.UpdatedAt,
		GroupID:     group,
		Evidence:    ev,
	}
	if a.State == model.AlertResolved && !a.ResolvedAt.IsZero() {
		r := a.ResolvedAt
		src.ResolvedAt = &r
	}
	return src
}

// Send delivers a batch to every sink. Every sink is tried; the errors of
// those that failed are joined.
func (x *Exporter) Send(ctx context.Context, alerts []model.Alert) error {
	if len(alerts) == 0 {
		return nil
	}
	evs := make([]hfs.Event, 0, len(alerts))
	for _, a := range alerts {
		evs = append(evs, x.Event(a, time.Time{}))
	}
	var errs []error
	for _, s := range x.Sinks {
		if err := s.Send(ctx, evs); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// Tee copies every alert from in to out and to the exporter. Out is never
// held up by the SIEM: alerts for export queue in their own buffer, and
// when that is full the alert that does not fit is not exported (and is
// counted in the log); a notification is never dropped for the SIEM.
func (x *Exporter) Tee(ctx context.Context, in <-chan model.Alert, out chan<- model.Alert) {
	q := make(chan model.Alert, 4096)
	go x.run(ctx, q)
	dropped := 0
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-in:
			select {
			case out <- a:
			case <-ctx.Done():
				return
			}
			select {
			case q <- a.Clone():
			default:
				dropped++
				if dropped == 1 || dropped%100 == 0 {
					log.Printf("SIEM export: queue full, %d alert(s) not exported", dropped)
				}
			}
		}
	}
}

func (x *Exporter) run(ctx context.Context, q <-chan model.Alert) {
	size := x.Batch
	if size <= 0 {
		size = 50
	}
	every := x.Every
	if every <= 0 {
		every = 2 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	var batch []model.Alert
	flush := func() {
		if len(batch) == 0 {
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := x.Send(sctx, batch); err != nil {
			log.Printf("SIEM export: %v", err)
		}
		cancel()
		batch = nil
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case a := <-q:
			batch = append(batch, a)
			if len(batch) >= size {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}
