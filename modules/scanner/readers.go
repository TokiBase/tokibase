//go:build !no_scanner

package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/devio"
)

type closer = io.ReadCloser

// openDevice opens the device of a serial or evdev scanner.
func openDevice(sc *Scanner) (closer, error) {
	// only character devices: /dev/zero and friends would spin a core
	if fi, err := os.Stat(sc.Device); err != nil {
		return nil, err
	} else if fi.Mode()&os.ModeCharDevice == 0 {
		return nil, errors.New("scanner: " + sc.Device + " is not a character device")
	}
	switch sc.Kind {
	case KindSerial:
		return devio.OpenSerial(sc.Device, devio.SerialConfig{Baud: sc.Baud})
	case KindEvdev:
		return devio.OpenEvdev(sc.Device, sc.Grab)
	}
	return nil, errors.New("scanner: no device for kind " + sc.Kind)
}

// Reconnect backoff of a reader whose device vanished (USB unplug).
var (
	backoffMin = 500 * time.Millisecond
	backoffMax = 15 * time.Second
)

// reader runs one enabled serial or evdev scanner.
type reader struct {
	sc     *Scanner
	sig    string
	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	state      string // connecting, connected, error
	lastErr    string
	lastScan   time.Time
	scans      int64
	rejected   int64
	reconnects int64
}

// ReaderStatus is the public state of a scanner.
type ReaderStatus struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Device     string `json:"device,omitempty"`
	Enabled    bool   `json:"enabled"`
	State      string `json:"state"`
	LastError  string `json:"last_error,omitempty"`
	LastScan   string `json:"last_scan,omitempty"`
	Scans      int64  `json:"scans"`
	Rejected   int64  `json:"rejected"`
	Reconnects int64  `json:"reconnects"`
}

func (r *reader) set(state, errMsg string) {
	r.mu.Lock()
	r.state, r.lastErr = state, errMsg
	r.mu.Unlock()
}

func (r *reader) status() ReaderStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := ReaderStatus{
		Name: r.sc.Name, Kind: r.sc.Kind, Device: r.sc.Device, Enabled: true, State: r.state,
		LastError: r.lastErr, Scans: r.scans, Rejected: r.rejected, Reconnects: r.reconnects,
	}
	if !r.lastScan.IsZero() {
		s.LastScan = fmtTime(r.lastScan)
	}
	return s
}

func sigOf(sc *Scanner) string {
	b, _ := json.Marshal(sc)
	return string(b)
}

// wakeSupervisor asks the supervisor to re-read _scanners.
func (m *Module) wakeSupervisor() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Start launches the supervisor (readers, retention). Called on serve.
func (m *Module) Start() {
	m.supMu.Lock()
	if m.started {
		m.supMu.Unlock()
		return
	}
	m.started = true
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.supMu.Unlock()

	_ = m.app.Cron().Add("__tokiScanPrune__", pruneCron, func() { _, _ = m.Prune() })
	apis.SetHealthExtra(m.app, "scanner", func(core.App) any { return m.Status() })

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.reconcile(ctx)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
			case <-t.C:
			}
			m.reconcile(ctx)
		}
	}()
}

// Stop stops all readers and waits for them.
func (m *Module) Stop() {
	m.supMu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.started = false
	m.supMu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
	m.supMu.Lock()
	rs := m.running
	m.running = map[string]*reader{}
	m.supMu.Unlock()
	for _, r := range rs {
		r.cancel()
		<-r.done
	}
	m.app.Cron().Remove("__tokiScanPrune__")
}

// reconcile starts, restarts and stops readers to match the enabled
// serial/evdev rows of _scanners.
func (m *Module) reconcile(ctx context.Context) {
	all, err := loadAll(m.app)
	if err != nil {
		m.app.Logger().Warn("scanner: failed to load _scanners", "error", err)
		return
	}
	want := map[string]*Scanner{}
	for _, sc := range all {
		if sc.Enabled && (sc.Kind == KindSerial || sc.Kind == KindEvdev) && sc.Validate() == nil {
			want[sc.Name] = sc
		}
	}
	m.supMu.Lock()
	defer m.supMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	for name, r := range m.running {
		if w, ok := want[name]; !ok || sigOf(w) != r.sig {
			r.cancel()
			<-r.done
			delete(m.running, name)
		}
	}
	for name, sc := range want {
		if _, ok := m.running[name]; ok {
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		r := &reader{sc: sc, sig: sigOf(sc), cancel: cancel, done: make(chan struct{}), state: "connecting"}
		m.running[name] = r
		go func() {
			defer close(r.done)
			m.runReader(rctx, r)
		}()
	}
}

// Status lists every configured scanner with its reader state.
func (m *Module) Status() []ReaderStatus {
	out := []ReaderStatus{}
	all, _ := loadAll(m.app)
	m.supMu.Lock()
	defer m.supMu.Unlock()
	for _, sc := range all {
		if r, ok := m.running[sc.Name]; ok {
			out = append(out, r.status())
			continue
		}
		st := "idle"
		if !sc.Enabled {
			st = "disabled"
		}
		if sc.Kind == KindWeb && sc.Enabled {
			st = "ready"
		}
		out = append(out, ReaderStatus{Name: sc.Name, Kind: sc.Kind, Device: sc.Device, Enabled: sc.Enabled, State: st})
	}
	return out
}

// runReader keeps a session open for the scanner, reconnecting with backoff.
func (m *Module) runReader(ctx context.Context, r *reader) {
	backoff := backoffMin
	for ctx.Err() == nil {
		r.set("connecting", r.lastErrLocked())
		rc, err := m.open(r.sc)
		if err == nil {
			r.set("connected", "")
			start := time.Now()
			err = m.session(ctx, r, rc)
			if time.Since(start) > 10*time.Second {
				backoff = backoffMin
			}
		}
		if ctx.Err() != nil {
			return
		}
		msg := "device closed"
		if err != nil {
			msg = err.Error()
		}
		r.mu.Lock()
		r.state, r.lastErr = "error", msg
		r.reconnects++
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

func (r *reader) lastErrLocked() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// session reads scans from rc until it fails or ctx ends; it closes rc.
func (m *Module) session(ctx context.Context, r *reader, rc closer) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			rc.Close()
		case <-stop:
		}
	}()
	defer rc.Close()
	return m.readScans(ctx, r.sc, rc, devio.EventSize, func(code string, rejected bool) {
		r.mu.Lock()
		if rejected {
			r.rejected++
		} else {
			r.scans++
			r.lastScan = m.now()
		}
		r.mu.Unlock()
	})
}

// readScans decodes the byte stream of a scanner (lines for serial, input_event
// structs of eventSize bytes for evdev) and ingests each scan. note, when set,
// is called per scan with rejected=true for a filtered one.
func (m *Module) readScans(ctx context.Context, sc *Scanner, src io.Reader, eventSize int, note func(code string, rejected bool)) error {
	maxLine := sc.MaxLen + len(sc.Prefix) + len(sc.Suffix) + 16
	if maxLine < 64 {
		maxLine = 64
	}
	var next func() (string, error)
	var tooLong error
	switch sc.Kind {
	case KindEvdev:
		ks, err := devio.NewKeyScanner(src, eventSize, maxLine)
		if err != nil {
			return err
		}
		next, tooLong = ks.Next, devio.ErrScanTooLong
	default:
		lr := devio.NewLineReader(src, maxLine)
		next, tooLong = lr.Next, devio.ErrLineTooLong
	}
	for {
		raw, err := next()
		if errors.Is(err, tooLong) {
			if note != nil {
				note("", true)
			}
			continue
		}
		if err != nil {
			return err
		}
		_, ierr := m.Ingest(ctx, sc, raw, IngestOptions{Source: sc.Kind})
		var rj *RejectedError
		switch {
		case errors.As(ierr, &rj):
			m.app.Logger().Debug("scanner: scan rejected", "scanner", sc.Name, "reason", rj.Reason)
			if note != nil {
				note(raw, true)
			}
		case ierr != nil:
			m.app.Logger().Warn("scanner: failed to ingest a scan", "scanner", sc.Name, "error", ierr)
		default:
			if note != nil {
				note(raw, false)
			}
		}
	}
}
