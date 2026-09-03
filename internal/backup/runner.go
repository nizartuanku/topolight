package backup

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
)

// Runner schedules configuration backups for every device that has an SSH
// credential (its own, or its site's default).
type Runner struct {
	st     *store.Store
	Cfg    *Store
	Events chan model.Event
	// Concurrency caps simultaneous SSH sessions (default 8).
	Concurrency int

	mu       sync.Mutex
	next     map[string]time.Time
	running  map[string]bool
	failing  map[string]bool
	Runs     int64
	Failures int64
	Changes  int64
}

// New builds a runner.
func New(st *store.Store, cfg *Store) *Runner {
	return &Runner{st: st, Cfg: cfg, Events: make(chan model.Event, 256), Concurrency: 8, next: map[string]time.Time{}, running: map[string]bool{}, failing: map[string]bool{}}
}

// CredFor resolves the SSH credential of a device ("" when none).
func (r *Runner) CredFor(d model.Device) (model.Credential, bool) {
	id := d.SSHCredID
	if id == "" {
		if s, err := r.st.Site(d.SiteID); err == nil {
			id = s.SSHCredID
		}
	}
	if id == "" {
		return model.Credential{}, false
	}
	c, err := r.st.Cred(id)
	if err != nil || !c.IsSSH() {
		return model.Credential{}, false
	}
	return c, true
}

// interval for a device: its own hours, else the global setting (default 24 h); ≤0 disables.
func (r *Runner) interval(d model.Device) time.Duration {
	if d.BackupEvery < 0 {
		return 0
	}
	h := d.BackupEvery
	if h == 0 {
		h = r.st.Settings().BackupEveryHours
		if h == 0 {
			h = 24
		}
	}
	return time.Duration(h) * time.Hour
}

// Run schedules until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	sem := make(chan struct{}, max(1, r.Concurrency))
	// stagger the first pass over 10 minutes so a restart does not open 500 SSH sessions at once
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if r.st.Settings().BackupEveryHours < 0 {
				continue
			}
			for _, d := range r.st.Devices() {
				if !d.Monitored || d.PingOnly {
					continue
				}
				if _, ok := r.CredFor(d); !ok {
					continue
				}
				every := r.interval(d)
				if every <= 0 {
					continue
				}
				r.mu.Lock()
				nx, ok := r.next[d.ID]
				if !ok {
					// first run: soon, spread; afterwards: from the last stored attempt
					_, st := r.Cfg.Versions(d.ID)
					if !st.LastTry.IsZero() && now.Sub(st.LastTry) < every {
						nx = st.LastTry.Add(every)
					} else {
						nx = now.Add(time.Duration(len(r.next)%20) * 30 * time.Second)
					}
					r.next[d.ID] = nx
				}
				due := !now.Before(nx) && !r.running[d.ID]
				if due {
					r.running[d.ID] = true
					r.next[d.ID] = now.Add(every)
				}
				r.mu.Unlock()
				if !due {
					continue
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				go func(d model.Device) {
					defer func() { <-sem }()
					r.Backup(ctx, d, "schedule")
					r.mu.Lock()
					r.running[d.ID] = false
					r.mu.Unlock()
				}(d)
			}
		}
	}
}

// Trigger asks for a backup in about two minutes (debounces bursts of
// "config changed" syslog lines while an engineer is still typing).
func (r *Runner) Trigger(deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	nx := time.Now().Add(2 * time.Minute)
	if cur, ok := r.next[deviceID]; !ok || cur.After(nx) {
		r.next[deviceID] = nx
	}
}

// Forget drops per-device state and history.
func (r *Runner) Forget(deviceID string) {
	r.mu.Lock()
	delete(r.next, deviceID)
	delete(r.running, deviceID)
	delete(r.failing, deviceID)
	r.mu.Unlock()
	r.Cfg.Forget(deviceID)
}

// Backup pulls the configuration of one device now and stores it.
func (r *Runner) Backup(ctx context.Context, d model.Device, source string) (Version, bool, error) {
	cred, ok := r.CredFor(d)
	if !ok {
		return Version{}, false, fmt.Errorf("no SSH credential for %s", d.Name)
	}
	r.mu.Lock()
	r.Runs++
	r.mu.Unlock()
	rc := RecipeFor(d.ProfileID)
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	now := time.Now()
	raw, err := Fetch(cctx, d.IP, Auth{User: cred.User, Password: cred.Password, PrivateKey: cred.PrivateKey, EnablePassword: cred.EnablePass, Port: cred.Port}, rc)
	if err == nil && len(strings.TrimSpace(raw)) < 20 {
		err = fmt.Errorf("empty output from %q — wrong recipe for this device? (profile %s)", strings.Split(rc.Show, "\n")[0], d.ProfileID)
	}
	if err != nil {
		r.Cfg.Fail(d.ID, err.Error(), now)
		r.mu.Lock()
		r.Failures++
		first := !r.failing[d.ID]
		r.failing[d.ID] = true
		r.mu.Unlock()
		if first {
			r.emit(model.Event{Kind: "config_backup_failed", DeviceID: d.ID, Source: "ssh", Severity: model.SevMinor, Domain: d.Domain,
				Message: fmt.Sprintf("Configuration backup of %s failed: %s", d.Name, err.Error()), DedupKey: "config_backup_failed:" + d.ID})
		}
		return Version{}, false, err
	}
	v, changed, perr := r.Cfg.Put(d.ID, raw, Normalise(raw, rc), source, "", now, func(s string) string { return Normalise(s, rc) })
	if perr != nil {
		return v, false, perr
	}
	r.mu.Lock()
	wasFailing := r.failing[d.ID]
	r.failing[d.ID] = false
	if changed {
		r.Changes++
	}
	r.mu.Unlock()
	if wasFailing {
		r.emit(model.Event{Kind: "config_backup_ok", DeviceID: d.ID, Source: "ssh", Severity: model.SevInfo, Domain: d.Domain,
			Message: fmt.Sprintf("Configuration backup of %s working again", d.Name), DedupKey: "config_backup_failed:" + d.ID})
	}
	if changed && v.Added+v.Removed > 0 {
		r.emit(model.Event{Kind: "config_backup_changed", DeviceID: d.ID, Source: "ssh", Severity: model.SevInfo, Domain: d.Domain,
			Message: fmt.Sprintf("Configuration of %s changed: +%d −%d lines (version %s)", d.Name, v.Added, v.Removed, v.ID),
			Attrs:   map[string]string{"version": v.ID, "added": fmt.Sprint(v.Added), "removed": fmt.Sprint(v.Removed)}, DedupKey: "config_backup_changed:" + d.ID + ":" + v.ID})
	}
	return v, changed, nil
}

func (r *Runner) emit(ev model.Event) {
	ev.TS = time.Now()
	select {
	case r.Events <- ev:
	default:
	}
}

// Stats for Admin → System.
func (r *Runner) Stats() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{"runs": r.Runs, "failures": r.Failures, "changes": r.Changes, "disk_bytes": r.Cfg.DiskUsage()}
}
