package siemexport

import (
	"flag"
	"fmt"
	"net"

	"github.com/nizartuanku/hexward-core/hfs"
	"github.com/nizartuanku/hexward-core/sink"
)

// Flags are the SIEM export settings. Every channel is available in every
// edition, Free included.
type Flags struct {
	file string

	syslogAddr    string
	syslogNetwork string
	syslogFormat  string
	syslogRFC     int
	syslogCAFile  string

	hecURL       string
	hecTokenFile string
	hecIndex     string
	hecCAFile    string

	webhook           string
	webhookSecretFile string

	evidence string
}

// Register adds the export flags to fs.
func Register(fs *flag.FlagSet) *Flags {
	c := &Flags{}
	fs.StringVar(&c.file, "export-file", "", "append findings as Hexward Finding Schema NDJSON to this file, for Wazuh, Splunk or Elastic agents to read")
	fs.StringVar(&c.syslogAddr, "export-syslog", "", "syslog collector host:port that receives alerts as HFS JSON or CEF, e.g. wazuh.example:514 (not the -syslog-listen receiver)")
	fs.StringVar(&c.syslogNetwork, "export-syslog-network", "udp", "transport for -export-syslog: udp, tcp or tls")
	fs.StringVar(&c.syslogFormat, "export-syslog-format", "json", "message format for -export-syslog: json (Hexward Finding Schema) or cef")
	fs.IntVar(&c.syslogRFC, "export-syslog-rfc", 5424, "syslog header for -export-syslog: 5424 or 3164")
	fs.StringVar(&c.syslogCAFile, "export-syslog-ca-file", "", "PEM CA file trusted for -export-syslog-network tls, in addition to the system roots")
	fs.StringVar(&c.hecURL, "hec-url", "", "Splunk HTTP Event Collector URL, e.g. https://splunk.example:8088")
	fs.StringVar(&c.hecTokenFile, "hec-token-file", "", "file holding the Splunk HEC token (required with -hec-url)")
	fs.StringVar(&c.hecIndex, "hec-index", "", "Splunk index for findings (optional; must be allowed for the token)")
	fs.StringVar(&c.hecCAFile, "hec-ca-file", "", "PEM CA file trusted for -hec-url, in addition to the system roots")
	fs.StringVar(&c.webhook, "export-webhook", "", "URL that receives alerts as Hexward Finding Schema events (webhook payload v2; the signed -webhook-secret webhooks are unchanged)")
	fs.StringVar(&c.webhookSecretFile, "export-webhook-secret-file", "", "file holding a shared secret, sent as the X-Hexward-Token header")
	fs.StringVar(&c.evidence, "export-evidence", "redacted", "evidence in exported alerts: redacted (secret-like keys dropped) or none")
	return c
}

// Build turns the flags into an Exporter, or nil when no channel is set. It
// fails on any inconsistent setting, so a typo stops the binary at start-up
// instead of silently dropping alerts later.
func (c *Flags) Build(meta hfs.Meta) (*Exporter, error) {
	mode, ok := hfs.ParseEvidenceMode(c.evidence)
	if !ok {
		return nil, fmt.Errorf("-export-evidence %q: want redacted or none", c.evidence)
	}
	var sinks []sink.Sink

	if c.file != "" {
		sinks = append(sinks, &sink.File{Path: c.file})
	}
	if c.syslogAddr != "" {
		s := &sink.Syslog{Addr: c.syslogAddr, Network: c.syslogNetwork, Format: c.syslogFormat, RFC: c.syslogRFC}
		if c.syslogNetwork == "tls" {
			host, _, _ := net.SplitHostPort(c.syslogAddr)
			tc, err := sink.TLSConfig(c.syslogCAFile, host)
			if err != nil {
				return nil, fmt.Errorf("-export-syslog-ca-file: %w", err)
			}
			s.TLS = tc
		}
		if err := s.Validate(); err != nil {
			return nil, err
		}
		sinks = append(sinks, s)
	}
	if c.hecURL != "" {
		if c.hecTokenFile == "" {
			return nil, fmt.Errorf("-hec-url needs -hec-token-file")
		}
		tok, err := sink.ReadSecretFile(c.hecTokenFile)
		if err != nil {
			return nil, fmt.Errorf("-hec-token-file: %w", err)
		}
		h := &sink.HEC{URL: c.hecURL, Token: tok, Index: c.hecIndex}
		if _, err := h.Endpoint(); err != nil {
			return nil, fmt.Errorf("-hec-url: %w", err)
		}
		if c.hecCAFile != "" {
			tc, err := sink.TLSConfig(c.hecCAFile, "")
			if err != nil {
				return nil, fmt.Errorf("-hec-ca-file: %w", err)
			}
			h.TLS = tc
		}
		sinks = append(sinks, h)
	}
	if c.webhook != "" {
		secret := ""
		if c.webhookSecretFile != "" {
			s, err := sink.ReadSecretFile(c.webhookSecretFile)
			if err != nil {
				return nil, fmt.Errorf("-export-webhook-secret-file: %w", err)
			}
			secret = s
		}
		sinks = append(sinks, &sink.Webhook{URL: c.webhook, Secret: secret})
	}
	if len(sinks) == 0 {
		return nil, nil
	}
	return &Exporter{Sinks: sinks, Meta: meta, Options: hfs.Options{Evidence: mode}}, nil
}

// Names lists the configured sinks, for the start-up banner.
func (x *Exporter) Names() []string {
	if x == nil {
		return nil
	}
	out := make([]string, 0, len(x.Sinks))
	for _, s := range x.Sinks {
		out = append(out, s.Name())
	}
	return out
}
