//go:build !no_sync

// Command parking is the exit-gate scenario of phase 3 (docs/SYNC_DESIGN.md
// §9.1): a parking company with a hub (solo binary), two gates (edge binaries)
// and one officer's phone (the nano embed API inside this process), 48 simulated
// hours without network, then a reconnect and eight assertions (a) to (h).
//
// Run it with tests/e2e/parking.sh, which builds the binaries and passes them in.
// The simulated clock is a file read by every process (TOKI_SYNC_TEST_CLOCK_FILE),
// advanced one hour at a time.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tokibase/tokibase/embed"
)

var cfg struct {
	hubBin, gateBin, proxyBin, wasmGuest, work string
	hours, gateNew, gateClose                  int
	settle                                     time.Duration
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[parking] "+format+"\n", a...)
}

func main() {
	flag.StringVar(&cfg.hubBin, "hub-bin", "", "solo toki binary (hub)")
	flag.StringVar(&cfg.gateBin, "gate-bin", "", "edge toki binary (gates)")
	flag.StringVar(&cfg.proxyBin, "proxy-bin", "", "syncproxy binary")
	flag.StringVar(&cfg.wasmGuest, "wasm-guest", "", "syncconflict .wasm guest (double payment hook)")
	flag.StringVar(&cfg.work, "work", "", "work directory (created by parking.sh)")
	flag.IntVar(&cfg.hours, "hours", 48, "simulated hours offline")
	flag.IntVar(&cfg.gateNew, "gate-new", 20, "tickets each gate creates per hour")
	flag.IntVar(&cfg.gateClose, "gate-close", 15, "tickets each gate closes per hour")
	flag.DurationVar(&cfg.settle, "settle", 150*time.Second, "bound for the final convergence")
	flag.Parse()
	for _, p := range []string{cfg.hubBin, cfg.gateBin, cfg.proxyBin, cfg.wasmGuest, cfg.work} {
		if p == "" {
			fmt.Fprintln(os.Stderr, "parking: all of -hub-bin -gate-bin -proxy-bin -wasm-guest -work are required")
			os.Exit(2)
		}
	}
	sc := newScenario()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; sc.cleanup(); os.Exit(130) }()
	err := sc.run()
	sc.cleanup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parking] ABORTED: %v\n", err)
		os.Exit(1)
	}
	if !sc.report() {
		os.Exit(1)
	}
}

// phoneOptions are the embed options of the officer's phone.
func (s *scenario) phoneOptions(hub string) embed.Options {
	return embed.Options{
		DataDir: filepath.Join(cfg.work, "phone"), Listen: "-", Profile: "nano", LogLevel: "error",
		Sync: &embed.SyncOptions{HubURL: hub, Interval: "2s"},
		Env: map[string]string{
			"TOKI_SYNC_INSECURE": "1", "TOKI_SYNC_TEST": "1", "TOKI_SYNC_TEST_CLOCK_FILE": s.clockFile, "TOKI_SYNC_PAGE": "100",
		},
	}
}
