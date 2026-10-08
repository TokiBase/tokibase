//go:build !no_printer

package printer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/devio"
	"github.com/tokibase/tokibase/internal/escpos"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/types"
)

// serialConn sets a deadline before every operation (a serial port has none
// of its own).
type serialConn struct {
	*devio.Serial
	timeout time.Duration
}

func (c *serialConn) Read(p []byte) (int, error) {
	_ = c.Serial.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Serial.Read(p)
}

func (c *serialConn) Write(p []byte) (int, error) {
	_ = c.Serial.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Serial.Write(p)
}

// openTransport opens the transport of p under the TOKI_PRINT_ALLOW_CIDRS
// policy. The address always comes from the superuser owned _printers row.
func (m *Module) openTransport(ctx context.Context, p *Printer) (io.ReadWriteCloser, error) {
	pol, err := policy(p.timeout())
	if err != nil {
		return nil, err
	}
	if p.Transport == "serial" {
		if err := pol.CheckFile(p.Address); err != nil {
			return nil, err
		}
		s, err := devio.OpenSerial(p.Address, devio.SerialConfig{Baud: p.Baud})
		if err != nil {
			return nil, err
		}
		return &serialConn{Serial: s, timeout: p.timeout()}, nil
	}
	return (&devio.Dialer{Policy: pol}).Open(ctx, p.target())
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// guardConn bounds every Write with a timer: a char device such as
// /dev/usb/lp0 has no write deadline and blocks while the printer is out of
// paper. On expiry the file is closed (which frees the caller; the stuck
// kernel write ends with the close or when the printer wakes up).
type guardConn struct {
	io.ReadWriteCloser
	timeout time.Duration
}

func (g *guardConn) Write(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := g.ReadWriteCloser.Write(p)
		ch <- result{n, err}
	}()
	// a large payload may take longer to drain than a small one
	t := time.NewTimer(g.timeout + time.Duration(len(p))*time.Millisecond/4)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-t.C:
		_ = g.ReadWriteCloser.Close()
		return 0, fmt.Errorf("write: %w", os.ErrDeadlineExceeded)
	}
}

// queryStatus sends DLE EOT 1..4 and decodes the answer within timeout. ok is
// false when the printer does not answer (many cheap models ignore real-time
// commands): the caller prints anyway. A broken connection is an error.
func queryStatus(c io.ReadWriter, timeout time.Duration) (st escpos.Status, ok bool, err error) {
	if _, err := c.Write(escpos.StatusQuery()); err != nil {
		return st, false, fmt.Errorf("status query: %w", err)
	}
	type reply struct {
		buf []byte
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		buf := make([]byte, 4)
		_, err := io.ReadFull(c, buf)
		ch <- reply{buf, err}
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	var r reply
	select {
	case r = <-ch:
	case <-t.C:
		return st, false, nil // the reader ends with the connection
	}
	if r.err != nil {
		if isTimeout(r.err) {
			return st, false, nil
		}
		return st, false, fmt.Errorf("status reply: %w", r.err)
	}
	st, perr := escpos.ParseStatus(r.buf)
	if perr != nil {
		return st, false, nil
	}
	return st, true, nil
}

func describe(s escpos.Status) string {
	var r []string
	add := func(c bool, t string) {
		if c {
			r = append(r, t)
		}
	}
	add(s.PaperEnd || s.PaperEndStop, "paper out")
	add(s.CoverOpen, "cover open")
	add(s.MechError, "mechanical error")
	add(s.CutterError, "cutter error")
	add(s.Unrecovered, "unrecoverable error")
	add(s.ErrorStop, "stopped by an error")
	add(s.Offline, "offline")
	if len(r) == 0 {
		return "ready"
	}
	return strings.Join(r, ", ")
}

// Describe formats a decoded status for people.
func Describe(s escpos.Status) string { return describe(s) }

type outcome int

const (
	outDone outcome = iota
	outWait
	// outUnconfirmed: everything was written, then the printer reported a
	// fault. The job must not be resent.
	outUnconfirmed
)

func (m *Module) statusSkipped(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.noStatus[name]
	return ok && m.Now().Before(until)
}

func (m *Module) rememberNoStatus(name string, answered bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if answered {
		delete(m.noStatus, name)
	} else {
		m.noStatus[name] = m.Now().Add(noStatusTTL)
	}
}

// transmit writes copies of payload to the printer. It returns outWait with a
// reason when the printer is out of paper (nothing was written), outUnconfirmed
// when the data went out and the printer reported a fault afterwards (never
// resent: ESC/POS printers keep the buffer and finish it when the fault is
// fixed), and an error for everything the job queue retries.
func (m *Module) transmit(ctx context.Context, p *Printer, payload []byte, copies int) (outcome, string, error) {
	c, err := m.Open(ctx, p)
	if err != nil {
		m.setStatus(p.Name, "offline", err.Error())
		return outDone, "", err
	}
	defer c.Close()
	var conn io.ReadWriter = c
	if p.Transport == "file" {
		conn = &guardConn{ReadWriteCloser: c, timeout: p.timeout()}
	}
	hasStatus := p.Transport != "file" && !m.statusSkipped(p.Name)

	if hasStatus {
		st, ok, err := queryStatus(conn, p.statusTimeout())
		if err != nil {
			m.setStatus(p.Name, "offline", err.Error())
			return outDone, "", err
		}
		m.rememberNoStatus(p.Name, ok)
		if !ok {
			hasStatus = false // prints anyway, and is not asked again for a while
		} else {
			switch {
			case st.PaperEnd || st.PaperEndStop:
				m.setStatus(p.Name, "paper", describe(st))
				return outWait, describe(st), nil
			case !st.Ready():
				m.setStatus(p.Name, "offline", describe(st))
				return outDone, "", fmt.Errorf("printer not ready: %s", describe(st))
			}
		}
	}
	for i := 0; i < copies; i++ {
		if err := writeAll(conn, payload); err != nil {
			m.setStatus(p.Name, "offline", err.Error())
			return outDone, "", fmt.Errorf("write: %w", err)
		}
	}
	if hasStatus {
		st, ok, err := queryStatus(conn, p.statusTimeout())
		if err != nil {
			// the connection broke after the write: the print may or may not
			// have happened; retry (a duplicate is possible, see the docs)
			m.setStatus(p.Name, "offline", err.Error())
			return outDone, "", err
		}
		if ok && (st.PaperEndStop || st.ErrorStop || st.MechError || st.CutterError || st.Unrecovered || st.CoverOpen) {
			m.setStatus(p.Name, "paper", describe(st))
			return outUnconfirmed, describe(st), nil
		}
		if ok {
			m.setStatus(p.Name, "ok", "")
			return outDone, "", nil
		}
		m.rememberNoStatus(p.Name, false)
	}
	m.setStatus(p.Name, "ok", "")
	return outDone, "", nil
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// handle is the `print.send` job handler. It is idempotent per job: a job that
// is already done or dead does nothing.
func (m *Module) handle(ctx context.Context, _ kernel.App, job *kernel.Job) error {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(job.Payload, &in); err != nil || in.ID == "" {
		return nil // nothing to retry
	}
	rec, err := m.app.FindRecordById(JobsCollection, in.ID)
	if err != nil {
		return nil // pruned or deleted
	}
	if s := rec.GetString("state"); s == StateDone || s == StateDead {
		return nil
	}
	final := job.Attempt >= job.MaxAttempts
	prn, err := findPrinter(m.app, rec.GetString("printer"))
	if err == nil && !prn.Enabled {
		err = fmt.Errorf("printer %q is disabled", prn.Name)
	}
	if err != nil {
		return m.fail(rec, err, final)
	}

	// one job at a time per printer, whatever the number of workers
	release, err := m.acquire(ctx, prn.Name)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return m.fail(rec, err, final) // the queue retries; the worker is free again
	}
	defer release()
	if rec, err = m.app.FindRecordById(JobsCollection, in.ID); err != nil {
		return nil
	}
	if s := rec.GetString("state"); s == StateDone || s == StateDead {
		return nil
	}
	payload, err := base64.StdEncoding.DecodeString(rec.GetString("payload"))
	if err != nil {
		return m.fail(rec, fmt.Errorf("payload: %w", err), true)
	}
	rec.Set("state", StatePrinting)
	rec.Set("attempts", rec.GetInt("attempts")+1)
	if err := m.app.Save(rec); err != nil {
		return err
	}

	res, reason, terr := m.transmit(ctx, prn, payload, max(1, rec.GetInt("copies")))
	if terr != nil {
		if ctx.Err() != nil {
			return ctx.Err() // shutdown, the queue gives the attempt back
		}
		return m.fail(rec, terr, final)
	}
	switch res {
	case outWait:
		return m.wait(ctx, rec, reason)
	case outUnconfirmed:
		msg := "printed, but the printer reported: " + reason
		rec.Set("state", StateUnconfirmed)
		rec.Set("last_error", msg)
		rec.Set("printed_at", types.NowDateTime())
		m.app.Logger().Warn("printer: print not confirmed, not resent", "job", rec.Id, "printer", prn.Name, "reason", reason)
		audit(AuditUnconfirmed, rec.Id, map[string]any{"printer": prn.Name, "reason": reason})
		return m.app.Save(rec)
	}
	rec.Set("state", StateDone)
	rec.Set("last_error", "")
	rec.Set("printed_at", types.NowDateTime())
	return m.app.Save(rec)
}

// fail records an error. The job queue retries with its backoff; the last
// attempt dead-letters the print.
func (m *Module) fail(rec *core.Record, cause error, final bool) error {
	msg := cause.Error()
	if len(msg) > 1900 {
		msg = msg[:1900]
	}
	rec.Set("last_error", msg)
	m.app.Logger().Warn("printer: transmission failed", "job", rec.Id, "printer", rec.GetString("printer"), "final", final, "error", msg)
	if final {
		rec.Set("state", StateDead)
	} else {
		rec.Set("state", StateFailed)
	}
	if err := m.app.Save(rec); err != nil {
		m.app.Logger().Warn("printer: failed to record an error", "job", rec.Id, "error", err)
	}
	if final {
		audit(AuditDead, rec.Id, map[string]any{
			"printer": rec.GetString("printer"), "attempts": rec.GetInt("attempts"), "error": msg,
		})
	}
	return cause
}

// wait parks the print until paper is loaded: it does not use the retry
// backoff or burn an attempt but queues a fresh job that runs after WaitDelay.
// The key carries a counter: the running job still holds its own key, and a
// second Enqueue with it would only return the running job.
func (m *Module) wait(ctx context.Context, rec *core.Record, reason string) error {
	n := rec.GetInt("waits") + 1
	if limit := m.MaxWaits; (limit > 0 && n > limit) || (limit <= 0 && n > MaxWaits) {
		rec.Set("state", StateDead)
		rec.Set("last_error", "printer stuck: waited too long for the printer ("+reason+")")
		audit(AuditStuck, rec.Id, map[string]any{"printer": rec.GetString("printer"), "waits": n - 1, "reason": reason})
		return m.app.Save(rec)
	}
	rec.Set("state", StateWaiting)
	rec.Set("waits", n)
	rec.Set("last_error", reason)
	if err := m.app.Save(rec); err != nil {
		return err
	}
	_, err := kernel.Jobs(m.app).Enqueue(ctx, JobKind, map[string]string{"id": rec.Id},
		kernel.Delay(m.WaitDelay), kernel.Unique(fmt.Sprintf("print:%s:w%d", rec.Id, n)), kernel.MaxAttempts(MaxAttempts))
	return err
}
