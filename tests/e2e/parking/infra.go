//go:build !no_sync

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	adminEmail = "admin@example.com"
	adminPass  = "adminpass1234"
)

// freePort returns a free loopback port (closed again: a small race, fine here).
func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// simClock is the shared fake clock: a file with a Go duration that every
// process adds to its wall clock (TOKI_SYNC_TEST_CLOCK_FILE).
type simClock struct {
	file string
	mu   sync.Mutex
	off  time.Duration
}

func (c *simClock) set(d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off = d
	tmp := c.file + ".tmp"
	if err := os.WriteFile(tmp, []byte(d.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.file)
}

func (c *simClock) advance(d time.Duration) error {
	c.mu.Lock()
	off := c.off + d
	c.mu.Unlock()
	return c.set(off)
}

func (c *simClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

// date is the PocketBase date format of the simulated now.
func (c *simClock) date() string { return c.now().UTC().Format("2006-01-02 15:04:05.000Z") }

// childEnv is the environment of a child process: the parent's without any
// TOKI_ variable (the embedded phone sets some in this process) plus extra.
func childEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TOKI_") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// proc is a child process killed through its own handle (never pkill).
type proc struct {
	name, bin string
	args      []string
	env       []string
	logPath   string
	pidPath   string
	cmd       *exec.Cmd
}

func (p *proc) start() error {
	lf, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(p.bin, p.args...)
	cmd.Env = p.env
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		lf.Close()
		return err
	}
	p.cmd = cmd
	_ = os.WriteFile(p.pidPath, []byte(fmt.Sprint(cmd.Process.Pid)), 0o644)
	go func() { _ = cmd.Wait(); lf.Close() }()
	return nil
}

func (p *proc) kill() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		// give the wait goroutine a moment to reap it and release the files
		time.Sleep(200 * time.Millisecond)
	}
	p.cmd = nil
}

// runCLI runs a toki command to completion and returns its stdout.
func runCLI(bin string, env []string, dir string, args ...string) (string, error) {
	cmd := exec.Command(bin, append(args, "--dir", dir, "--dev=false")...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// proxyHandle controls one syncproxy process.
type proxyHandle struct {
	p       *proc
	listen  string // host:port the spoke uses
	control string
}

var ctl = &http.Client{Timeout: 10 * time.Second}

func (h *proxyHandle) set(on bool) error {
	path := "/off"
	if on {
		path = "/on"
	}
	r, err := ctl.Get("http://" + h.control + path)
	if err != nil {
		return err
	}
	r.Body.Close()
	return nil
}

func startProxy(name, target string) (*proxyHandle, error) {
	pp := &proc{name: name, bin: cfg.proxyBin, args: []string{"-target", target}, env: childEnv(),
		logPath: filepath.Join(cfg.work, name+".log"), pidPath: filepath.Join(cfg.work, name+".pid")}
	// the address line goes to stdout: read it from a pipe instead of the log file
	cmd := exec.Command(pp.bin, pp.args...)
	cmd.Env = pp.env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	lf, _ := os.OpenFile(pp.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pp.cmd = cmd
	_ = os.WriteFile(pp.pidPath, []byte(fmt.Sprint(cmd.Process.Pid)), 0o644)
	line := make([]byte, 0, 256)
	buf := make([]byte, 1)
	for len(line) < 255 {
		if _, err := stdout.Read(buf); err != nil {
			return nil, fmt.Errorf("syncproxy exited: %w", err)
		}
		if buf[0] == '\n' {
			break
		}
		line = append(line, buf[0])
	}
	go func() { _, _ = io.Copy(io.Discard, stdout); _ = cmd.Wait() }()
	var addr struct{ Listen, Control string }
	if err := json.Unmarshal(line, &addr); err != nil {
		return nil, fmt.Errorf("syncproxy line %q: %w", line, err)
	}
	return &proxyHandle{p: pp, listen: addr.Listen, control: addr.Control}, nil
}

// ---- tiny HTTP/JSON client ---------------------------------------------------

var hc = &http.Client{Timeout: 60 * time.Second}

func doJSON(method, url, token string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	res, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, nil
}

// api calls a node and decodes an object; a non-2xx answer is an error.
func api(method, url, token string, body any) (map[string]any, error) {
	st, b, err := doJSON(method, url, token, body)
	if err != nil {
		return nil, err
	}
	if st/100 != 2 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, url, st, strings.TrimSpace(string(b)))
	}
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out, nil
}

func superToken(base string) (string, error) {
	r, err := api("POST", base+"/api/collections/_superusers/auth-with-password", "", map[string]any{"identity": adminEmail, "password": adminPass})
	if err != nil {
		return "", err
	}
	t, _ := r["token"].(string)
	if t == "" {
		return "", fmt.Errorf("no token from %s", base)
	}
	return t, nil
}

func waitHealth(base string, d time.Duration) error {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if r, err := hc.Get(base + "/api/health"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become healthy", base)
}

// lastJSONLine returns the last line of s that starts with { (log lines may start with [).
func lastJSONLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "{") {
			return l
		}
	}
	return ""
}
