package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Realtime event titles: rt<kind>-<seq>-<unixnano>. kind b=burst, p=paced, s=soak.
func rtTitle(kind byte, seq int) string {
	return fmt.Sprintf("rt%c-%d-%d", kind, seq, time.Now().UnixNano())
}

func kindIdx(k byte) int {
	switch k {
	case 'b':
		return 0
	case 'p':
		return 1
	}
	return 2
}

type sseConn struct {
	clientID string
	resp     *http.Response
	cancel   context.CancelFunc
	counts   [3]atomic.Int64
	closed   atomic.Bool
	dead     atomic.Bool // reader ended without us closing
}

func (c *sseConn) Close() { c.closed.Store(true); c.cancel(); c.resp.Body.Close() }

type evFn func(c *sseConn, kind byte, seq int, lat time.Duration)

func sseClient() *http.Client {
	return &http.Client{Transport: &http.Transport{MaxIdleConns: 0, DisableKeepAlives: false, MaxConnsPerHost: 0}}
}

// connectSSE opens /api/realtime, waits for PB_CONNECT and subscribes to the scratch collection.
func connectSSE(ctx context.Context, cl *http.Client, base, token string, on evFn) (*sseConn, error) {
	cctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(cctx, "GET", base+"/api/realtime", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := cl.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("sse status %d", resp.StatusCode)
	}
	c := &sseConn{resp: resp, cancel: cancel}
	rd := bufio.NewReaderSize(resp.Body, 8192)
	ready := make(chan error, 1)
	go func() {
		var ev string
		var data []byte
		sent := false
		for {
			ln, err := rd.ReadBytes('\n')
			now := time.Now()
			if err != nil {
				if !sent {
					ready <- err
				}
				if !c.closed.Load() {
					c.dead.Store(true)
				}
				return
			}
			ln = bytes.TrimRight(ln, "\r\n")
			switch {
			case bytes.HasPrefix(ln, []byte("event:")):
				ev = strings.TrimSpace(string(ln[6:]))
			case bytes.HasPrefix(ln, []byte("data:")):
				data = append(data[:0], bytes.TrimSpace(ln[5:])...)
			case len(ln) == 0:
				if ev == "PB_CONNECT" && !sent {
					var d struct {
						ClientID string `json:"clientId"`
					}
					json.Unmarshal(data, &d)
					c.clientID = d.ClientID
					sent = true
					ready <- nil
				} else if ev == scratch {
					if i := bytes.Index(data, []byte(`"title":"rt`)); i >= 0 {
						t := string(data[i+9:])
						if j := strings.IndexByte(t, '"'); j > 0 {
							p := strings.Split(t[:j], "-")
							if len(p) == 3 && len(p[0]) == 3 {
								seq, _ := strconv.Atoi(p[1])
								ns, _ := strconv.ParseInt(p[2], 10, 64)
								k := p[0][2]
								c.counts[kindIdx(k)].Add(1)
								if on != nil {
									on(c, k, seq, now.Sub(time.Unix(0, ns)))
								}
							}
						}
					}
				}
				ev, data = "", data[:0]
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			return nil, err
		}
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		cancel()
		return nil, fmt.Errorf("no PB_CONNECT in 30s")
	}
	body, _ := json.Marshal(map[string]any{"clientId": c.clientID, "subscriptions": []string{scratch}})
	sreq, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/realtime", bytes.NewReader(body))
	sreq.Header.Set("Content-Type", "application/json")
	sreq.Header.Set("Authorization", token)
	sresp, err := cl.Do(sreq)
	if err != nil {
		c.Close()
		return nil, err
	}
	sresp.Body.Close()
	if sresp.StatusCode != 204 {
		c.Close()
		return nil, fmt.Errorf("subscribe status %d", sresp.StatusCode)
	}
	return c, nil
}

type latBag struct {
	mu  sync.Mutex
	lat [3][]int64
}

func (l *latBag) add(k byte, d time.Duration) {
	i := kindIdx(k)
	l.mu.Lock()
	l.lat[i] = append(l.lat[i], d.Microseconds())
	l.mu.Unlock()
}
