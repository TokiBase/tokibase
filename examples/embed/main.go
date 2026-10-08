// Command embed is a minimal host program for package embed (size probe and
// usage example): it serves an instance until interrupted.
//
//	embed [flags] [datadir]
//
// Sync demo (docs/EMBED.md, "Sync from a Flutter app"): a hub operator creates a
// code with `toki sync enroll --name phone --profile nano --actor col/id`, then
//
//	embed -sync-hub http://hub.example:8090 -sync-code <code> ./phone_data
//
// enrolls this process as a nano spoke, prints the sync events and the status
// every 10 s. Plain http works only for loopback and private hosts and needs
// -sync-insecure.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/tokibase/tokibase/embed"
)

func main() {
	hub := flag.String("sync-hub", "", "hub URL: run as a sync spoke (profile nano)")
	code := flag.String("sync-code", "", "one-time enrollment code (first run only)")
	interval := flag.String("sync-interval", "", "idle sync interval, for example 30s")
	insecure := flag.Bool("sync-insecure", false, "allow a plain http hub on a private network (TOKI_SYNC_INSECURE=1)")
	metered := flag.Bool("sync-metered", false, "report a metered connection (push only, 5 minute interval)")
	flag.Parse()
	dir := "./toki_data"
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}
	opts := embed.Options{DataDir: dir}
	if *hub != "" {
		opts.Sync = &embed.SyncOptions{HubURL: *hub, Interval: *interval}
		if *insecure {
			opts.Env = map[string]string{"TOKI_SYNC_INSECURE": "1"}
		}
	}
	inst, err := embed.Start(opts)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("listening on", inst.URL())

	if *hub != "" {
		s := inst.Sync()
		s.SetConditions(true, *metered, false, false)
		cancelEv := s.OnEvent(func(ev []byte) { fmt.Println("sync event:", string(ev)) })
		defer cancelEv()
		if *code != "" {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err := s.Enroll(ctx, "", *code)
			cancel()
			if err != nil {
				log.Fatal("enroll: ", err)
			}
			fmt.Println("enrolled at", *hub)
		}
		go func() {
			for range time.Tick(10 * time.Second) {
				if st, err := s.Status(); err == nil {
					fmt.Println("sync status:", string(st))
				} else {
					fmt.Println("sync status:", err)
				}
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	if err := inst.Stop(context.Background()); err != nil {
		log.Fatal(err)
	}
}
