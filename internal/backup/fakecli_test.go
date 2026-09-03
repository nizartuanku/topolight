package backup

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeIOS is a tiny SSH server that behaves like a Cisco IOS box: user EXEC
// prompt, enable with password, pager on unless "terminal length 0", and a
// running-config that changes when cfgVersion changes.
type fakeIOS struct {
	ln         net.Listener
	cfgVersion int
	exec       bool // MikroTik-style: answer exec requests instead of a shell
}

func (f *fakeIOS) config() string {
	var b strings.Builder
	b.WriteString("Building configuration...\n\nCurrent configuration : 2048 bytes\n!\n! Last configuration change at " + time.Now().Format("15:04:05 MST Mon Jan 2 2006") + " by admin\n!\nversion 15.2\nhostname sw1\n!\n")
	for i := 1; i <= 48; i++ {
		fmt.Fprintf(&b, "interface GigabitEthernet1/0/%d\n switchport mode access\n switchport access vlan %d\n!\n", i, 10+i%3)
	}
	if f.cfgVersion > 0 {
		b.WriteString("interface Vlan99\n description added-in-v" + fmt.Sprint(f.cfgVersion) + "\n ip address 10.99.0.1 255.255.255.0\n!\n")
	}
	b.WriteString("line vty 0 4\n transport input ssh\n!\nend\n")
	return b.String()
}

func startFake(t *testing.T, exec bool) *fakeIOS {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "admin" && string(pw) == "cisco" {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIOS{ln: ln, exec: exec}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, cfg)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeIOS) serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			pty := false
			for req := range requests {
				switch req.Type {
				case "pty-req":
					pty = true
					req.Reply(true, nil)
				case "shell":
					req.Reply(true, nil)
					f.shell(ch, pty)
					ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return
				case "exec":
					req.Reply(true, nil)
					cmd := string(req.Payload[4:])
					if cmd == "/export" {
						io.WriteString(ch, "# sep/03/2026 03:20:00 by RouterOS 7.12\n# software id = ABCD-1234\n/interface bridge\nadd name=bridge1\n/ip address\nadd address=10.0.0.1/24 interface=bridge1\n")
					} else {
						io.WriteString(ch, "bad command\n")
					}
					ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return
				default:
					req.Reply(false, nil)
				}
			}
		}()
	}
}

func (f *fakeIOS) shell(ch ssh.Channel, pty bool) {
	w := func(s string) { io.WriteString(ch, strings.ReplaceAll(s, "\n", "\r\n")) }
	w("\nUser Access Verification\n\nsw1>")
	enabled, pager := false, true
	r := bufio.NewReader(ch)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		switch {
		case cmd == "enable":
			w("Password: ")
			pw, _ := r.ReadString('\n')
			if strings.TrimSpace(pw) == "secret" {
				enabled = true
				w("sw1#")
			} else {
				w("% Bad secrets\n\nsw1>")
			}
			continue
		case cmd == "terminal length 0":
			pager = false
		case cmd == "exit":
			return
		case cmd == "show running-config":
			if !enabled {
				w("      ^\n% Invalid input detected at '^' marker.\n\n")
				break
			}
			cfg := f.config()
			if pager {
				lines := strings.Split(cfg, "\n")
				for i := 0; i < len(lines); i += 24 {
					end := i + 24
					if end > len(lines) {
						end = len(lines)
					}
					w(strings.Join(lines[i:end], "\n") + "\n")
					if end < len(lines) {
						w(" --More-- ")
						b := make([]byte, 1)
						r.Read(b)
						w("\x08\x08\x08\x08\x08\x08\x08\x08\x08\x08          \x08\x08\x08\x08\x08\x08\x08\x08\x08\x08")
					}
				}
			} else {
				w(cfg)
			}
		}
		if enabled {
			w("sw1#")
		} else {
			w("sw1>")
		}
	}
}

func TestFetchIOSShell(t *testing.T) {
	f := startFake(t, false)
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	var p int
	fmt.Sscan(port, &p)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rc := RecipeFor("cisco-ios")
	cfg, err := Fetch(ctx, host, Auth{User: "admin", Password: "cisco", EnablePassword: "secret", Port: p}, rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "hostname sw1") || !strings.Contains(cfg, "interface GigabitEthernet1/0/48") || !strings.Contains(cfg, "end") {
		t.Fatalf("config incomplete:\n%s", cfg)
	}
	if strings.Contains(cfg, "More") || strings.Contains(cfg, "sw1#") || strings.Contains(cfg, "\x08") {
		t.Fatalf("artefacts left:\n%s", cfg)
	}
	n1 := Normalise(cfg, rc)
	// the same config a moment later differs only by the timestamp line
	cfg2, _ := Fetch(ctx, host, Auth{User: "admin", Password: "cisco", EnablePassword: "secret", Port: p}, rc)
	if Normalise(cfg2, rc) != n1 {
		t.Fatal("normalised configs differ")
	}
	// a real change is detected
	f.cfgVersion = 1
	cfg3, _ := Fetch(ctx, host, Auth{User: "admin", Password: "cisco", EnablePassword: "secret", Port: p}, rc)
	add, rem := Counts(Diff(strings.Split(cfg, "\n"), strings.Split(cfg3, "\n")))
	if add < 4 || rem > 1 {
		t.Fatalf("diff %d/%d", add, rem)
	}
	// wrong enable password: fails with a clear message
	if _, err := Fetch(ctx, host, Auth{User: "admin", Password: "cisco", EnablePassword: "nope", Port: p}, rc); err == nil {
		t.Fatal("expected failure without privilege")
	}
	// wrong password
	if _, err := Fetch(ctx, host, Auth{User: "admin", Password: "bad", Port: p}, rc); err == nil {
		t.Fatal("expected auth failure")
	}
}

func TestFetchExec(t *testing.T) {
	f := startFake(t, true)
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	var p int
	fmt.Sscan(port, &p)
	rc := RecipeFor("mikrotik")
	cfg, err := Fetch(context.Background(), host, Auth{User: "admin", Password: "cisco", Port: p}, rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "/ip address") {
		t.Fatalf("mikrotik: %q", cfg)
	}
	if strings.Contains(Normalise(cfg, rc), "by RouterOS") {
		t.Fatal("timestamp not ignored")
	}
}
