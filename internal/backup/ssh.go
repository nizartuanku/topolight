// Package backup pulls device configurations over SSH, keeps a versioned
// history on disk, and reports what changed between versions.
package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Recipe is how to get the configuration out of one vendor's CLI.
type Recipe struct {
	// Setup runs before Show (pager off etc.); errors are ignored.
	Setup []string
	// Show prints the configuration.
	Show string
	// Exec uses one non-interactive exec request per command instead of an
	// interactive shell (MikroTik, Junos, Linux hosts).
	Exec bool
	// Prompt matches the CLI prompt at the end of output (shell mode).
	Prompt *regexp.Regexp
	// Ignore lists line patterns dropped before comparing versions (timestamps, counters).
	Ignore []*regexp.Regexp
	// Enable, when set, is sent to enter privileged mode before Setup (Cisco IOS/ASA when
	// the user lands in user EXEC); the enable password comes from the credential.
	Enable string
}

var (
	promptHash  = regexp.MustCompile(`(?m)^[\w.@()\-/:~\[\]]{1,80}[#>$] ?$`)
	promptForti = regexp.MustCompile(`(?m)^[\w\-.]{1,64} (\([\w\-.]+\) )?[#$] ?$`)
)

// Recipes by profile id; "generic" is the fallback.
var Recipes = map[string]Recipe{
	"cisco-ios":          {Setup: []string{"terminal length 0", "terminal width 512"}, Show: "show running-config", Prompt: promptHash, Enable: "enable", Ignore: res(`^! (Last configuration change|NVRAM config last updated|No configuration change since)`, `^! Time:`, `^ntp clock-period`, `^Building configuration`, `^Current configuration`)},
	"cisco-nxos":         {Setup: []string{"terminal length 0", "terminal width 511"}, Show: "show running-config", Prompt: promptHash, Ignore: res(`^!Command:`, `^!Running configuration last done at`, `^!Time:`, `^!No entry for`)},
	"cisco-asa":          {Setup: []string{"terminal pager 0"}, Show: "show running-config", Prompt: promptHash, Enable: "enable", Ignore: res(`^: (Written by|Saved|Serial Number|Hardware|Call-home)`, `^ASA Version`, `^Cryptochecksum`, `^\s*$`)},
	"fortinet-fortigate": {Show: "show", Prompt: promptForti, Ignore: res(`^#conf_file_ver=`, `^#buildno=`, `^#global_vdom=`, `^#config-version=`)},
	"paloalto":           {Setup: []string{"set cli pager off", "set cli config-output-format set"}, Show: "configure\nshow", Prompt: promptHash, Ignore: res(`^Entering configuration mode`, `^\[edit\]`)},
	"juniper":            {Exec: true, Show: "show configuration | display set | no-more", Ignore: res(`^## Last commit:`, `^## Last changed:`, `^# \d{4}-\d\d-\d\d`)},
	"aruba-aos-s":        {Setup: []string{"no page"}, Show: "show running-config", Prompt: promptHash, Ignore: res(`^; .* Configuration Editor; Created on release`, `^; Ver `)},
	"aruba-aos-cx":       {Setup: []string{"no page"}, Show: "show running-config", Prompt: promptHash, Ignore: res(`^Current configuration:`, `^!Version`, `^!export-password`)},
	"mikrotik":           {Exec: true, Show: "/export", Ignore: res(`^# \w{3}/\d\d/\d{4} \d\d:\d\d:\d\d by RouterOS`, `^# [0-9]{4}-[0-9]{2}-[0-9]{2} [0-9:]+ by RouterOS`, `^# software id =`, `^# model =`, `^# serial number =`)},
	"huawei":             {Setup: []string{"screen-length 0 temporary"}, Show: "display current-configuration", Prompt: regexp.MustCompile(`(?m)^[<\[][\w\-.]{1,64}[>\]] ?$`), Ignore: res(`^!Software Version`, `^!Last configuration was`)},
	"ubiquiti":           {Setup: []string{"terminal length 0"}, Show: "show configuration commands", Prompt: promptHash, Ignore: res(`^set system .*(login|time-zone)`)},
	"net-snmp":           {Exec: true, Show: "cat /etc/network/interfaces 2>/dev/null; echo; ip -br addr; echo; ip route", Ignore: res(`^\s*valid_lft`)},
	"generic":            {Setup: []string{"terminal length 0"}, Show: "show running-config", Prompt: promptHash, Ignore: res(`^!.*(change|updated|Time)`)},
}

func res(p ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(p))
	for _, x := range p {
		out = append(out, regexp.MustCompile(x))
	}
	return out
}

// RecipeFor picks the recipe for a profile id.
func RecipeFor(profileID string) Recipe {
	if r, ok := Recipes[profileID]; ok {
		return r
	}
	return Recipes["generic"]
}

// Auth is the SSH credential.
type Auth struct {
	User, Password, PrivateKey, EnablePassword string
	Port                                       int
}

// Fetch connects and returns the raw configuration text.
func Fetch(ctx context.Context, host string, a Auth, rc Recipe) (string, error) {
	cfg := &ssh.ClientConfig{User: a.User, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 15 * time.Second,
		Config: ssh.Config{KeyExchanges: kex, Ciphers: ciphers}}
	if a.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(a.PrivateKey))
		if err != nil {
			return "", fmt.Errorf("ssh key: %w", err)
		}
		cfg.Auth = append(cfg.Auth, ssh.PublicKeys(signer))
	}
	if a.Password != "" {
		pw := a.Password
		cfg.Auth = append(cfg.Auth, ssh.Password(pw), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range qs {
				ans[i] = pw
			}
			return ans, nil
		}))
	}
	if len(cfg.Auth) == 0 {
		return "", errors.New("credential has neither password nor private key")
	}
	port := a.Port
	if port <= 0 {
		port = 22
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return "", err
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	if rc.Exec {
		return fetchExec(client, rc)
	}
	return fetchShell(ctx, client, a, rc)
}

// Old gear speaks old algorithms; list the modern ones first, keep the legacy ones.
var (
	kex     = []string{"curve25519-sha256", "curve25519-sha256@libssh.org", "ecdh-sha2-nistp256", "ecdh-sha2-nistp384", "ecdh-sha2-nistp521", "diffie-hellman-group14-sha256", "diffie-hellman-group16-sha512", "diffie-hellman-group14-sha1", "diffie-hellman-group-exchange-sha256", "diffie-hellman-group-exchange-sha1", "diffie-hellman-group1-sha1"}
	ciphers = []string{"aes128-gcm@openssh.com", "aes256-gcm@openssh.com", "chacha20-poly1305@openssh.com", "aes128-ctr", "aes192-ctr", "aes256-ctr", "aes128-cbc", "3des-cbc"}
)

func fetchExec(client *ssh.Client, rc Recipe) (string, error) {
	var out bytes.Buffer
	for _, cmd := range append(rc.Setup, rc.Show) {
		s, err := client.NewSession()
		if err != nil {
			return "", err
		}
		b, err := s.CombinedOutput(cmd)
		s.Close()
		if err != nil && cmd == rc.Show {
			return "", fmt.Errorf("%s: %w (%s)", cmd, err, strings.TrimSpace(string(b)))
		}
		if cmd == rc.Show {
			out.Write(b)
		}
	}
	return out.String(), nil
}

// fetchShell drives an interactive session: request a pty, send the
// commands, answer pagers, and stop at the prompt (or after 3 s of silence).
func fetchShell(ctx context.Context, client *ssh.Client, a Auth, rc Recipe) (string, error) {
	s, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	if err := s.RequestPty("vt100", 200, 512, ssh.TerminalModes{ssh.ECHO: 0, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}); err != nil {
		return "", err
	}
	stdin, err := s.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := s.Shell(); err != nil {
		return "", err
	}
	lines := make(chan []byte, 256)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				lines <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	var all bytes.Buffer
	// read until the prompt shows up or the line goes quiet
	waitPrompt := func(quiet time.Duration, max time.Duration) {
		deadline := time.Now().Add(max)
		for {
			select {
			case chunk, ok := <-lines:
				if !ok {
					return
				}
				all.Write(chunk)
				tail := all.Bytes()
				if len(tail) > 512 {
					tail = tail[len(tail)-512:]
				}
				if bytes.Contains(tail, []byte("--More--")) || bytes.Contains(tail, []byte("--more--")) || bytes.Contains(tail, []byte("-- More --")) {
					stdin.Write([]byte(" "))
					continue
				}
				if rc.Prompt != nil && rc.Prompt.Match(lastLine(tail)) {
					return
				}
				if bytes.HasSuffix(bytes.TrimRight(tail, " "), []byte("assword:")) { // enable prompt
					return
				}
			case <-time.After(quiet):
				return
			case <-ctx.Done():
				return
			}
			if time.Now().After(deadline) {
				return
			}
		}
	}
	waitPrompt(1500*time.Millisecond, 5*time.Second) // banner + first prompt
	if rc.Enable != "" && a.EnablePassword != "" {
		tail := lastLine(all.Bytes())
		if bytes.HasSuffix(bytes.TrimSpace(tail), []byte(">")) {
			stdin.Write([]byte(rc.Enable + "\n"))
			waitPrompt(1500*time.Millisecond, 5*time.Second)
			stdin.Write([]byte(a.EnablePassword + "\n"))
			waitPrompt(1500*time.Millisecond, 5*time.Second)
		}
	}
	for _, c := range rc.Setup {
		stdin.Write([]byte(c + "\n"))
		waitPrompt(700*time.Millisecond, 4*time.Second)
	}
	all.Reset()
	for _, c := range strings.Split(rc.Show, "\n") {
		stdin.Write([]byte(c + "\n"))
		waitPrompt(3*time.Second, 120*time.Second)
	}
	stdin.Write([]byte("exit\n"))
	out := clean(all.String(), rc)
	if m := cliError.FindString(out); m != "" && len(out) < 400 {
		return "", fmt.Errorf("device answered %q — no privilege, wrong command for this platform, or a restricted user", strings.TrimSpace(m))
	}
	if len(strings.TrimSpace(out)) < 20 {
		return "", fmt.Errorf("empty output from %q — wrong recipe for this platform?", strings.Split(rc.Show, "\n")[0])
	}
	return out, nil
}

var cliError = regexp.MustCompile(`(?m)^\s*(% ?(Invalid input|Incomplete command|Ambiguous command|Bad secrets|Authorization failed)[^\n]*|Permission denied[^\n]*|Unknown action[^\n]*|command not found[^\n]*|ERROR: % Invalid[^\n]*)`)

func lastLine(b []byte) []byte {
	b = bytes.TrimRight(b, " \r\n")
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return b[i+1:]
	}
	return b
}

// clean strips pager artefacts, command echo and the trailing prompt.
func clean(s string, rc Recipe) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "")
	s = regexp.MustCompile(`--More--\x08*\s*|-- More --\x08*\s*|--more--\x08*\s*`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`).ReplaceAllString(s, "") // ANSI
	lines := strings.Split(s, "\n")
	// drop the echoed command(s) and the final prompt
	for len(lines) > 0 && (strings.TrimSpace(lines[0]) == "" || strings.Contains(lines[0], strings.Split(rc.Show, "\n")[0])) {
		lines = lines[1:]
	}
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" || (rc.Prompt != nil && rc.Prompt.MatchString(last)) || last == "exit" {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	return strings.Join(lines, "\n") + "\n"
}

// Normalise drops volatile lines so two identical configurations compare equal.
func Normalise(cfg string, rc Recipe) string {
	var out []string
	for _, l := range strings.Split(cfg, "\n") {
		skip := false
		for _, re := range rc.Ignore {
			if re.MatchString(l) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, strings.TrimRight(l, " \t"))
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n")) + "\n"
}
