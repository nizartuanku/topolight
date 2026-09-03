// Package notify delivers alerts by e-mail, Telegram and signed webhook,
// grouped per site and throttled by quiet hours. A failed delivery is recorded
// as an event, never swallowed.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nizartuanku/topolight/internal/license"
	"github.com/nizartuanku/topolight/internal/model"
	"github.com/nizartuanku/topolight/internal/store"
)

// SMTP settings come from flags/env, never from the UI.
type SMTP struct {
	Host, User, Pass, From string
	Port                   int
	StartTLS               bool
}

// Telegram bot settings.
type Telegram struct {
	Token string
}

// Dispatcher groups and sends.
type Dispatcher struct {
	st       *store.Store
	Caps     func() license.Caps
	SMTP     SMTP
	Telegram Telegram
	// WebhookSecret signs webhook bodies (X-TopoLight-Signature: sha256=...).
	WebhookSecret string
	ConsoleURL    string
	Events        chan model.Event
	HTTP          *http.Client

	mu      sync.Mutex
	pending []model.Alert
	Sent    int64
	Failed  int64
	last    map[string]time.Time
}

// New builds a dispatcher.
func New(st *store.Store, caps func() license.Caps) *Dispatcher {
	return &Dispatcher{st: st, Caps: caps, Events: make(chan model.Event, 256), HTTP: &http.Client{Timeout: 15 * time.Second}, last: map[string]time.Time{}}
}

// Run drains the alert channel and flushes groups until ctx ends.
func (d *Dispatcher) Run(ctx context.Context, in <-chan model.Alert) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-in:
			d.mu.Lock()
			d.pending = append(d.pending, a)
			d.mu.Unlock()
		case <-tick.C:
			d.flush(false)
		}
	}
}

func (d *Dispatcher) flush(force bool) {
	cfg := d.st.Notify()
	group := time.Duration(cfg.GroupSeconds) * time.Second
	if group <= 0 {
		group = 60 * time.Second
	}
	d.mu.Lock()
	if len(d.pending) == 0 {
		d.mu.Unlock()
		return
	}
	oldest := d.pending[0].UpdatedAt
	hasCritical := false
	for _, a := range d.pending {
		if a.Severity == model.SevCritical && a.State != model.AlertResolved {
			hasCritical = true
		}
	}
	if !force && !hasCritical && time.Since(oldest) < group {
		d.mu.Unlock()
		return
	}
	batch := d.pending
	d.pending = nil
	d.mu.Unlock()

	var send []model.Alert
	for _, a := range batch {
		if a.State == model.AlertResolved && !cfg.ResolvedToo {
			continue
		}
		if a.Severity.Rank() < cfg.MinSeverity.Rank() && a.State != model.AlertResolved {
			continue
		}
		if a.State == model.AlertResolved {
			// only announce resolution of alerts that were notified
			if !a.Notified {
				continue
			}
		}
		if inQuiet(cfg, time.Now()) && !(cfg.CriticalAlways && a.Severity == model.SevCritical) {
			continue
		}
		send = append(send, a)
	}
	if len(send) == 0 {
		return
	}
	sort.SliceStable(send, func(i, j int) bool { return send[i].Severity.Rank() > send[j].Severity.Rank() })
	caps := d.Caps()
	text := d.render(send)
	var errs []string
	if len(cfg.EmailTo) > 0 && d.SMTP.Host != "" {
		if err := d.sendMail(cfg.EmailTo, subject(send), text); err != nil {
			errs = append(errs, "email: "+err.Error())
		} else {
			d.count(true)
		}
	}
	if caps.Telegram && cfg.TelegramChat != "" && d.Telegram.Token != "" {
		if err := d.sendTelegram(cfg.TelegramChat, d.renderTelegram(send)); err != nil {
			errs = append(errs, "telegram: "+err.Error())
		} else {
			d.count(true)
		}
	}
	if caps.Webhook && cfg.WebhookURL != "" {
		if err := d.sendWebhook(cfg.WebhookURL, send); err != nil {
			errs = append(errs, "webhook: "+err.Error())
		} else {
			d.count(true)
		}
	}
	for _, a := range send {
		if a.State != model.AlertResolved {
			d.st.UpdateAlert(a.ID, func(x *model.Alert) { x.Notified = true })
		}
	}
	for _, e := range errs {
		d.count(false)
		select {
		case d.Events <- model.Event{TS: time.Now(), Kind: "notify_failed", Source: "notify", Severity: model.SevMinor, Domain: model.DomainNetwork, Message: "Notification failed — " + e, DedupKey: "notify_failed"}:
		default:
		}
	}
}

func (d *Dispatcher) count(ok bool) {
	d.mu.Lock()
	if ok {
		d.Sent++
	} else {
		d.Failed++
	}
	d.mu.Unlock()
}

// Flush sends whatever is pending now (used by the "send test" button).
func (d *Dispatcher) Flush() { d.flush(true) }

func inQuiet(cfg model.Notify, now time.Time) bool {
	if cfg.QuietFrom == "" || cfg.QuietTo == "" {
		return false
	}
	parse := func(s string) (int, bool) {
		var h, m int
		if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
			return 0, false
		}
		return h*60 + m, true
	}
	from, ok1 := parse(cfg.QuietFrom)
	to, ok2 := parse(cfg.QuietTo)
	if !ok1 || !ok2 {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	if from <= to {
		return cur >= from && cur < to
	}
	return cur >= from || cur < to
}

func icon(a model.Alert) string {
	if a.State == model.AlertResolved {
		return "🟢"
	}
	switch a.Severity {
	case model.SevCritical:
		return "🔴"
	case model.SevMajor:
		return "🟠"
	case model.SevMinor:
		return "🟡"
	}
	return "🔵"
}

func subject(send []model.Alert) string {
	a := send[0]
	state := strings.ToUpper(string(a.Severity))
	if a.State == model.AlertResolved {
		state = "RESOLVED"
	}
	s := fmt.Sprintf("[TopoLight] %s · %s", state, a.Title)
	if len(send) > 1 {
		s += fmt.Sprintf(" (+%d more)", len(send)-1)
	}
	return s
}

func (d *Dispatcher) siteName(id string) string {
	if s, err := d.st.Site(id); err == nil {
		return s.Name
	}
	return "—"
}

func (d *Dispatcher) render(send []model.Alert) string {
	var b strings.Builder
	for _, a := range send {
		state := strings.ToUpper(string(a.Severity))
		if a.State == model.AlertResolved {
			state = "RESOLVED"
		}
		fmt.Fprintf(&b, "%s %s · %s · %s\n", icon(a), state, d.siteName(a.SiteID), a.Title)
		if a.Detail != "" {
			fmt.Fprintf(&b, "   %s\n", a.Detail)
		}
		if a.Impact != "" {
			fmt.Fprintf(&b, "   Impact: %s\n", a.Impact)
		}
		if a.State == model.AlertResolved && !a.OpenedAt.IsZero() {
			fmt.Fprintf(&b, "   Duration: %s\n", a.ResolvedAt.Sub(a.OpenedAt).Round(time.Second))
		} else {
			fmt.Fprintf(&b, "   Opened: %s\n", a.OpenedAt.Format("2 Jan 15:04:05"))
		}
		if d.ConsoleURL != "" {
			fmt.Fprintf(&b, "   %s/#/alerts/%s\n", strings.TrimRight(d.ConsoleURL, "/"), a.ID)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (d *Dispatcher) renderTelegram(send []model.Alert) string {
	var b strings.Builder
	for i, a := range send {
		if i >= 10 {
			fmt.Fprintf(&b, "…and %d more\n", len(send)-10)
			break
		}
		state := strings.ToUpper(string(a.Severity))
		if a.State == model.AlertResolved {
			state = "RESOLVED"
		}
		fmt.Fprintf(&b, "%s %s · %s · %s", icon(a), state, d.siteName(a.SiteID), a.Title)
		if a.Impact != "" {
			fmt.Fprintf(&b, " · %s", a.Impact)
		}
		if a.State == model.AlertResolved {
			fmt.Fprintf(&b, " · %s", a.ResolvedAt.Sub(a.OpenedAt).Round(time.Second))
		} else {
			fmt.Fprintf(&b, " · %s", a.OpenedAt.Format("15:04"))
		}
		b.WriteString("\n")
	}
	if d.ConsoleURL != "" {
		fmt.Fprintf(&b, "%s/#/alerts", strings.TrimRight(d.ConsoleURL, "/"))
	}
	return b.String()
}

func (d *Dispatcher) sendMail(to []string, subj, body string) error {
	return d.sendMailType(to, subj, body, "text/plain")
}

// SendHTML mails an HTML document (reports).
func (d *Dispatcher) SendHTML(to []string, subj, html string) error {
	if d.SMTP.Host == "" {
		return fmt.Errorf("SMTP not configured (-smtp-host)")
	}
	return d.sendMailType(to, subj, html, "text/html")
}

func (d *Dispatcher) sendMailType(to []string, subj, body, ctype string) error {
	addr := net.JoinHostPort(d.SMTP.Host, fmt.Sprint(portOr(d.SMTP.Port, 587)))
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: %s; charset=UTF-8\r\nDate: %s\r\n\r\n%s",
		d.SMTP.From, strings.Join(to, ", "), subj, ctype, time.Now().Format(time.RFC1123Z), body)
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if d.SMTP.StartTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: d.SMTP.Host}); err != nil {
				return err
			}
		}
	}
	if d.SMTP.User != "" {
		if err := c.Auth(smtp.PlainAuth("", d.SMTP.User, d.SMTP.Pass, d.SMTP.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(d.SMTP.From); err != nil {
		return err
	}
	for _, t := range to {
		if err := c.Rcpt(t); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func portOr(p, def int) int {
	if p <= 0 {
		return def
	}
	return p
}

func (d *Dispatcher) sendTelegram(chat, text string) error {
	form := url.Values{"chat_id": {chat}, "text": {text}, "disable_web_page_preview": {"true"}}
	resp, err := d.HTTP.PostForm("https://api.telegram.org/bot"+d.Telegram.Token+"/sendMessage", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// WebhookPayload is the JSON body sent to webhooks.
type WebhookPayload struct {
	Version string        `json:"version"`
	SentAt  time.Time     `json:"sent_at"`
	Alerts  []model.Alert `json:"alerts"`
	Console string        `json:"console_url,omitempty"`
}

func (d *Dispatcher) sendWebhook(u string, send []model.Alert) error {
	body, err := json.Marshal(WebhookPayload{Version: "1", SentAt: time.Now(), Alerts: send, Console: d.ConsoleURL})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TopoLight")
	if d.WebhookSecret != "" {
		mac := hmac.New(sha256.New, []byte(d.WebhookSecret))
		mac.Write(body)
		req.Header.Set("X-TopoLight-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
	}
	return nil
}

// Test sends a synthetic alert through every configured channel right now.
func (d *Dispatcher) Test() []string {
	a := model.Alert{ID: "test", Rule: "test", Severity: model.SevMinor, State: model.AlertOpen, Domain: model.DomainNetwork,
		Title: "Test notification from TopoLight", Detail: "If you can read this, the channel works.", OpenedAt: time.Now(), UpdatedAt: time.Now(), Occurrences: 1}
	cfg := d.st.Notify()
	caps := d.Caps()
	var out []string
	if len(cfg.EmailTo) > 0 {
		if d.SMTP.Host == "" {
			out = append(out, "email: SMTP host not configured (start with -smtp-host)")
		} else if err := d.sendMail(cfg.EmailTo, subject([]model.Alert{a}), d.render([]model.Alert{a})); err != nil {
			out = append(out, "email: "+err.Error())
		} else {
			out = append(out, "email: sent")
		}
	}
	if cfg.TelegramChat != "" {
		switch {
		case !caps.Telegram:
			out = append(out, "telegram: not included in the "+caps.Tier.Title()+" tier")
		case d.Telegram.Token == "":
			out = append(out, "telegram: bot token not configured (start with -telegram-token)")
		default:
			if err := d.sendTelegram(cfg.TelegramChat, d.renderTelegram([]model.Alert{a})); err != nil {
				out = append(out, "telegram: "+err.Error())
			} else {
				out = append(out, "telegram: sent")
			}
		}
	}
	if cfg.WebhookURL != "" {
		if !caps.Webhook {
			out = append(out, "webhook: not included in the "+caps.Tier.Title()+" tier")
		} else if err := d.sendWebhook(cfg.WebhookURL, []model.Alert{a}); err != nil {
			out = append(out, "webhook: "+err.Error())
		} else {
			out = append(out, "webhook: sent")
		}
	}
	if len(out) == 0 {
		out = append(out, "no channel configured")
	}
	return out
}
