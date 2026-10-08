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

// queryStatus sends DLE EOT 1..4 and decodes the answer. ok is false when the
// printer does not answer (many cheap models ignore real-time commands): the
// caller prints anyway. A broken connection is an error.
func queryStatus(c io.ReadWriter) (st escpos.Status, ok bool, err error) {
	if _, err := c.Write(escpos.StatusQuery()); err != nil {
		return st, false, fmt.Errorf("status query: %w", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		if isTimeout(err) {
			return st, false, nil
		}
		return st, false, fmt.Errorf("status reply: %w", err)
	}
	st, perr := escpos.ParseStatus(buf)
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
)

// transmit writes copies of payload to the printer. It returns outWait with a
// reason when the printer needs a person (paper out, cover open); any other
// problem is an error that the job queue retries.
func (m *Module) transmit(ctx context.Context, p *Printer, payload []byte, copies int) (outcome, string, error) {
	c, err := m.Open(ctx, p)
	if err != nil {
		m.setStatus(p.Name, "offline", err.Error())
		return outDone, "", err
	}
	defer c.Close()
	hasStatus := p.Transport != "file"

	if hasStatus {
		st, ok, err := queryStatus(c)
		if err != nil {
			m.setStatus(p.Name, "offline", err.Error())
			return outDone, "", err
		}
		if ok {
			switch {
			case st.NeedsAttention():
				m.setStatus(p.Name, "paper", describe(st))
				return outWait, describe(st), nil
			case !st.Ready():
				m.setStatus(p.Name, "offline", describe(st))
				return outDone, "", fmt.Errorf("printer not ready: %s", describe(st))
			}
		}
	}
	for i := 0; i < copies; i++ {
		if err := writeAll(c, payload); err != nil {
			m.setStatus(p.Name, "offline", err.Error())
			return outDone, "", fmt.Errorf("write: %w", err)
		}
	}
	if hasStatus {
		st, ok, err := queryStatus(c)
		if err != nil {
			// the connection broke after the write: the print may or may not
			// have happened; retry (a duplicate is possible, see the docs)
			m.setStatus(p.Name, "offline", err.Error())
			return outDone, "", err
		}
		if ok && (st.PaperEndStop || st.ErrorStop || st.MechError || st.CutterError || st.Unrecovered) {
			m.setStatus(p.Name, "paper", describe(st))
			return outWait, describe(st), nil
		}
		if ok {
			m.setStatus(p.Name, "ok", "")
		}
	} else {
		m.setStatus(p.Name, "ok", "")
	}
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
	l := m.lockFor(prn.Name)
	l.Lock()
	defer l.Unlock()
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
	if res == outWait {
		return m.wait(ctx, rec, reason)
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
