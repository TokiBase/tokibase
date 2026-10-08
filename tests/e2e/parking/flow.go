//go:build !no_sync

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

func (s *scenario) run() error {
	logf("hub: solo binary, gates: edge binaries, phone: embed (nano)")
	if err := s.startHub(); err != nil {
		return fmt.Errorf("hub: %w", err)
	}
	if err := s.hubSchema(); err != nil {
		return fmt.Errorf("hub schema: %w", err)
	}
	if err := s.startWebhookSink(); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	if err := s.hubSeed(); err != nil {
		return fmt.Errorf("hub seed: %w", err)
	}
	target := strings.TrimPrefix(s.hubURL, "http://")
	for i := 1; i <= 2; i++ {
		if err := s.addGate(i, target); err != nil {
			return fmt.Errorf("gate %d: %w", i, err)
		}
	}
	if err := s.startPhone(target); err != nil {
		return fmt.Errorf("phone: %w", err)
	}

	// 1. all online: bootstrap, reservations
	logf("step 1: all online, waiting for the bootstrap and the reserved ranges")
	for _, g := range s.gates {
		g := g
		if err := waitFor(90*time.Second, g.name+" bootstrap", func() bool { return s.hasSeeds(g.url, g.tok, nil) }); err != nil {
			return err
		}
	}
	if err := waitFor(90*time.Second, "phone bootstrap", func() bool { return s.hasSeeds("", s.phoneSU, s.pcall) }); err != nil {
		st, _ := s.phone.Sync().Status()
		return fmt.Errorf("%w (%s; status %s)", err, s.lastSeed, st)
	}
	for _, g := range s.gates {
		g := g
		if err := waitFor(60*time.Second, g.name+" reserved range", func() bool { _, err := s.gateCreate(g, 0, 0); return err == nil }); err != nil {
			return err
		}
	}
	if err := waitFor(60*time.Second, "phone reserved range", func() bool { _, err := s.phoneTicket(0, 0); return err == nil }); err != nil {
		return err
	}
	// let the warm-up tickets reach the hub before the lights go out
	if err := waitFor(60*time.Second, "warm-up tickets on the hub", func() bool { return s.hubCount("tickets") >= 33 }); err != nil {
		return err
	}
	logf("step 1 done: %d tickets on the hub", s.hubCount("tickets"))

	// 2. network loss, 48 simulated hours
	proxies := []*proxyHandle{s.gates[0].proxy, s.gates[1].proxy, s.phoneProxy}
	for _, p := range proxies {
		if err := p.set(false); err != nil {
			return err
		}
	}
	t0 := time.Now()
	for h := 1; h <= cfg.hours; h++ {
		if err := s.hour(h); err != nil {
			return fmt.Errorf("hour %d: %w", h, err)
		}
		if h%12 == 0 {
			logf("hour %d/%d done (%s real, %d tickets created so far)", h, cfg.hours, time.Since(t0).Round(time.Second), s.createdCount())
		}
	}
	// the hub did not see any of it
	logf("offline phase over: hub holds %d tickets, ledger %d", s.hubCount("tickets"), s.createdCount())

	// 3. network back
	for _, p := range proxies {
		if err := p.set(true); err != nil {
			return err
		}
	}
	for _, g := range s.gates {
		// a gate that was down for a long time has backed off for minutes: restart it, as a reboot would
		g.proc.kill()
		if err := s.startGate(g); err != nil {
			return err
		}
	}
	logf("step 3: network back, syncing")
	if err := s.converge(); err != nil {
		s.record("converge", "all nodes idle and equal", false, "%v", err)
	}
	s.assertAll()
	return nil
}

// hasSeeds reports whether a node already holds the 30 seed tickets and the 3 rates.
func (s *scenario) hasSeeds(base, tok string, phone func(string, string, string, any) (map[string]any, error)) bool {
	get := func(col string) int {
		path := "/api/collections/" + col + "/records?perPage=1"
		var r map[string]any
		var err error
		if phone != nil {
			r, err = phone(tok, "GET", path, nil)
		} else {
			r, err = api("GET", base+path, tok, nil)
		}
		if err != nil {
			return -1
		}
		n, _ := r["totalItems"].(float64)
		return int(n)
	}
	t, r, p := get("tickets"), get("rates"), get("payments")
	s.lastSeed = fmt.Sprintf("tickets=%d rates=%d payments=%d", t, r, p)
	return t >= 30 && r == 3 && p >= 1
}

func (s *scenario) hubCount(col string) int {
	r, err := s.hubAPI("GET", "/api/collections/"+col+"/records?perPage=1", nil)
	if err != nil {
		return -1
	}
	n, _ := r["totalItems"].(float64)
	return int(n)
}

func (s *scenario) createdCount() int {
	s.led.mu.Lock()
	defer s.led.mu.Unlock()
	return len(s.led.t)
}

// ---- one simulated hour ------------------------------------------------------

func (s *scenario) hour(h int) error {
	if err := s.clock.advance(time.Hour); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond) // every process re-reads the clock file within 50 ms
	switch h {
	case 24:
		if _, err := s.hubAPI("PATCH", "/api/collections/rates/records/"+s.carRate, map[string]any{"price": 7000}); err != nil {
			return fmt.Errorf("rates change on the hub: %w", err)
		}
	case 30:
		if _, err := runCLI(cfg.hubBin, s.hubEnvv, s.hubDir, "sync", "purge", "tickets", s.tp, "--legal", "--reason", "e2e erasure"); err != nil {
			return fmt.Errorf("purge on the hub: %w", err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i, g := range s.gates {
		wg.Add(1)
		go func(i int, g *gate) {
			defer wg.Done()
			if err := s.gateHour(i+1, g, h); err != nil {
				errs <- fmt.Errorf("%s: %w", g.name, err)
			}
		}(i, g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.phoneHour(h); err != nil {
			errs <- fmt.Errorf("phone: %w", err)
		}
	}()
	wg.Wait()
	close(errs)
	return <-errs
}

func (s *scenario) ticketBody() map[string]any {
	return map[string]any{"plate": "B 1234 XY", "entry_at": s.clock.date(), "status": "open", "branch": "B1", "fee": 0}
}

func (s *scenario) gateCreate(g *gate, h, i int) (string, error) {
	body := s.ticketBody()
	body["plate"] = fmt.Sprintf("%s-%02d-%02d", strings.ToUpper(g.name[:1]), h, i)
	r, err := api("POST", g.url+"/api/collections/tickets/records", g.tok, body)
	if err != nil {
		return "", err
	}
	id := r["id"].(string)
	s.led.add(&ticket{id: id, creator: g.name})
	g.open = append(g.open, id)
	return id, nil
}

func (s *scenario) phoneTicket(h, i int) (string, error) {
	body := s.ticketBody()
	body["plate"] = fmt.Sprintf("P-%02d-%02d", h, i)
	r, err := s.pcall(s.phoneTok, "POST", "/api/collections/tickets/records", body)
	if err != nil {
		return "", err
	}
	id := r["id"].(string)
	s.led.add(&ticket{id: id, creator: "phone"})
	return id, nil
}

func (s *scenario) gateHour(n int, g *gate, h int) error {
	for i := 0; i < cfg.gateNew; i++ {
		if _, err := s.gateCreate(g, h, i); err != nil {
			return fmt.Errorf("create: %w", err)
		}
	}
	for i := 0; i < cfg.gateClose && len(g.open) > 0; i++ {
		id := g.open[0]
		g.open = g.open[1:]
		inc := float64(1000 + 100*(i%4))
		if _, err := api("PATCH", g.url+"/api/collections/tickets/records/"+id, g.tok,
			map[string]any{"status": "closed", "exit_at": s.clock.date(), "fee+": inc}); err != nil {
			return fmt.Errorf("close: %w", err)
		}
		s.led.inc(id, inc)
	}
	patch := func(col, id string, body map[string]any) error {
		_, err := api("PATCH", g.url+"/api/collections/"+col+"/records/"+id, g.tok, body)
		return err
	}
	switch {
	case n == 1 && h == 5: // gate-1 takes the QR payment of the invoice
		return patch("payments", s.pd, map[string]any{"status": "paid", "provider_ref": "QR-111"})
	case n == 2 && h == 10: // gate-2 closes the ticket that the phone flags next hour
		if err := patch("tickets", s.tx, map[string]any{"status": "closed", "exit_at": s.clock.date(), "fee+": 2000}); err != nil {
			return err
		}
		s.led.inc(s.tx, 2000)
	case n == 1 && h == 10: // gate-1 and the phone both fix the plate: a real field conflict
		return patch("tickets", s.tx, map[string]any{"plate": "G1-PLATE"})
	case n == 1 && h == 31: // gate-1 edits the ticket the hub purged at hour 30
		return patch("tickets", s.tp, map[string]any{"note": "late edit"})
	}
	return nil
}

func (s *scenario) phoneHour(h int) error {
	for j := 0; j < 5; j++ {
		id := s.pool[(h*5+j)%len(s.pool)]
		f := flagVals[(h+j)%len(flagVals)]
		if _, err := s.pcall(s.phoneTok, "PATCH", "/api/collections/tickets/records/"+id, map[string]any{"flags+": []string{f}}); err != nil {
			return fmt.Errorf("flag: %w", err)
		}
		s.led.flag(id, f)
	}
	for j := 0; j < 2; j++ {
		id := s.pool[(h*2+j)%len(s.pool)]
		if _, err := s.pcall(s.phoneTok, "POST", "/api/collections/payments/records", map[string]any{
			"ticket": id, "amount": 5000, "status": "paid", "provider_ref": fmt.Sprintf("CASH-%d-%d", h, j)}); err != nil {
			return fmt.Errorf("payment: %w", err)
		}
		if _, err := s.phoneTicket(h, j); err != nil {
			return fmt.Errorf("ticket: %w", err)
		}
	}
	switch h {
	case 6: // the officer takes cash for the invoice gate-1 already settled by QR
		if _, err := s.pcall(s.phoneTok, "PATCH", "/api/collections/payments/records/"+s.pd, map[string]any{"status": "paid", "provider_ref": "CASH-9"}); err != nil {
			return err
		}
	case 11:
		if _, err := s.pcall(s.phoneTok, "PATCH", "/api/collections/tickets/records/"+s.tx,
			map[string]any{"flags+": []string{"disputed"}, "fee+": 500, "plate": "PHONE-PLATE"}); err != nil {
			return err
		}
		s.led.flag(s.tx, "disputed")
		s.led.inc(s.tx, 500)
	}
	return nil
}

// ---- convergence ---------------------------------------------------------------

func (s *scenario) pendingZero() bool {
	for _, g := range s.gates {
		r, err := api("GET", g.url+"/api/health", g.tok, nil)
		if err != nil {
			return false
		}
		d, _ := r["data"].(map[string]any)
		sy, _ := d["sync"].(map[string]any)
		if p, _ := sy["pending"].(float64); p != 0 {
			return false
		}
	}
	b, err := s.phone.Sync().Status()
	if err != nil {
		return false
	}
	return strings.Contains(string(b), `"pending":0`)
}

func (s *scenario) converge() error {
	deadline := time.Now().Add(cfg.settle)
	start := time.Now()
	last := time.Time{}
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		_ = s.phone.Sync().Now(ctx)
		cancel()
		if s.pendingZero() {
			d, err := s.digests()
			if err == nil && digestsEqual(d) {
				logf("converged after %s", time.Since(start).Round(time.Second))
				s.record("converge", "all nodes idle and equal", true, "%s after reconnect", time.Since(start).Round(time.Second))
				return nil
			}
			if time.Since(last) > 15*time.Second {
				last = time.Now()
				logf("not converged yet: %v %s", err, summarize(d))
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("no convergence within %s", cfg.settle)
}
