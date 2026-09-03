package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/model"
)

// Mailer sends a finished report.
type Mailer interface {
	SendHTML(to []string, subj, html string) error
}

// Runner generates scheduled reports and keeps the last ones on disk.
type Runner struct {
	Deps   Deps
	Mail   Mailer
	dir    string // <data>/reports
	mu     sync.Mutex
	Runs   int64
	Failed int64
	Keep   int // stored reports per definition (default 12)
}

// NewRunner builds one; dir "" keeps nothing on disk.
func NewRunner(d Deps, mail Mailer, dataDir string) *Runner {
	r := &Runner{Deps: d, Mail: mail, Keep: 12}
	if dataDir != "" {
		r.dir = filepath.Join(dataDir, "reports")
		_ = os.MkdirAll(r.dir, 0o700)
	}
	return r
}

// Run checks every minute whether a scheduled report is due.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, rep := range r.Deps.Store.Reports() {
				if !rep.Enabled || rep.Schedule == "" || !due(rep, now) {
					continue
				}
				go r.RunReport(rep, now, true)
			}
		}
	}
}

// due decides whether the schedule fires in this minute (local time).
func due(rep model.Report, now time.Time) bool {
	if now.Hour() != rep.Hour || now.Minute() != 0 {
		return false
	}
	if !rep.LastRun.IsZero() && now.Sub(rep.LastRun) < 23*time.Hour {
		return false
	}
	switch rep.Schedule {
	case "daily":
		return true
	case "weekly":
		return now.Weekday() == time.Monday
	case "monthly":
		return now.Day() == 1
	}
	return false
}

// Stored is a report kept on disk.
type Stored struct {
	ReportID string    `json:"report_id"`
	File     string    `json:"file"`
	TS       time.Time `json:"ts"`
	Bytes    int64     `json:"bytes"`
}

// RunReport generates, stores and (when mail is true and recipients exist) mails a report.
func (r *Runner) RunReport(rep model.Report, now time.Time, mail bool) (*Result, string, error) {
	r.mu.Lock()
	r.Runs++
	r.mu.Unlock()
	res := Generate(r.Deps, rep, now)
	page := HTML(res, r.Deps.Instance)
	file := ""
	if r.dir != "" {
		file = rep.ID + "-" + now.UTC().Format("20060102T150405Z") + ".html"
		if err := os.WriteFile(filepath.Join(r.dir, file), []byte(page), 0o600); err == nil {
			r.prune(rep.ID)
		}
	}
	var err error
	if mail && len(rep.EmailTo) > 0 {
		if r.Mail == nil {
			err = fmt.Errorf("no mailer")
		} else {
			err = r.Mail.SendHTML(rep.EmailTo, "["+r.Deps.Instance+"] "+res.Title+" — "+now.Format("2 Jan 2006"), page)
		}
	}
	rep.LastRun = now
	rep.LastErr = ""
	if err != nil {
		rep.LastErr = err.Error()
		r.mu.Lock()
		r.Failed++
		r.mu.Unlock()
	}
	if cur, e := r.Deps.Store.Report(rep.ID); e == nil {
		cur.LastRun, cur.LastErr = rep.LastRun, rep.LastErr
		r.Deps.Store.PutReport(cur)
	}
	return res, file, err
}

func (r *Runner) prune(id string) {
	names, _ := filepath.Glob(filepath.Join(r.dir, id+"-*.html"))
	sort.Strings(names)
	keep := r.Keep
	if keep <= 0 {
		keep = 12
	}
	for len(names) > keep {
		os.Remove(names[0])
		names = names[1:]
	}
}

// List returns stored reports, newest first (all definitions when id is "").
func (r *Runner) List(id string) []Stored {
	if r.dir == "" {
		return nil
	}
	pat := "*.html"
	if id != "" {
		pat = id + "-*.html"
	}
	names, _ := filepath.Glob(filepath.Join(r.dir, pat))
	var out []Stored
	for _, n := range names {
		base := filepath.Base(n)
		i := strings.LastIndex(base, "-")
		if i < 0 {
			continue
		}
		ts, err := time.Parse("20060102T150405Z", strings.TrimSuffix(base[i+1:], ".html"))
		if err != nil {
			continue
		}
		var size int64
		if fi, err := os.Stat(n); err == nil {
			size = fi.Size()
		}
		out = append(out, Stored{ReportID: base[:i], File: base, TS: ts, Bytes: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	return out
}

// Read returns a stored report page.
func (r *Runner) Read(file string) ([]byte, error) {
	if r.dir == "" || strings.ContainsAny(file, "/\\") || !strings.HasSuffix(file, ".html") {
		return nil, fmt.Errorf("bad file")
	}
	return os.ReadFile(filepath.Join(r.dir, file))
}
