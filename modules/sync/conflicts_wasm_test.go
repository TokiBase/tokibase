//go:build !no_sync && !no_wasm

package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/modules/wasm"
)

// buildGuest builds modules/wasm/testdata/<name> for wasip1 (skips when the
// toolchain cannot).
func buildGuest(t *testing.T, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	wasmDir := filepath.Join(filepath.Dir(file), "..", "wasm")
	out := filepath.Join(t.TempDir(), name+".wasm")
	cmd := exec.Command("go", "build", "-o", out, "./testdata/"+name)
	cmd.Dir = wasmDir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build guest %s: %v\n%s", name, err, b)
	}
	return out
}

// The §4.6 double payment: gate-1 recorded the QR payment, the offline phone
// recorded a cash payment for the same invoice. The wasm hook merges it into a
// refund note; a payment on an unpaid invoice is accepted.
func TestWasmHookDoublePayment(t *testing.T) {
	h, a, _ := hubFixture(t)
	open := ""
	pay := core.NewBaseCollection("payments")
	pay.Fields.Add(&core.TextField{Name: "invoice"}, &core.TextField{Name: "status"}, &core.TextField{Name: "provider_ref"},
		&core.NumberField{Name: "amount"}, &core.TextField{Name: "note"})
	pay.ListRule, pay.ViewRule, pay.CreateRule, pay.UpdateRule, pay.DeleteRule = &open, &open, &open, &open, &open
	if err := h.app.Save(pay); err != nil {
		t.Fatal(err)
	}
	h.policy(t, "payments", DirBoth, nil, nil)
	h.strategy(t, "payments", StratHook, "payments_conflict", false)

	dir := t.TempDir()
	wasmFile, err := os.ReadFile(buildGuest(t, "syncconflict"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payments_conflict.wasm"), wasmFile, 0o644); err != nil {
		t.Fatal(err)
	}
	toml := "events = [\"sync.conflict.payments\"]\ntimeout_ms = 3000\n[env]\nMODE = \"double_payment\"\n"
	if err := os.WriteFile(filepath.Join(dir, "payments_conflict.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	host := wasm.RegisterWithConfig(h.app, wasm.Config{Dir: dir})
	t.Cleanup(host.Close)
	if len(host.Modules()) != 1 {
		t.Fatalf("modules: %v", host.LoadErrors())
	}

	ta, na := a.token(t), a.m.NodeID()
	cid := pay.Id
	mk := func(vals map[string]any) *core.Record {
		r := core.NewRecord(pay)
		for k, v := range vals {
			r.Set(k, v)
		}
		if err := h.app.Save(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	paid := mk(map[string]any{"invoice": "inv_1", "status": "paid", "provider_ref": "QR-111", "amount": 50000})
	unpaid := mk(map[string]any{"invoice": "inv_2", "status": "pending", "amount": 1000})

	// the phone never saw the hub state (base 0): concurrent
	_, resp, eb := rawPush(t, h, ta, pushReq(
		pc(na, 1, nowHLC(-2000, 0), 0, cid, paid.Id, "u", map[string]any{"status": "paid", "provider_ref": "CASH-9"}),
		pc(na, 2, nowHLC(-1000, 0), 0, cid, unpaid.Id, "u", map[string]any{"status": "paid", "provider_ref": "CASH-10"}),
	))
	if len(resp.Results) != 2 {
		t.Fatalf("%+v %+v", resp, eb)
	}
	if resp.Results[0].Status != proto.ResMerged || resp.Results[1].Status != proto.ResApplied {
		t.Fatalf("results %+v", resp.Results)
	}
	hr, _ := h.app.FindRecordById("payments", paid.Id)
	if hr.GetString("provider_ref") != "QR-111" || hr.GetString("status") != "paid" || !strings.Contains(hr.GetString("note"), "double payment CASH-9") {
		t.Fatalf("merged payment: %v", hr.FieldsData())
	}
	hu, _ := h.app.FindRecordById("payments", unpaid.Id)
	if hu.GetString("provider_ref") != "CASH-10" {
		t.Fatalf("accepted payment: %v", hu.FieldsData())
	}
	rows := conflictRows(t, h.app)
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	res := map[string]string{}
	for _, r := range rows {
		res[r.Record] = r.Resolution + "/" + r.Status
	}
	if res[paid.Id] != ResolutionAutoMerge+"/resolved" || res[unpaid.Id] != ResolutionAccepted+"/resolved" {
		t.Fatalf("rows %v", res)
	}
	_ = hlc.HLC(0)
}

// A wasm module that traps parks the change: fail closed, code hook_failed.
func TestWasmHookTrapParks(t *testing.T) {
	h, a, _ := hubFixture(t)
	h.strategy(t, "items", StratHook, "", false)
	dir := t.TempDir()
	wasmFile, err := os.ReadFile(buildGuest(t, "syncconflict"))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "g.wasm"), wasmFile, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "g.toml"), []byte("events = [\"sync.conflict.*\"]\ntimeout_ms = 3000\n[env]\nMODE = \"trap\"\n"), 0o644)
	host := wasm.RegisterWithConfig(h.app, wasm.Config{Dir: dir})
	t.Cleanup(host.Close)
	staleEdit(t, h, a)
	if r := a.sync(t); r.Parked != 1 {
		t.Fatalf("%+v", r)
	}
	rows := conflictRows(t, h.app)
	if len(rows) != 1 || rows[0].Kind != KindHookFailed || rows[0].Status != ConflictOpen {
		t.Fatalf("%+v", rows)
	}
}
