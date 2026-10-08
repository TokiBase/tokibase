package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Variant describes one server configuration.
type Variant struct {
	Name    string
	Env     map[string]string
	Hook    string // JS content for pb_hooks/load.pb.js ("" = none)
	Replica bool
}

type Server struct {
	Bin, Work, Addr string
	cmd             *exec.Cmd
	done            chan struct{}
	Variant         string
}

func (s *Server) DataDir() string { return filepath.Join(s.Work, "pb_data") }
func (s *Server) URL() string     { return "http://" + s.Addr }
func (s *Server) PID() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func baseEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TOKI_") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "TOKI_TLS_CHECK=off")
}

// Start launches the server for a variant and waits for /api/health.
func (s *Server) Start(v Variant) error {
	if s.cmd != nil {
		return fmt.Errorf("server already running")
	}
	hooks := filepath.Join(s.Work, "hooks")
	os.RemoveAll(hooks)
	os.MkdirAll(hooks, 0o755)
	if v.Hook != "" {
		if err := os.WriteFile(filepath.Join(hooks, "load.pb.js"), []byte(v.Hook), 0o644); err != nil {
			return err
		}
	}
	env := baseEnv()
	if v.Replica {
		rdir := filepath.Join(s.Work, "replica")
		os.RemoveAll(rdir)
		env = append(env, "TOKI_REPLICA_URL=file://"+rdir)
	}
	for k, val := range v.Env {
		env = append(env, k+"="+val)
	}
	logf, err := os.OpenFile(filepath.Join(s.Work, "serve-"+v.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(s.Bin, "serve", "--dir", s.DataDir(), "--hooksDir", hooks, "--http", s.Addr)
	cmd.Env = env
	cmd.Dir = s.Work
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd = cmd
	s.Variant = v.Name
	s.done = make(chan struct{})
	go func() { cmd.Wait(); logf.Close(); close(s.done) }()
	os.WriteFile(filepath.Join(s.Work, "server.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			s.cmd = nil
			return fmt.Errorf("server exited during start, see serve-%s.log", v.Name)
		default:
		}
		resp, err := http.Get(s.URL() + "/api/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("server not healthy after 120s")
}

// Stop sends SIGTERM and waits (SIGKILL after 90 s).
func (s *Server) Stop() {
	if s.cmd == nil {
		return
	}
	s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(90 * time.Second):
		s.cmd.Process.Kill()
		<-s.done
	}
	s.cmd = nil
	os.Remove(filepath.Join(s.Work, "server.pid"))
}

// ---- process / host stats ----

type ProcStats struct {
	RSSKB, HWMKB int64
	Threads, FDs int
	CPUSec       float64
}

func readProc(pid int) ProcStats {
	var p ProcStats
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "VmRSS:":
			p.RSSKB = n
		case "VmHWM:":
			p.HWMKB = n
		case "Threads:":
			p.Threads = int(n)
		}
	}
	if d, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
		p.FDs = len(d)
	}
	st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if i := strings.LastIndex(string(st), ")"); i > 0 {
		f := strings.Fields(string(st)[i+2:])
		if len(f) > 13 {
			u, _ := strconv.ParseFloat(f[11], 64)
			sy, _ := strconv.ParseFloat(f[12], 64)
			p.CPUSec = (u + sy) / 100
		}
	}
	return p
}

func (s *Server) Proc() ProcStats {
	if s.PID() == 0 {
		return ProcStats{}
	}
	return readProc(s.PID())
}

func (s *Server) Files() map[string]int64 {
	m := map[string]int64{}
	for _, n := range []string{"data.db", "data.db-wal", "auxiliary.db", "auxiliary.db-wal"} {
		if fi, err := os.Stat(filepath.Join(s.DataDir(), n)); err == nil {
			m[n] = fi.Size()
		}
	}
	var rs int64
	filepath.Walk(filepath.Join(s.Work, "replica"), func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rs += fi.Size()
		}
		return nil
	})
	if rs > 0 {
		m["replica_dir"] = rs
	}
	return m
}

func loadavg() string {
	b, _ := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(b))
	if len(f) >= 3 {
		return strings.Join(f[:3], " ")
	}
	return ""
}

// Sampler records server RSS every interval until ctx ends; returns peak and series.
type RSSSeries struct {
	PeakKB int64
	Series []int64
}

func (s *Server) SampleRSS(ctx context.Context, every time.Duration) *RSSSeries {
	r := &RSSSeries{}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				v := s.Proc().RSSKB
				if v > r.PeakKB {
					r.PeakKB = v
				}
				r.Series = append(r.Series, v)
			}
		}
	}()
	return r
}
