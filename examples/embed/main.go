// Command embed is a minimal host program for package embed (size probe and
// usage example): it serves an instance until interrupted.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/tokibase/tokibase/embed"
)

func main() {
	dir := "./toki_data"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	inst, err := embed.Start(embed.Options{DataDir: dir})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("listening on", inst.URL())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	if err := inst.Stop(context.Background()); err != nil {
		log.Fatal(err)
	}
}
