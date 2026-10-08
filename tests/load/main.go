// Command load is the TokiBase load/soak harness. It starts a toki binary on a COPY of a
// pb_data directory (made by run.sh), prepares test users and a scratch collection, runs the
// selected scenarios and writes a JSON + markdown report. It is not part of CI.
//
//	go run ./tests/load -bin /root/tokibase-load/toki -work /root/tokibase-load -scenarios all
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type randT = rand.Rand

func newRand() *randT { return rand.New(rand.NewSource(time.Now().UnixNano())) }

func startSleepServer() func() {
	srv := &http.Server{Addr: "127.0.0.1:8098", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte("ok"))
	})}
	go srv.ListenAndServe()
	return func() { srv.Close() }
}

func ints(s string) []int {
	var o []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			o = append(o, n)
		}
	}
	return o
}

func main() {
	bin := flag.String("bin", "", "path to the toki binary")
	work := flag.String("work", "/root/tokibase-load", "work dir; <work>/pb_data must hold the data copy")
	addr := flag.String("addr", "127.0.0.1:8097", "listen address of the server under test")
	scn := flag.String("scenarios", "all", "comma list: read-list,write-create,realtime,batch,backup-under-load,mixed-soak or all")
	dur := flag.Duration("duration", 60*time.Second, "measured duration per case")
	warm := flag.Duration("warmup", 10*time.Second, "warm-up per case (not measured)")
	soak := flag.Duration("soak", 20*time.Minute, "mixed-soak duration (whole minutes)")
	readConc := flag.String("read-conc", "50,200,500", "read-list concurrency levels")
	writeConc := flag.String("write-conc", "1,8,32", "write-create concurrency levels")
	rtConns := flag.String("rt-conns", "200,500,1000", "realtime SSE connection counts")
	batchSizes := flag.String("batch-sizes", "50,500", "sub-request counts for batch")
	out := flag.String("out", "tests/load/results", "report directory")
	tag := flag.String("tag", "wpe", "report name suffix")
	quick := flag.Bool("quick", false, "smoke settings: 4 s cases, 1 min soak, small connection counts")
	flag.Parse()
	if *bin == "" {
		fmt.Fprintln(os.Stderr, "-bin required")
		os.Exit(2)
	}
	cfg := Config{Warmup: *warm, Dur: *dur, Soak: *soak, ReadConc: ints(*readConc), WriteConc: ints(*writeConc), RTConns: ints(*rtConns), BatchSizes: ints(*batchSizes), BatchConc: 4, SoakWorkers: 32, SoakThink: 50 * time.Millisecond}
	if *quick {
		cfg.Warmup, cfg.Dur, cfg.Soak = 2*time.Second, 4*time.Second, time.Minute
		cfg.RTConns = []int{20, 50}
		cfg.ReadConc = []int{10, 50}
		cfg.WriteConc = []int{1, 8}
	}
	raiseNoFile()
	logf := func(f string, a ...any) {
		fmt.Printf("%s "+f+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
	}

	s := &Server{Bin: *bin, Work: *work, Addr: *addr}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; logf("signal, stopping server"); s.Stop(); os.Exit(130) }()
	defer s.Stop()

	if err := ensureSuperuser(*bin, s.DataDir()); err != nil {
		fatal(err)
	}
	if err := s.Start(Variant{Name: "setup"}); err != nil {
		fatal(err)
	}
	a := NewAPI(s.URL())
	if err := a.LoginSU(); err != nil {
		fatal(err)
	}
	ds, toks, err := a.Setup(logf)
	if err != nil {
		fatal(err)
	}
	if len(ds.Top) == 0 {
		fatal(fmt.Errorf("no base collections found in the copy"))
	}
	logf("dataset: %d collections, %d records; top: %+v", ds.Collections, ds.TotalRecs, ds.Top)
	rep := &Report{Tag: *tag, Date: time.Now().UTC().Format("2006-01-02"), Host: hostInfo(*bin), Dataset: ds}
	rep.Host["data.db"] = fb(s.Files()["data.db"])
	rep.Host["auxiliary.db"] = fb(s.Files()["auxiliary.db"])
	rep.Host["toki"] = os.Getenv("TOKI_VERSION_INFO")
	rep.Host["load_generator"] = "same host, Go net/http"
	env := &Env{S: s, A: a, Tokens: toks, DS: ds, Cfg: cfg, Rep: rep, Log: logf}

	all := []struct {
		name string
		fn   func() error
	}{
		{"read-list", env.ReadList}, {"write-create", env.WriteCreate}, {"realtime", env.Realtime},
		{"batch", env.Batch}, {"backup-under-load", env.Backup}, {"mixed-soak", env.Soak},
	}
	sel := map[string]bool{}
	for _, n := range strings.Split(*scn, ",") {
		sel[strings.TrimSpace(n)] = true
	}
	os.MkdirAll(*out, 0o755)
	base := filepath.Join(*out, fmt.Sprintf("%s-%s", rep.Date, *tag))
	write := func() {
		b, _ := json.MarshalIndent(rep, "", " ")
		os.WriteFile(base+".json", b, 0o644)
		os.WriteFile(base+".md", []byte(rep.Markdown()), 0o644)
	}
	failed := false
	for _, sc := range all {
		if !sel["all"] && !sel[sc.name] {
			continue
		}
		logf("=== %s", sc.name)
		if err := sc.fn(); err != nil {
			logf("SCENARIO %s ERROR: %v", sc.name, err)
			rep.Cases = append(rep.Cases, &Case{Scenario: sc.name, Variant: "error", Params: map[string]any{}, Verdicts: []string{"scenario aborted: " + err.Error()}})
			failed = true
			s.Stop()
		}
		write()
	}
	write()
	logf("report: %s.md", base)
	if failed {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "FATAL:", err)
	os.Exit(1)
}

func raiseNoFile() {
	var r syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r) == nil {
		r.Cur = r.Max
		syscall.Setrlimit(syscall.RLIMIT_NOFILE, &r)
	}
}
