package main

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder collects latency samples after the warm-up, keyed by phase and op.
type Recorder struct {
	mu          sync.Mutex
	measureFrom time.Time
	phase       atomic.Value // string
	buckets     map[string]*bucket
	lastSample  time.Time
}

type bucket struct {
	lat    []int64 // microseconds
	status map[string]int64
	errs   map[string]int64
}

func NewRecorder() *Recorder {
	r := &Recorder{buckets: map[string]*bucket{}}
	r.phase.Store("main")
	return r
}

func (r *Recorder) SetPhase(p string) { r.phase.Store(p) }
func (r *Recorder) Phase() string     { return r.phase.Load().(string) }

// Add records one finished operation. status 0 = transport error; status < 0 = custom failure label.
func (r *Recorder) Add(op string, start time.Time, d time.Duration, status int, err error) {
	if start.Before(r.measureFrom) {
		return
	}
	key := r.Phase() + "\x00" + op
	r.mu.Lock()
	b := r.buckets[key]
	if b == nil {
		b = &bucket{status: map[string]int64{}, errs: map[string]int64{}}
		r.buckets[key] = b
	}
	b.lat = append(b.lat, d.Microseconds())
	b.status[itoa(status)]++
	if err != nil {
		msg := err.Error()
		if len(msg) > 90 {
			msg = msg[len(msg)-90:]
		}
		b.errs[msg]++
	}
	if t := start.Add(d); t.After(r.lastSample) {
		r.lastSample = t
	}
	r.mu.Unlock()
}

// Worker is one simulated client.
type Worker struct {
	ID  int
	Rnd *rand.Rand
	C   *http.Client
	Rec *Recorder
}

// Do times fn and records it under op.
func (w *Worker) Do(ctx context.Context, op string, fn func() (int, error)) {
	t0 := time.Now()
	st, err := fn()
	if ctx.Err() != nil {
		return
	}
	w.Rec.Add(op, t0, time.Since(t0), st, err)
}

// Req performs a request and drains the body; returns the status.
func (w *Worker) Req(ctx context.Context, method, url, token string, body io.Reader) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.C.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

type OpFunc func(ctx context.Context, w *Worker)

func newClient(conns int) *http.Client {
	return &http.Client{
		Timeout: 90 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        conns + 16,
			MaxIdleConnsPerHost: conns + 16,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// RunLoad starts conc workers and stops when ctx ends. Samples before measureFrom are discarded.
func RunLoad(ctx context.Context, rec *Recorder, conc int, warmup time.Duration, think time.Duration, op OpFunc) {
	cl := newClient(conc)
	defer cl.CloseIdleConnections()
	rec.measureFrom = time.Now().Add(warmup)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w := &Worker{ID: id, Rnd: rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)*7919)), C: cl, Rec: rec}
			for ctx.Err() == nil {
				op(ctx, w)
				if think > 0 {
					select {
					case <-ctx.Done():
					case <-time.After(time.Duration(w.Rnd.Int63n(int64(think) * 2))):
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

// ---- statistics ----

type Stats struct {
	Count  int64            `json:"count"`
	RPS    float64          `json:"rps"`
	P50ms  float64          `json:"p50_ms"`
	P95ms  float64          `json:"p95_ms"`
	P99ms  float64          `json:"p99_ms"`
	MaxMs  float64          `json:"max_ms"`
	Status map[string]int64 `json:"status"`
	Errs   map[string]int64 `json:"error_samples,omitempty"`
	Failed int64            `json:"failed"` // non-2xx
}

func pct(s []int64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	i := int(float64(len(s)-1) * p)
	return float64(s[i]) / 1000
}

func calc(lat []int64, status, errs map[string]int64, secs float64) Stats {
	s := append([]int64(nil), lat...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	st := Stats{Count: int64(len(s)), Status: status, Errs: errs}
	if secs > 0 {
		st.RPS = float64(len(s)) / secs
	}
	st.P50ms, st.P95ms, st.P99ms = pct(s, .5), pct(s, .95), pct(s, .99)
	if len(s) > 0 {
		st.MaxMs = float64(s[len(s)-1]) / 1000
	}
	for k, v := range status {
		if !strings.HasPrefix(k, "2") {
			st.Failed += v
		}
	}
	return st
}

// Summarize returns stats per phase: result[phase]["*"] is the merge of all ops, other keys are per op.
func (r *Recorder) Summarize(phaseSecs map[string]float64) map[string]map[string]Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	type agg struct {
		lat    []int64
		status map[string]int64
		errs   map[string]int64
	}
	merged := map[string]*agg{}
	out := map[string]map[string]Stats{}
	for key, b := range r.buckets {
		parts := strings.SplitN(key, "\x00", 2)
		ph, op := parts[0], parts[1]
		secs := phaseSecs[ph]
		if secs == 0 {
			secs = phaseSecs["*"]
		}
		if out[ph] == nil {
			out[ph] = map[string]Stats{}
			merged[ph] = &agg{status: map[string]int64{}, errs: map[string]int64{}}
		}
		out[ph][op] = calc(b.lat, b.status, b.errs, secs)
		m := merged[ph]
		m.lat = append(m.lat, b.lat...)
		for k, v := range b.status {
			m.status[k] += v
		}
		for k, v := range b.errs {
			m.errs[k] += v
		}
	}
	for ph, m := range merged {
		secs := phaseSecs[ph]
		if secs == 0 {
			secs = phaseSecs["*"]
		}
		out[ph]["*"] = calc(m.lat, m.status, m.errs, secs)
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }
