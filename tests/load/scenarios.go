package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Warmup, Dur, Soak time.Duration
	ReadConc          []int
	WriteConc         []int
	RTConns           []int
	BatchSizes        []int
	BatchConc         int
	SoakWorkers       int
	SoakThink         time.Duration
}

type Env struct {
	S      *Server
	A      *API
	Tokens []string
	DS     *Dataset
	Cfg    Config
	Rep    *Report
	Log    func(string, ...any)
}

func (e *Env) tok(w *Worker) string { return e.Tokens[w.Rnd.Intn(len(e.Tokens))] }

// ---- ops ----

func (e *Env) readOp() OpFunc {
	return func(ctx context.Context, w *Worker) {
		c := e.DS.Top[w.Rnd.Intn(len(e.DS.Top))]
		q := url.Values{}
		q.Set("page", fmt.Sprint(1+w.Rnd.Intn(10)))
		q.Set("perPage", "30")
		if c.Created {
			q.Set("sort", "-created")
			q.Set("filter", `created >= "2020-01-01 00:00:00"`)
		} else {
			q.Set("sort", "-id")
			q.Set("filter", `id != ""`)
		}
		if c.Expand != "" {
			q.Set("expand", c.Expand)
		}
		u := e.S.URL() + "/api/collections/" + c.Name + "/records?" + q.Encode()
		tok := e.tok(w)
		w.Do(ctx, c.Name, func() (int, error) { return w.Req(ctx, "GET", u, tok, nil) })
	}
}

func (e *Env) writeOp(su bool) OpFunc {
	return func(ctx context.Context, w *Worker) {
		tok := e.A.SU
		if !su {
			tok = e.tok(w)
		}
		body := itemBody(w.Rnd, "w-"+randStr(w.Rnd, 12))
		u := e.S.URL() + "/api/collections/" + scratch + "/records"
		w.Do(ctx, "create", func() (int, error) { return w.Req(ctx, "POST", u, tok, bytes.NewReader(body)) })
	}
}

// ---- case runner ----

func (e *Env) runCase(scn, variant string, params map[string]any, conc int, think time.Duration, op OpFunc) *Case {
	c := &Case{Scenario: scn, Variant: variant, Params: params, LoadAvg: loadavg()}
	c.FilesPre = e.S.Files()
	pre := e.S.Proc()
	c.RSSBefore = pre.RSSKB
	ctx, cancel := context.WithTimeout(context.Background(), e.Cfg.Warmup+e.Cfg.Dur)
	defer cancel()
	sctx, scancel := context.WithCancel(context.Background())
	series := e.S.SampleRSS(sctx, 2*time.Second)
	rec := NewRecorder()
	t0 := time.Now()
	RunLoad(ctx, rec, conc, e.Cfg.Warmup, think, op)
	secs := time.Since(t0).Seconds() - e.Cfg.Warmup.Seconds()
	scancel()
	post := e.S.Proc()
	sum := rec.Summarize(map[string]float64{"*": secs})
	c.Seconds = secs
	c.Total = sum["main"]["*"]
	c.Ops = sum["main"]
	delete(c.Ops, "*")
	c.RSSAfter = post.RSSKB
	c.RSSPeak = series.PeakKB
	if post.RSSKB > c.RSSPeak {
		c.RSSPeak = post.RSSKB
	}
	c.CPUPct = (post.CPUSec - pre.CPUSec) / (secs + e.Cfg.Warmup.Seconds()) * 100
	c.FilesPost = e.S.Files()
	e.flag(c)
	e.Rep.Cases = append(e.Rep.Cases, c)
	e.Log("%s/%s %v: %.0f req/s p50=%.1f p99=%.1f failed=%d rss %s->%sMB", scn, variant, params, c.Total.RPS, c.Total.P50ms, c.Total.P99ms, c.Total.Failed, mb(c.RSSBefore), mb(c.RSSAfter))
	return c
}

// flag applies the bug criteria from the brief: errors, p99 > 2 s at modest concurrency, RSS growth > 2x.
func (e *Env) flag(c *Case) {
	if c.Total.Failed > 0 {
		c.Verdicts = append(c.Verdicts, fmt.Sprintf("%d non-2xx/transport failures: %v", c.Total.Failed, c.Total.Status))
	}
	if conc, ok := c.Params["conc"].(int); ok && conc <= 50 && c.Total.P99ms > 2000 {
		c.Verdicts = append(c.Verdicts, fmt.Sprintf("p99 %.0f ms > 2 s at concurrency %d", c.Total.P99ms, conc))
	}
	if c.RSSBefore > 0 && c.RSSAfter > 2*c.RSSBefore {
		c.Verdicts = append(c.Verdicts, fmt.Sprintf("RSS grew %.1fx (%s -> %s MB)", float64(c.RSSAfter)/float64(c.RSSBefore), mb(c.RSSBefore), mb(c.RSSAfter)))
	}
}

func (e *Env) restart(v Variant) error {
	e.S.Stop()
	if err := e.S.Start(v); err != nil {
		return err
	}
	if err := e.A.LoginSU(); err != nil {
		return err
	}
	toks, err := e.A.Tokens()
	if err != nil {
		return err
	}
	e.Tokens = toks
	if v.Replica {
		time.Sleep(8 * time.Second) // initial snapshot of the copy
	}
	return nil
}

// ---- scenarios ----

func (e *Env) ReadList() error {
	if err := e.restart(Variant{Name: "default"}); err != nil {
		return err
	}
	for _, n := range e.Cfg.ReadConc {
		e.runCase("read-list", "default", map[string]any{"conc": n}, n, 0, e.readOp())
	}
	return nil
}

const hookCPU = `onRecordCreateRequest((e) => {
  const end = Date.now() + 50
  while (Date.now() < end) {}
  e.next()
}, "load_items")
`

const hookIO = `onRecordCreateRequest((e) => {
  $http.send({url: "http://127.0.0.1:8098/sleep50", method: "GET", timeout: 10})
  e.next()
}, "load_items")
`

func (e *Env) WriteCreate() error {
	type vt struct {
		v  Variant
		su bool
	}
	vts := []vt{
		{Variant{Name: "audit-off", Env: map[string]string{"TOKI_AUDIT": "off"}}, false},
		{Variant{Name: "audit-on(default)"}, false},
		{Variant{Name: "audit-off,superuser", Env: map[string]string{"TOKI_AUDIT": "off"}}, true},
		{Variant{Name: "audit-on,superuser"}, true},
		{Variant{Name: "walreplica-file", Replica: true}, false},
		{Variant{Name: "hook-50ms-cpu", Hook: hookCPU}, false},
		{Variant{Name: "hook-50ms-io", Hook: hookIO}, false},
	}
	stopSleep := startSleepServer()
	defer stopSleep()
	for _, x := range vts {
		if err := e.restart(x.v); err != nil {
			return err
		}
		for _, n := range e.Cfg.WriteConc {
			if err := e.A.ResetItems(); err != nil {
				return err
			}
			c := e.runCase("write-create", x.v.Name, map[string]any{"conc": n}, n, 0, e.writeOp(x.su))
			if cnt, err := e.A.Count(scratch); err == nil {
				c.Extra = map[string]any{"records_in_scratch_after": cnt, "writes_per_s": fmt.Sprintf("%.1f", c.Total.RPS)}
			}
		}
	}
	return nil
}

func (e *Env) Realtime() error {
	if err := e.restart(Variant{Name: "default"}); err != nil {
		return err
	}
	for _, n := range e.Cfg.RTConns {
		if err := e.A.ResetItems(); err != nil {
			return err
		}
		if err := e.realtimeCase(n); err != nil {
			return err
		}
	}
	return nil
}

func (e *Env) realtimeCase(n int) error {
	c := &Case{Scenario: "realtime", Variant: "default", Params: map[string]any{"conns": n}, LoadAvg: loadavg(), Extra: map[string]any{}}
	c.FilesPre = e.S.Files()
	pre := e.S.Proc()
	c.RSSBefore = pre.RSSKB
	sctx, scancel := context.WithCancel(context.Background())
	series := e.S.SampleRSS(sctx, time.Second)
	defer scancel()
	bag := &latBag{}
	cl := sseClient()
	defer cl.CloseIdleConnections()
	conns := make([]*sseConn, n)
	var fail atomic.Int64
	t0 := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 50)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			sc, err := connectSSE(context.Background(), cl, e.S.URL(), e.Tokens[i%len(e.Tokens)], func(_ *sseConn, k byte, _ int, d time.Duration) { bag.add(k, d) })
			if err != nil {
				fail.Add(1)
				return
			}
			conns[i] = sc
		}(i)
	}
	wg.Wait()
	live := 0
	for _, sc := range conns {
		if sc != nil {
			live++
		}
	}
	connectSecs := time.Since(t0).Seconds()
	time.Sleep(2 * time.Second)
	afterConn := e.S.Proc()
	c.Extra["connect_failures"] = fail.Load()
	c.Extra["connect_seconds"] = fmt.Sprintf("%.1f", connectSecs)
	c.Extra["rss_after_connect_mb"] = mb(afterConn.RSSKB)
	c.Extra["server_fds_after_connect"] = afterConn.FDs
	c.Extra["server_threads_after_connect"] = afterConn.Threads

	const nrec = 100
	wapi := &Worker{C: newClient(8), Rec: NewRecorder()}
	wapi.Rec.measureFrom = time.Time{}
	post := func(kind byte, seq int, tok string) {
		body := itemBody(wapi.rnd(), rtTitle(kind, seq))
		wapi.Do(context.Background(), "write-"+string(kind), func() (int, error) {
			return wapi.Req(context.Background(), "POST", e.S.URL()+"/api/collections/"+scratch+"/records", tok, bytes.NewReader(body))
		})
	}
	waitQuiet := func() {
		last, still := int64(-1), 0
		for i := 0; i < 60 && still < 4; i++ {
			time.Sleep(500 * time.Millisecond)
			var tot int64
			for _, sc := range conns {
				if sc != nil {
					tot += sc.counts[0].Load() + sc.counts[1].Load()
				}
			}
			if tot == last {
				still++
			} else {
				still = 0
			}
			last = tot
		}
	}
	// burst: 4 writers, 100 records
	var seq atomic.Int64
	var ww sync.WaitGroup
	tb := time.Now()
	for g := 0; g < 4; g++ {
		ww.Add(1)
		go func() {
			defer ww.Done()
			for {
				s := int(seq.Add(1))
				if s > nrec {
					return
				}
				post('b', s, e.A.SU)
			}
		}()
	}
	ww.Wait()
	burstWrite := time.Since(tb).Seconds()
	waitQuiet()
	c.Extra["burst_write_seconds"] = fmt.Sprintf("%.2f", burstWrite)
	// paced: 20 records/s
	tk := time.NewTicker(50 * time.Millisecond)
	for s := 1; s <= nrec; s++ {
		<-tk.C
		go post('p', s, e.A.SU)
	}
	tk.Stop()
	time.Sleep(time.Second)
	waitQuiet()
	afterWrites := e.S.Proc()

	for _, k := range []struct {
		name string
		idx  int
		kind byte
	}{{"burst", 0, 'b'}, {"paced20ps", 1, 'p'}} {
		exp := int64(live) * nrec
		var got, short, dup, dead int64
		for _, sc := range conns {
			if sc == nil {
				continue
			}
			v := sc.counts[k.idx].Load()
			got += v
			if v < nrec {
				short++
			}
			if v > nrec {
				dup += v - nrec
			}
		}
		for _, sc := range conns {
			if sc != nil && sc.dead.Load() {
				dead++
			}
		}
		bag.mu.Lock()
		lat := append([]int64(nil), bag.lat[k.idx]...)
		bag.mu.Unlock()
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		st := Stats{Count: int64(len(lat)), P50ms: pct(lat, .5), P95ms: pct(lat, .95), P99ms: pct(lat, .99), Status: map[string]int64{}}
		if len(lat) > 0 {
			st.MaxMs = float64(lat[len(lat)-1]) / 1000
		}
		st.RPS = 0
		note := fmt.Sprintf("expected=%d received=%d dropped=%d clients_short=%d duplicates=%d dead_conns=%d", exp, got, exp-got, short, dup, dead)
		c.Phases = append(c.Phases, PhaseRow{Name: k.name + " fan-out latency", Stats: st, RSSKB: afterWrites.RSSKB, Files: e.S.Files(), Note: note})
		c.Extra["dropped_"+k.name] = exp - got
		if exp-got != 0 || dead != 0 {
			c.Verdicts = append(c.Verdicts, fmt.Sprintf("%s: %s", k.name, note))
		}
		if k.idx == 0 {
			c.Total = st
		}
	}
	wsum := wapi.Rec.Summarize(map[string]float64{"*": 1})
	c.Extra["write_latency_ms(p50/p99)"] = fmt.Sprintf("burst %.1f/%.1f paced %.1f/%.1f", wsum["main"]["write-b"].P50ms, wsum["main"]["write-b"].P99ms, wsum["main"]["write-p"].P50ms, wsum["main"]["write-p"].P99ms)
	c.Extra["write_failed"] = fmt.Sprintf("burst %d paced %d", wsum["main"]["write-b"].Failed, wsum["main"]["write-p"].Failed)
	c.Extra["rss_after_writes_mb"] = mb(afterWrites.RSSKB)
	c.Extra["server_cpu_s_during_case"] = fmt.Sprintf("%.1f", afterWrites.CPUSec-pre.CPUSec)
	for _, sc := range conns {
		if sc != nil {
			sc.Close()
		}
	}
	time.Sleep(5 * time.Second)
	end := e.S.Proc()
	c.Extra["rss_after_close_mb"] = mb(end.RSSKB)
	c.Extra["server_fds_after_close"] = end.FDs
	c.RSSAfter = afterWrites.RSSKB
	c.RSSPeak = series.PeakKB
	c.FilesPost = e.S.Files()
	c.Seconds = time.Since(t0).Seconds()
	if c.RSSBefore > 0 && afterWrites.RSSKB > 2*c.RSSBefore {
		c.Notes = append(c.Notes, fmt.Sprintf("RSS %s -> %s MB with %d live connections (idle server baseline vs loaded)", mb(c.RSSBefore), mb(afterWrites.RSSKB), live))
	}
	e.Rep.Cases = append(e.Rep.Cases, c)
	e.Log("realtime %d conns: live=%d burst p50=%.1f p99=%.1f dropped=%v/%v rss %s->%sMB", n, live, c.Phases[0].Stats.P50ms, c.Phases[0].Stats.P99ms, c.Extra["dropped_burst"], c.Extra["dropped_paced20ps"], mb(c.RSSBefore), mb(afterWrites.RSSKB))
	return nil
}

func (w *Worker) rnd() *randT { return newRand() }

// ---- batch ----

func (e *Env) batchBody(r *randT, n int) []byte {
	reqs := make([]map[string]any, n)
	for i := range reqs {
		var m map[string]any
		json.Unmarshal(itemBody(r, "b-"+randStr(r, 10)), &m)
		reqs[i] = map[string]any{"method": "POST", "url": "/api/collections/" + scratch + "/records", "body": m}
	}
	b, _ := json.Marshal(map[string]any{"requests": reqs})
	return b
}

func (e *Env) Batch() error {
	if err := e.restart(Variant{Name: "default"}); err != nil {
		return err
	}
	type rule struct{ name, assert, post string }
	rules := []rule{{"no-rules", "", ""}, {"rule-assert-true", "true", ""}, {"rule-assert_post-true", "", "true"}}
	for _, size := range e.Cfg.BatchSizes {
		for _, rl := range rules {
			if err := e.A.ResetItems(); err != nil {
				return err
			}
			var ruleID string
			var notes []string
			if rl.assert != "" || rl.post != "" {
				var rr struct{ ID string }
				body := map[string]any{"name": "load-trivial", "enabled": true, "message": "load rule", "match": []map[string]string{{"collection": scratch, "method": "POST"}}}
				if rl.assert != "" {
					body["assert"] = rl.assert
				}
				if rl.post != "" {
					body["assert_post"] = rl.post
				}
				if _, err := e.A.Call("POST", "/api/collections/_batch_rules/records", e.A.SU, body, &rr); err != nil {
					notes = append(notes, "could not create rule: "+err.Error())
				}
				ruleID = rr.ID
			}
			// correctness probe: one batch, count rows
			before, _ := e.A.Count(scratch)
			probe := e.batchBody(newRand(), size)
			st, perr := e.A.Call("POST", "/api/batch", e.Tokens[0], json.RawMessage(probe), nil)
			after, _ := e.A.Count(scratch)
			notes = append(notes, fmt.Sprintf("probe batch status=%d rows %d->%d (expected +%d) err=%v", st, before, after, size, perr))

			op := func(ctx context.Context, w *Worker) {
				body := e.batchBody(w.Rnd, size)
				tok := e.tok(w)
				w.Do(ctx, fmt.Sprintf("batch%d", size), func() (int, error) { return w.Req(ctx, "POST", e.S.URL()+"/api/batch", tok, bytes.NewReader(body)) })
			}
			c := e.runCase("batch", rl.name, map[string]any{"subrequests": size, "conc": e.Cfg.BatchConc}, e.Cfg.BatchConc, 0, op)
			c.Notes = append(c.Notes, notes...)
			if after-before != int64(size) {
				c.Verdicts = append(c.Verdicts, fmt.Sprintf("probe batch committed %d rows, expected %d", after-before, size))
			}
			c.Extra = map[string]any{"records_per_s": fmt.Sprintf("%.0f", c.Total.RPS*float64(size))}
			if ruleID != "" {
				e.A.Call("DELETE", "/api/collections/_batch_rules/records/"+ruleID, e.A.SU, nil, nil)
			}
		}
	}
	// rejection check: a rule that must reject rolls the batch back
	var rr struct{ ID string }
	if _, err := e.A.Call("POST", "/api/collections/_batch_rules/records", e.A.SU, map[string]any{"name": "load-reject", "enabled": true, "message": "load reject", "assert": "false", "match": []map[string]string{{"collection": scratch, "method": "POST"}}}, &rr); err == nil {
		e.A.ResetItems()
		var out json.RawMessage
		st, _ := e.A.Call("POST", "/api/batch", e.Tokens[0], json.RawMessage(e.batchBody(newRand(), 5)), &out)
		n, _ := e.A.Count(scratch)
		last := e.Rep.Cases[len(e.Rep.Cases)-1]
		last.Notes = append(last.Notes, fmt.Sprintf("rejecting rule check: status=%d rows committed=%d (want 400 and 0) body=%s", st, n, trunc(string(out), 160)))
		if st != 400 || n != 0 {
			last.Verdicts = append(last.Verdicts, "batchguard rejecting rule did not reject/rollback")
		}
		e.A.Call("DELETE", "/api/collections/_batch_rules/records/"+rr.ID, e.A.SU, nil, nil)
	}
	return nil
}

// ---- backup under load ----

func (e *Env) Backup() error {
	if err := e.restart(Variant{Name: "default"}); err != nil {
		return err
	}
	c := &Case{Scenario: "backup-under-load", Variant: "default", Params: map[string]any{"conc": 200}, LoadAvg: loadavg(), Extra: map[string]any{}}
	c.FilesPre = e.S.Files()
	pre := e.S.Proc()
	c.RSSBefore = pre.RSSKB
	ctx, cancel := context.WithCancel(context.Background())
	sctx, scancel := context.WithCancel(context.Background())
	series := e.S.SampleRSS(sctx, time.Second)
	rec := NewRecorder()
	done := make(chan struct{})
	t0 := time.Now()
	go func() { RunLoad(ctx, rec, 200, e.Cfg.Warmup, 0, e.readOp()); close(done) }()
	time.Sleep(e.Cfg.Warmup)
	secs := map[string]float64{}
	phase := func(name string, f func() string) {
		rec.SetPhase(name)
		s := time.Now()
		note := f()
		secs[name] = time.Since(s).Seconds()
		c.Extra["seconds_"+name] = fmt.Sprintf("%.1f", secs[name])
		if note != "" {
			c.Extra["note_"+name] = note
		}
	}
	bdir := filepath.Join(e.S.DataDir(), "backups")
	run := func(args ...string) (string, error) {
		cmd := exec.Command(e.S.Bin, append(args, "--dir", e.S.DataDir())...)
		cmd.Env = baseEnv()
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	phase("1-baseline", func() string { time.Sleep(e.Cfg.Dur / 2); return "" })
	phase("2-cli-backup-create", func() string {
		out, err := run("backup", "create", "loadbk-cli.zip")
		fi, _ := os.Stat(filepath.Join(bdir, "loadbk-cli.zip"))
		var sz int64
		if fi != nil {
			sz = fi.Size()
		}
		return fmt.Sprintf("err=%v size=%s out=%s", err, fb(sz), trunc(out, 150))
	})
	phase("3-cli-backup-verify", func() string {
		out, err := run("backup", "verify", "latest", "--json")
		return fmt.Sprintf("err=%v ok=%v", err, strings.Contains(out, `"integrityOk": true`))
	})
	phase("4-api-backup", func() string {
		// long client timeout: measure how long the call really takes instead of cutting it at 120 s
		long := &API{Base: e.A.Base, C: &http.Client{Timeout: 15 * time.Minute}, SU: e.A.SU}
		code, err := long.Call("POST", "/api/backups", long.SU, map[string]string{"name": "loadbk-api.zip"}, nil)
		return fmt.Sprintf("status=%d err=%v", code, err)
	})
	phase("5-after", func() string { time.Sleep(e.Cfg.Dur / 3); return "" })
	cancel()
	<-done
	scancel()
	for _, n := range []string{"loadbk-cli.zip", "loadbk-api.zip"} {
		e.A.Call("DELETE", "/api/backups/"+n, e.A.SU, nil, nil)
		os.Remove(filepath.Join(bdir, n))
	}
	sum := rec.Summarize(secs)
	names := make([]string, 0, len(secs))
	for k := range secs {
		names = append(names, k)
	}
	sort.Strings(names)
	var base Stats
	for _, n := range names {
		st := sum[n]["*"]
		if n == "1-baseline" {
			base = st
		}
		note := ""
		if base.P99ms > 0 && n != "1-baseline" {
			note = fmt.Sprintf("p99 x%.1f vs baseline", st.P99ms/base.P99ms)
		}
		c.Phases = append(c.Phases, PhaseRow{Name: n, Stats: st, Note: note})
	}
	c.Total = base
	post := e.S.Proc()
	c.RSSAfter, c.RSSPeak = post.RSSKB, series.PeakKB
	c.Seconds = time.Since(t0).Seconds()
	c.FilesPost = e.S.Files()
	e.flag(c)
	e.Rep.Cases = append(e.Rep.Cases, c)
	e.Log("backup-under-load done")
	return nil
}

// ---- mixed soak ----

func (e *Env) Soak() error {
	if err := e.restart(Variant{Name: "default"}); err != nil {
		return err
	}
	if err := e.A.ResetItems(); err != nil {
		return err
	}
	minutes := int(e.Cfg.Soak / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	c := &Case{Scenario: "mixed-soak", Variant: "default", Params: map[string]any{"workers": e.Cfg.SoakWorkers, "think_avg": e.Cfg.SoakThink.String(), "mix": "70r/25w/5rt"}, LoadAvg: loadavg(), Extra: map[string]any{}}
	c.FilesPre = e.S.Files()
	pre := e.S.Proc()
	c.RSSBefore = pre.RSSKB
	ctx, cancel := context.WithCancel(context.Background())
	sctx, scancel := context.WithCancel(context.Background())
	series := e.S.SampleRSS(sctx, 5*time.Second)
	rec := NewRecorder()
	rec.SetPhase("warmup")
	var wseq atomic.Int64
	scl := sseClient()
	read, write := e.readOp(), e.writeOp(false)
	op := func(ctx context.Context, w *Worker) {
		x := w.Rnd.Intn(100)
		switch {
		case x < 70:
			read(ctx, w)
		case x < 95:
			// soak writes carry an rts title so that realtime sessions can time the event
			tok := e.tok(w)
			body := itemBody(w.Rnd, rtTitle('s', int(wseq.Add(1))))
			w.Do(ctx, "create", func() (int, error) {
				return w.Req(ctx, "POST", e.S.URL()+"/api/collections/"+scratch+"/records", tok, bytes.NewReader(body))
			})
			_ = write
		default:
			got := make(chan time.Duration, 1)
			t := time.Now()
			sc, err := connectSSE(ctx, scl, e.S.URL(), e.tok(w), func(_ *sseConn, k byte, _ int, d time.Duration) {
				if k == 's' {
					select {
					case got <- d:
					default:
					}
				}
			})
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				w.Rec.Add("rt-connect", t, time.Since(t), 0, err)
				return
			}
			w.Rec.Add("rt-connect", t, time.Since(t), 200, nil)
			select {
			case d := <-got:
				w.Rec.Add("rt-event", t, d, 200, nil)
			case <-time.After(3 * time.Second):
				w.Rec.Add("rt-event", t, 3*time.Second, -1, fmt.Errorf("no event within 3s"))
			case <-ctx.Done():
			}
			sc.Close()
		}
	}
	done := make(chan struct{})
	go func() { RunLoad(ctx, rec, e.Cfg.SoakWorkers, e.Cfg.Warmup, e.Cfg.SoakThink, op); close(done) }()
	time.Sleep(e.Cfg.Warmup)
	secs := map[string]float64{}
	type snap struct {
		name  string
		rss   int64
		files map[string]int64
	}
	var snaps []snap
	for m := 1; m <= minutes; m++ {
		name := fmt.Sprintf("min%02d", m)
		rec.SetPhase(name)
		time.Sleep(time.Minute)
		secs[name] = 60
		snaps = append(snaps, snap{name, e.S.Proc().RSSKB, e.S.Files()})
		e.Log("soak %s: rss %sMB", name, mb(snaps[len(snaps)-1].rss))
	}
	cancel()
	<-done
	scancel()
	sum := rec.Summarize(secs)
	var p99s []float64
	for _, s := range snaps {
		st := sum[s.name]["*"]
		c.Phases = append(c.Phases, PhaseRow{Name: s.name, Stats: st, RSSKB: s.rss, Files: s.files})
		p99s = append(p99s, st.P99ms)
	}
	var tot Stats
	var all []int64
	allStatus := map[string]int64{}
	_ = all
	for _, s := range snaps {
		for k, v := range sum[s.name]["*"].Status {
			allStatus[k] += v
		}
		tot.Count += sum[s.name]["*"].Count
	}
	tot.Status = allStatus
	tot.RPS = float64(tot.Count) / (60 * float64(minutes))
	for k := range allStatus {
		if !strings.HasPrefix(k, "2") {
			tot.Failed += allStatus[k]
		}
	}
	// p50/p99 over the whole run: median of the per-minute values (approximation, exact merge not kept)
	if len(p99s) > 0 {
		tot.P99ms = median(p99s)
		var p50s, p95s []float64
		for _, s := range snaps {
			p50s = append(p50s, sum[s.name]["*"].P50ms)
			p95s = append(p95s, sum[s.name]["*"].P95ms)
			if m := sum[s.name]["*"].MaxMs; m > tot.MaxMs {
				tot.MaxMs = m
			}
		}
		tot.P50ms, tot.P95ms = median(p50s), median(p95s)
	}
	c.Total = tot
	c.Ops = map[string]Stats{}
	for _, op := range []string{"create", "rt-connect", "rt-event"} {
		var lats Stats
		for _, s := range snaps {
			if o, ok := sum[s.name][op]; ok {
				lats.Count += o.Count
				lats.P99ms = max64(lats.P99ms, o.P99ms)
				lats.P50ms = max64(lats.P50ms, o.P50ms)
				if lats.Status == nil {
					lats.Status = map[string]int64{}
				}
				for k, v := range o.Status {
					lats.Status[k] += v
				}
			}
		}
		if lats.Count > 0 {
			lats.RPS = float64(lats.Count) / (60 * float64(minutes))
			c.Ops[op] = lats
		}
	}
	for _, s := range snaps[:1] {
		_ = s
	}
	post := e.S.Proc()
	c.RSSAfter, c.RSSPeak = post.RSSKB, series.PeakKB
	c.CPUPct = (post.CPUSec - pre.CPUSec) / (e.Cfg.Soak.Seconds() + e.Cfg.Warmup.Seconds()) * 100
	c.Seconds = e.Cfg.Soak.Seconds()
	c.FilesPost = e.S.Files()
	if len(p99s) >= 6 {
		first, last := avg(p99s[:3]), avg(p99s[len(p99s)-3:])
		c.Extra["p99_first3min_ms"] = fmt.Sprintf("%.1f", first)
		c.Extra["p99_last3min_ms"] = fmt.Sprintf("%.1f", last)
		c.Extra["p99_drift"] = fmt.Sprintf("x%.2f", last/maxf(first, 0.001))
		if last > 2*first && last > 50 {
			c.Verdicts = append(c.Verdicts, fmt.Sprintf("p99 drift x%.2f (%.1f -> %.1f ms)", last/first, first, last))
		}
		r1, r2 := snaps[0].rss, snaps[len(snaps)-1].rss
		c.Extra["rss_min01_mb"], c.Extra["rss_last_mb"] = mb(r1), mb(r2)
	}
	cnt, _ := e.A.Count(scratch)
	c.Extra["scratch_records_at_end"] = cnt
	e.flag(c)
	e.Rep.Cases = append(e.Rep.Cases, c)
	return nil
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}
func avg(v []float64) float64 {
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}
func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func maxf(a, b float64) float64 { return max64(a, b) }
