//go:build no_sync

// Command parking needs the sync module; this stub keeps `go build -tags no_sync ./...` green.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "parking: built with no_sync, nothing to run")
	os.Exit(2)
}
