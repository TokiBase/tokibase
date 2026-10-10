//go:build !no_sync

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	tsync "github.com/tokibase/tokibase/modules/sync"
)

var scoped = []string{"tickets", "payments", "rates"}

type colDigest struct {
	Records  int
	Digest   string
	Mismatch int
}

type nodeDigest struct {
	Pending int64
	Cols    map[string]colDigest
}

// digests runs `toki sync verify` (hub, gates) and sync.Verify (phone).
func (s *scenario) digests() (map[string]nodeDigest, error) {
	out := map[string]nodeDigest{}
	cli := func(name, bin string, env []string, dir string) error {
		o, err := runCLI(bin, env, dir, "sync", "verify", "--json")
		if err != nil {
			return err
		}
		var rep tsync.VerifyReport
		if err := json.Unmarshal([]byte(lastJSONLine(o)), &rep); err != nil {
			return fmt.Errorf("%s verify output %q: %w", name, o, err)
		}
		out[name] = fromReport(&rep)
		return nil
	}
	if err := cli("hub", cfg.hubBin, s.hubEnvv, s.hubDir); err != nil {
		return out, err
	}
	for _, g := range s.gates {
		if err := cli(g.name, cfg.gateBin, s.gateEnv(), g.dir); err != nil {
			return out, err
		}
	}
	rep, err := tsync.Verify(s.phone.App())
	if err != nil {
		return out, err
	}
	out["phone"] = fromReport(rep)
	return out, nil
}

func fromReport(r *tsync.VerifyReport) nodeDigest {
	nd := nodeDigest{Pending: r.Pending, Cols: map[string]colDigest{}}
	for _, c := range r.Collections {
		nd.Cols[c.Name] = colDigest{Records: c.Records, Digest: c.Digest, Mismatch: len(c.Mismatches)}
	}
	return nd
}

func digestsEqual(d map[string]nodeDigest) bool {
	hub, ok := d["hub"]
	if !ok {
		return false
	}
	for name, nd := range d {
		if name != "hub" && nd.Pending != 0 {
			return false
		}
		for _, c := range scoped {
			h, ok := hub.Cols[c]
			x, ok2 := nd.Cols[c]
			if !ok || !ok2 || h != x || x.Mismatch != 0 || x.Digest == "" {
				return false
			}
		}
	}
	return true
}

func summarize(d map[string]nodeDigest) string {
	names := make([]string, 0, len(d))
	for n := range d {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		fmt.Fprintf(&sb, "%s[pending %d", n, d[n].Pending)
		for _, c := range scoped {
			fmt.Fprintf(&sb, " %s=%d", c, d[n].Cols[c].Records)
		}
		sb.WriteString("] ")
	}
	return sb.String()
}

// ---- reading every node --------------------------------------------------------

type fetcher func(path string) (map[string]any, error)

func (s *scenario) fetchers() map[string]fetcher {
	m := map[string]fetcher{
		"hub": func(p string) (map[string]any, error) { return s.hubAPI("GET", p, nil) },
		"phone": func(p string) (map[string]any, error) {
			return s.pcall(s.phoneSU, "GET", p, nil)
		},
	}
	for _, g := range s.gates {
		g := g
		m[g.name] = func(p string) (map[string]any, error) { return api("GET", g.url+p, g.tok, nil) }
	}
	return m
}

func fetchAll(f fetcher, col string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for page := 1; ; page++ {
		r, err := f(fmt.Sprintf("/api/collections/%s/records?perPage=500&page=%d&sort=id", col, page))
		if err != nil {
			return nil, err
		}
		items, _ := r["items"].([]any)
		for _, it := range items {
			rec, _ := it.(map[string]any)
			out[rec["id"].(string)] = rec
		}
		tp, _ := r["totalPages"].(float64)
		if page >= int(tp) {
			return out, nil
		}
	}
}

func flagSet(v any) map[string]bool {
	out := map[string]bool{}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	case string:
		if x != "" {
			out[x] = true
		}
	}
	return out
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// ---- assertions (a) to (h) -------------------------------------------------------

func (s *scenario) assertAll() {
	fs := s.fetchers()
	names := []string{"hub", "gate-1", "gate-2", "phone"}
	tickets := map[string]map[string]map[string]any{}
	for _, n := range names {
		t, err := fetchAll(fs[n], "tickets")
		if err != nil {
			s.record("read", "read "+n, false, "%v", err)
			return
		}
		tickets[n] = t
	}

	s.led.mu.Lock()
	expected := map[string]*ticket{}
	byCreator := map[string]int{}
	for id, t := range s.led.t {
		byCreator[t.creator]++
		if id != s.tp {
			expected[id] = t
		}
	}
	s.led.mu.Unlock()

	// (a) zero loss, exactly once
	{
		ok, detail := true, ""
		for _, n := range names {
			var missing, extra int
			for id := range expected {
				if _, has := tickets[n][id]; !has {
					missing++
				}
			}
			for id := range tickets[n] {
				if _, has := expected[id]; !has {
					extra++
				}
			}
			if missing > 0 || extra > 0 {
				ok = false
				detail += fmt.Sprintf("%s: %d missing, %d unexpected; ", n, missing, extra)
			}
		}
		if ok {
			detail = fmt.Sprintf("%d tickets (%d seed, %d gate-1, %d gate-2, %d phone, %d hub-purged excluded) exist exactly once on the hub and on every node",
				len(expected), 30, byCreator["gate-1"], byCreator["gate-2"], byCreator["phone"], 1)
		}
		s.record("a", "zero loss", ok, "%s", detail)
	}

	// (b) no duplicate numbers
	{
		ok, detail := true, ""
		for _, n := range names {
			seen := map[string]bool{}
			dups, empty := 0, 0
			for _, r := range tickets[n] {
				no, _ := r["no"].(string)
				if no == "" {
					empty++
				} else if seen[no] {
					dups++
				}
				seen[no] = true
			}
			if dups > 0 || empty > 0 {
				ok = false
				detail += fmt.Sprintf("%s: %d duplicate, %d empty; ", n, dups, empty)
			}
		}
		if ok {
			detail = fmt.Sprintf("%d distinct ticket numbers on each of the 4 nodes", len(tickets["hub"]))
		}
		s.record("b", "no duplicate numbers", ok, "%s", detail)
	}

	// (c) digests
	{
		d, err := s.digests()
		ok := err == nil && digestsEqual(d)
		detail := summarize(d)
		if err != nil {
			detail = err.Error()
		} else if ok {
			detail = "tickets, payments and rates digests equal on hub, gate-1, gate-2 and phone; " + detail
		}
		s.record("c", "sync verify digests equal", ok, "%s", detail)
	}

	// (d) counters and sets
	{
		bad := []string{}
		for _, n := range names {
			for id, t := range expected {
				r := tickets[n][id]
				if r == nil {
					continue
				}
				if fee, _ := r["fee"].(float64); fee != t.fee {
					bad = append(bad, fmt.Sprintf("%s %s fee %v want %v", n, id, fee, t.fee))
				}
				if t.seed && !sameSet(flagSet(r["flags"]), t.flags) {
					bad = append(bad, fmt.Sprintf("%s %s flags %v want %v", n, id, r["flags"], t.flags))
				}
			}
		}
		var sum float64
		for _, t := range expected {
			sum += t.fee
		}
		if len(bad) > 8 {
			bad = append(bad[:8], "...")
		}
		s.record("d", "counters and sets equal the sum of the operations", len(bad) == 0,
			"%s", orElse(strings.Join(bad, "; "), fmt.Sprintf("fee total %.0f = sum of all increments, flag sets = union of all adds, on all 4 nodes", sum)))
	}

	s.assertConflicts(fs, tickets)
	s.assertPurge(fs, tickets)

	// (g) phone rates
	{
		rates, err := fetchAll(fs["phone"], "rates")
		price := 0.0
		if r := rates[s.carRate]; r != nil {
			price, _ = r["price"].(float64)
		}
		s.record("g", "phone shows the new rates", err == nil && price == 7000, "car price on the phone: %v (hub changed it 5000 -> 7000 at hour 24)", price)
	}

	s.assertWebhook(len(s.led.t))
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *scenario) assertConflicts(fs map[string]fetcher, tickets map[string]map[string]map[string]any) {
	rows, err := fetchAll(fs["hub"], "_sync_conflicts")
	if err != nil {
		s.record("e", "conflicts resolved", false, "%v", err)
		return
	}
	var plateRow, payRow, open int
	var detail []string
	for _, r := range rows {
		col, _ := r["collection"].(string)
		rec, _ := r["record"].(string)
		kind, _ := r["kind"].(string)
		res, _ := r["resolution"].(string)
		st, _ := r["status"].(string)
		if st == "open" {
			open++
		}
		detail = append(detail, fmt.Sprintf("%s/%s/%s/%s", trimCol(col), kind, res, st))
		switch {
		case rec == s.tx && kind == "concurrent_field" && res == "auto_merge" && st == "resolved":
			plateRow++
		case rec == s.pd && res == "auto_merge" && st == "resolved":
			payRow++
		}
	}
	sort.Strings(detail)
	hubTx := tickets["hub"][s.tx]
	hubPd, _ := s.hubAPI("GET", "/api/collections/payments/records/"+s.pd, nil)
	var bad []string
	if plateRow == 0 {
		bad = append(bad, "no resolved concurrent_field conflict for the plate of the shared ticket")
	}
	if payRow == 0 {
		bad = append(bad, "no resolved auto_merge conflict for the double payment")
	}
	if open != 0 {
		bad = append(bad, fmt.Sprintf("%d conflicts still open", open))
	}
	if hubTx == nil {
		bad = append(bad, "shared ticket missing on the hub")
	} else {
		if hubTx["status"] != "closed" || !flagSet(hubTx["flags"])["disputed"] || hubTx["plate"] != "PHONE-PLATE" {
			bad = append(bad, fmt.Sprintf("shared ticket not merged: status=%v flags=%v plate=%v", hubTx["status"], hubTx["flags"], hubTx["plate"]))
		}
		if fee, _ := hubTx["fee"].(float64); fee != 2500 {
			bad = append(bad, fmt.Sprintf("shared ticket fee %v, want 2500", fee))
		}
	}
	note, _ := hubPd["note"].(string)
	if hubPd["status"] != "paid" || !strings.Contains(note, "double payment") {
		bad = append(bad, fmt.Sprintf("double payment not merged by the hook: status=%v note=%q ref=%v", hubPd["status"], note, hubPd["provider_ref"]))
	}
	// every node shows the merged result
	for _, n := range []string{"gate-1", "gate-2", "phone"} {
		if t := tickets[n][s.tx]; t == nil || t["plate"] != "PHONE-PLATE" || t["status"] != "closed" {
			bad = append(bad, n+" does not show the merged shared ticket")
		}
		if p, err := fs[n](fmt.Sprintf("/api/collections/payments/records/%s", s.pd)); err != nil || !strings.Contains(fmt.Sprint(p["note"]), "double payment") {
			bad = append(bad, n+" does not show the merged payment")
		}
	}
	ok := len(bad) == 0
	d := strings.Join(bad, "; ")
	if ok {
		d = fmt.Sprintf("field-merge: plate conflict auto-resolved (phone, later HLC) with status/exit_at/flags/fee merged; hook: double payment merged (%q); 0 open; rows: %s", note, strings.Join(detail, ", "))
	}
	s.record("e", "expected conflicts present and resolved", ok, "%s", d)
}

func trimCol(c string) string {
	switch c {
	case "pbc_tickets":
		return "tickets"
	case "pbc_payments":
		return "payments"
	case "pbc_rates":
		return "rates"
	}
	return c
}

func (s *scenario) assertPurge(fs map[string]fetcher, tickets map[string]map[string]map[string]any) {
	var bad []string
	for n, t := range tickets {
		if _, has := t[s.tp]; has {
			bad = append(bad, n+" still has the purged ticket")
		}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(s.hubDir, "data.db")+"?mode=ro&_pragma=busy_timeout(10000)")
	var rej int
	if err == nil {
		defer db.Close()
		err = db.QueryRow("SELECT COUNT(*) FROM _changes WHERE record=? AND status='rejected' AND code='legal_tombstone'", s.tp).Scan(&rej)
	}
	if err != nil {
		bad = append(bad, "hub log: "+err.Error())
	} else if rej == 0 {
		bad = append(bad, "gate-1's late edit was not rejected as legal_tombstone")
	}
	ok := len(bad) == 0
	d := strings.Join(bad, "; ")
	if ok {
		d = fmt.Sprintf("purged ticket absent on hub, gate-1, gate-2 and phone; gate-1's late edit rejected as legal_tombstone (%d log row)", rej)
	}
	s.record("f", "purged ticket gone, late edit refused", ok, "%s", d)
}

func (s *scenario) assertWebhook(created int) {
	// wait for the queue to drain, then make sure no late duplicate shows up
	_ = waitFor(90*time.Second, "webhook deliveries", func() bool {
		s.sinkMu.Lock()
		defer s.sinkMu.Unlock()
		return len(s.sinkIDs) >= created
	})
	time.Sleep(6 * time.Second)
	s.sinkMu.Lock()
	defer s.sinkMu.Unlock()
	dups := 0
	for _, n := range s.sinkIDs {
		if n > 1 {
			dups++
		}
	}
	ok := len(s.sinkIDs) == created && s.sinkN == created && dups == 0
	s.record("h", "no webhook duplicates", ok, "hub webhook sink got %d record.create deliveries for %d distinct tickets (expected exactly %d), %d duplicated", s.sinkN, len(s.sinkIDs), created, dups)
}
