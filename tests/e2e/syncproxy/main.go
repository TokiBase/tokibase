// Command syncproxy is a TCP proxy with a control port that simulates network
// loss between a sync spoke and its hub (tests/e2e/parking.sh, docs/SYNC_DESIGN.md
// §9.1).
//
//	syncproxy -target 127.0.0.1:8090 [-listen 127.0.0.1:0] [-control 127.0.0.1:0]
//
// It prints one JSON line on stdout once it is up:
//
//	{"listen":"127.0.0.1:41000","control":"127.0.0.1:41001"}
//
// The control port answers GET/POST /off (close every open connection and stop
// accepting: clients get "connection refused"), /on (accept again on the same
// port) and /status ({"up":true,"conns":3,"accepted":17}). It runs until it
// receives SIGINT/SIGTERM.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
)

type proxy struct {
	target string

	mu       sync.Mutex
	ln       net.Listener
	addr     string
	conns    map[net.Conn]struct{}
	accepted atomic.Int64
}

// on starts accepting (the first call picks the port, later ones reuse it).
func (p *proxy) on() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}
	p.ln, p.addr = ln, ln.Addr().String()
	go p.accept(ln)
	return nil
}

// off refuses new connections and drops the open ones.
func (p *proxy) off() {
	p.mu.Lock()
	ln := p.ln
	p.ln = nil
	conns := p.conns
	p.conns = map[net.Conn]struct{}{}
	p.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for c := range conns {
		_ = c.Close()
	}
}

func (p *proxy) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)
		go p.serve(c)
	}
}

func (p *proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil { // switched off between accept and here
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *proxy) serve(c net.Conn) {
	if !p.track(c) {
		_ = c.Close()
		return
	}
	up, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	if !p.track(up) {
		_ = c.Close()
		_ = up.Close()
		return
	}
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		_ = src.Close()
		done <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	<-done
	<-done
	p.mu.Lock()
	delete(p.conns, c)
	delete(p.conns, up)
	p.mu.Unlock()
}

func main() {
	target := flag.String("target", "", "host:port of the hub (required)")
	listen := flag.String("listen", "127.0.0.1:0", "address clients connect to")
	control := flag.String("control", "127.0.0.1:0", "address of the control port")
	flag.Parse()
	if *target == "" {
		log.Fatal("syncproxy: -target is required")
	}
	p := &proxy{target: *target, addr: *listen, conns: map[net.Conn]struct{}{}}
	if err := p.on(); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter) {
		p.mu.Lock()
		st := map[string]any{"up": p.ln != nil, "conns": len(p.conns), "accepted": p.accepted.Load()}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}
	mux.HandleFunc("/off", func(w http.ResponseWriter, r *http.Request) { p.off(); reply(w) })
	mux.HandleFunc("/on", func(w http.ResponseWriter, r *http.Request) {
		if err := p.on(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reply(w)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { reply(w) })
	cln, err := net.Listen("tcp", *control)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stdout, "{\"listen\":%q,\"control\":%q}\n", p.addr, cln.Addr().String())
	go func() { _ = http.Serve(cln, mux) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	p.off()
}
