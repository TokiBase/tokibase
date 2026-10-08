//go:build !no_sync

package sync

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// ticketsHub is a hub with a `tickets` collection whose `no` field is
// `reserve:tickets` and a sequence of blocks of 10.
func ticketsHub(t *testing.T) *hubEnv {
	t.Helper()
	h := newHub(t)
	open := ""
	c := core.NewBaseCollection("tickets")
	c.Fields.Add(
		&core.TextField{Name: "no", Required: true},
		&core.TextField{Name: "plate"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_tickets_no", true, "no", "")
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = &open, &open, &open, &open, &open
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	h.policy(t, "tickets", DirBoth, map[string]string{"no": "reserve:tickets"}, nil)
	if _, err := CreateSequence(h.app, SequenceOptions{Name: "tickets", Block: 10, MaxOpen: 2, MaxBlock: 50, Format: "G-{n:06}"}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (s *itemsSpoke) newTicket(t *testing.T, plate string) (int, string) {
	t.Helper()
	return s.localDo(t, "POST", "/api/collections/tickets/records", `{"plate":"`+plate+`"}`)
}

func ticketNo(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	return m["no"].(string)
}

func TestReservationExhaustionFailsClosedLocally(t *testing.T) {
	h := ticketsHub(t)
	s := newBareSpoke(t, h, "gate-1")
	// handshake only: the schema is there, no range was fetched yet
	if _, err := s.c.Handshake(ctxb); err != nil {
		t.Fatal(err)
	}
	s.sync(t) // bundle, and the first range at the end of the session
	st, body := s.newTicket(t, "B1")
	if st != 200 || ticketNo(t, body) != "1" {
		t.Fatalf("first ticket: %d %s", st, body)
	}
	for i := 2; i <= 10; i++ {
		st, body = s.newTicket(t, "B"+strconv.Itoa(i))
		if st != 200 || ticketNo(t, body) != strconv.Itoa(i) {
			t.Fatalf("ticket %d: %d %s", i, st, body)
		}
	}
	// the range of 10 is used up: the create fails closed with 503
	st, body = s.newTicket(t, "B11")
	var eb proto.ErrorBody
	_ = json.Unmarshal([]byte(body), &eb)
	if st != http.StatusServiceUnavailable || eb.Data["code"] != proto.CodeReservationExhausted {
		t.Fatalf("exhausted create: %d %s", st, body)
	}
	if n, _ := s.app.CountRecords("tickets"); n != 10 {
		t.Fatalf("records %d: a failed create must leave nothing behind", n)
	}
	// connecting to the hub brings the next range, numbers continue without a repeat
	s.sync(t)
	st, body = s.newTicket(t, "B11")
	if st != 200 || ticketNo(t, body) != "11" {
		t.Fatalf("ticket after the next range: %d %s", st, body)
	}
	// Go API
	if n, err := Next(s.app, "tickets"); err != nil || n != 12 {
		t.Fatalf("Next: %d %v", n, err)
	}
	if _, err := Next(s.app, "no_such_sequence"); !errors.Is(err, ErrReservationExhausted) {
		t.Fatalf("unknown sequence on a spoke: %v", err)
	}
}

func TestPrefetchBelowTwentyPercent(t *testing.T) {
	h := ticketsHub(t)
	s := newBareSpoke(t, h, "gate-1")
	s.sync(t)
	for i := 0; i < 9; i++ { // 1 of 10 left: below 20 %
		if _, err := Next(s.app, "tickets"); err != nil {
			t.Fatal(err)
		}
	}
	if !s.c.NeedsRange("tickets") {
		t.Fatal("below 20 % remaining a new range is due")
	}
	s.sync(t) // the cycle tops up before the range is exhausted
	if n := countWhere(t, s.app, "_sync_reserved", "sequence='tickets' AND status='active'", nil); n != 2 {
		t.Fatalf("active ranges after the prefetch: %d", n)
	}
	if n, err := Next(s.app, "tickets"); err != nil || n != 10 {
		t.Fatalf("Next: %d %v", n, err)
	}
	if n, err := Next(s.app, "tickets"); err != nil || n != 11 {
		t.Fatalf("Next across ranges: %d %v", n, err)
	}
}

func TestTwoSpokesNeverIssueTheSameNumber(t *testing.T) {
	h := ticketsHub(t)
	a, b := newBareSpoke(t, h, "gate-1"), newBareSpoke(t, h, "gate-2")
	a.sync(t)
	b.sync(t)
	for i := 0; i < 6; i++ {
		if st, body := a.newTicket(t, "A"+strconv.Itoa(i)); st != 200 {
			t.Fatalf("a: %d %s", st, body)
		}
		if st, body := b.newTicket(t, "B"+strconv.Itoa(i)); st != 200 {
			t.Fatalf("b: %d %s", st, body)
		}
	}
	a.sync(t)
	b.sync(t)
	a.sync(t)
	recs, err := h.app.FindAllRecords("tickets")
	if err != nil || len(recs) != 12 {
		t.Fatalf("hub tickets: %d %v", len(recs), err)
	}
	seen := map[string]bool{}
	for _, r := range recs {
		no := r.GetString("no")
		if seen[no] {
			t.Fatalf("duplicate ticket number %s", no)
		}
		seen[no] = true
	}
	if n, _ := a.app.CountRecords("tickets"); n != 12 {
		t.Fatalf("spoke a holds %d tickets", n)
	}
	// high water follows the pushes
	var hw float64
	_ = h.app.DB().NewQuery("SELECT MAX(high_water) FROM _sync_reservations WHERE node={:n}").Bind(dbx.Params{"n": a.m.NodeID()}).Row(&hw)
	if hw != 6 && hw != 16 {
		// a took 1..6 from its first range, b 11..16 from its own
		t.Fatalf("high water of a: %v", hw)
	}
}

func TestPushedValueOutsideTheIssuedRangeIsRejected(t *testing.T) {
	h := ticketsHub(t)
	a, b := newBareSpoke(t, h, "gate-1"), newBareSpoke(t, h, "gate-2")
	a.sync(t)
	b.sync(t) // a holds 1..10, b holds 11..20
	cid := h.items.Id
	if col, _ := h.app.FindCollectionByNameOrId("tickets"); col != nil {
		cid = col.Id
	}
	tok := a.token(t)
	push := func(oseq int64, rec, no string) proto.PushResult {
		req := pushReq(pc(a.m.NodeID(), oseq, nowHLC(-100, uint16(oseq)), 0, cid, rec, "c", map[string]any{"no": no, "plate": "x"}))
		req.SchemaVersion = hubSchemaVersion(h)
		st, ok, eb := rawPush(t, h, tok, req)
		if st != 200 || len(ok.Results) != 1 {
			t.Fatalf("push: %d %+v %+v", st, ok, eb)
		}
		return ok.Results[0]
	}
	if r := push(1, "tkt000000000001", "5"); r.Status != proto.ResApplied {
		t.Fatalf("in range: %+v", r)
	}
	if r := push(2, "tkt000000000002", "15"); r.Status != proto.ResRejected || r.Code != proto.CodeReservationOutOfRange {
		t.Fatalf("the range of another node: %+v", r)
	}
	if r := push(3, "tkt000000000003", "500"); r.Status != proto.ResRejected || r.Code != proto.CodeReservationOutOfRange {
		t.Fatalf("never issued: %+v", r)
	}
	if r := push(4, "tkt000000000004", "abc"); r.Status != proto.ResRejected || r.Code != proto.CodeReservationOutOfRange {
		t.Fatalf("not a number: %+v", r)
	}
	if n := countWhere(t, h.app, "tickets", "1=1", nil); n != 1 {
		t.Fatalf("hub tickets %d", n)
	}
	confs, _ := h.app.FindAllRecords(ConflictsCollection, dbx.NewExp("kind='reservation_out_of_range'"))
	if len(confs) != 3 {
		t.Fatalf("conflict rows: %d", len(confs))
	}
	var hw float64
	_ = h.app.DB().NewQuery("SELECT high_water FROM _sync_reservations WHERE node={:n}").Bind(dbx.Params{"n": a.m.NodeID()}).Row(&hw)
	if hw != 5 {
		t.Fatalf("high water %v", hw)
	}
	// using the last value exhausts the range
	if r := push(5, "tkt000000000005", "10"); r.Status != proto.ResApplied {
		t.Fatalf("last value: %+v", r)
	}
	if n := countWhere(t, h.app, "_sync_reservations", "node={:n} AND status='exhausted'", dbx.Params{"n": a.m.NodeID()}); n != 1 {
		t.Fatalf("exhausted ranges: %d", n)
	}
	// an exhausted range still validates the pushes of its node
	if r := push(6, "tkt000000000006", "7"); r.Status != proto.ResApplied {
		t.Fatalf("exhausted range: %+v", r)
	}
}

func TestReservationLimitsReleaseAndList(t *testing.T) {
	h := ticketsHub(t)
	a := newBareSpoke(t, h, "gate-1")
	a.sync(t) // one range (1..10)
	reserve := func(count int64) error { return a.c.Reserve(ctxb, "tickets", count) }
	if err := reserve(0); err != nil { // second open range
		t.Fatal(err)
	}
	// third: max_open_per_node = 2
	err := reserve(0)
	if err == nil || !errContains(err, proto.CodeReservationLimit) {
		t.Fatalf("third range: %v", err)
	}
	// count above max_block
	if err := reserve(51); err == nil || !errContains(err, proto.CodeReservationLimit) {
		t.Fatalf("count above max_block: %v", err)
	}
	// unknown sequence
	if err := a.c.Reserve(ctxb, "nope", 0); err == nil || !errContains(err, "sync_unknown_sequence") {
		t.Fatalf("unknown sequence: %v", err)
	}
	// the hub lists two active ranges and the handshake carries them
	hs, err := a.c.Handshake(ctxb)
	if err != nil || len(hs.Reservations) != 2 || hs.Reservations[0].Start != 1 || hs.Reservations[1].Start != 11 {
		t.Fatalf("handshake reservations: %+v %v", hs.Reservations, err)
	}
	// release the second range: the node may ask again, the numbers are never reissued
	var id string
	_ = a.app.DB().NewQuery("SELECT id FROM _sync_reserved WHERE start=11").Row(&id)
	if err := a.c.ReleaseRange(ctxb, id); err != nil {
		t.Fatal(err)
	}
	if err := reserve(0); err != nil {
		t.Fatalf("after the release: %v", err)
	}
	if n := countWhere(t, h.app, "_sync_reservations", "node={:n} AND start=21", dbx.Params{"n": a.m.NodeID()}); n != 1 {
		t.Fatal("the next range must start after the released one (21), never reuse 11..20")
	}
	// the released range is retired on the hub and no longer used locally
	if n := countWhere(t, h.app, "_sync_reservations", "status='retired'", nil); n != 1 {
		t.Fatalf("retired ranges: %d", n)
	}
	if n := countWhere(t, a.app, "_sync_reserved", "status='retired'", nil); n != 1 {
		t.Fatalf("retired local ranges: %d", n)
	}
	// ListReservations backs the CLI
	seqs, rngs, err := ListReservations(h.app)
	if err != nil || len(seqs) != 1 || seqs[0].Name != "tickets" || seqs[0].Next != 31 || len(rngs) != 3 {
		t.Fatalf("list: %+v %+v %v", seqs, rngs, err)
	}
}

func errContains(err error, s string) bool { return err != nil && strings.Contains(err.Error(), s) }

func TestRevokingANodeRetiresItsRanges(t *testing.T) {
	h := ticketsHub(t)
	a := newBareSpoke(t, h, "gate-1")
	a.sync(t)
	if n := countWhere(t, h.app, "_sync_reservations", "status='active'", nil); n != 1 {
		t.Fatalf("active ranges %d", n)
	}
	if _, err := RevokeNode(h.app, "gate-1", true); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, h.app, "_sync_reservations", "status='retired'", nil); n != 1 {
		t.Fatalf("retired ranges %d", n)
	}
}

func TestHubNextDrawsFromTheSequence(t *testing.T) {
	h := ticketsHub(t)
	for want := int64(1); want <= 3; want++ {
		if n, err := Next(h.app, "tickets"); err != nil || n != want {
			t.Fatalf("hub Next: %d %v", n, err)
		}
	}
	// a hub-local record gets a number from the sequence too
	r := core.NewRecord(pr8Col(t, h.app, "tickets"))
	r.Set("plate", "H1")
	if err := h.app.Save(r); err != nil || r.GetString("no") != "4" {
		t.Fatalf("hub autofill: %q %v", r.GetString("no"), err)
	}
	// the numbers handed to nodes start after them
	a := newBareSpoke(t, h, "gate-1")
	a.sync(t)
	if n := countWhere(t, h.app, "_sync_reservations", "start=5", nil); n != 1 {
		t.Fatal("the first range must start after the hub's own numbers")
	}
}

func pr8Col(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUsedValuesFreeTheOpenSlotBeforeThePushArrives(t *testing.T) {
	h := ticketsHub(t)
	a := newBareSpoke(t, h, "gate-1")
	a.sync(t)                                               // range 1..10
	if err := a.c.Reserve(ctxb, "tickets", 0); err != nil { // 11..20: two open ranges
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ { // burn the first range offline (nothing pushed yet)
		if _, err := Next(a.app, "tickets"); err != nil {
			t.Fatal(err)
		}
	}
	if n := countWhere(t, a.app, "_sync_reserved", "status='exhausted'", nil); n != 1 {
		t.Fatalf("local exhausted ranges: %d", n)
	}
	// the hub does not know yet: the request reports the usage and gets a third range
	if err := a.c.Reserve(ctxb, "tickets", 0); err != nil {
		t.Fatalf("third range after the first was used up: %v", err)
	}
	if n := countWhere(t, h.app, "_sync_reservations", "node={:n} AND status='exhausted'", dbx.Params{"n": a.m.NodeID()}); n != 1 {
		t.Fatalf("hub exhausted ranges: %d", n)
	}
}
