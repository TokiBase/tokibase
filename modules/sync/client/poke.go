//go:build !no_sync

package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tokibase/tokibase/modules/sync/proto"
)

// pokeLoop keeps one SSE subscription to the hub topic "@sync" while the node
// is online. A poke only asks the loop for a cycle (the payload is {"seq":N}
// and is not used). Without realtime (proxy, mobile background) the interval
// timer and Pull(wait) still work.
func (c *Client) pokeLoop(ctx context.Context) {
	wait := 2 * time.Second
	for ctx.Err() == nil {
		c.loop.mu.Lock()
		ok := c.loop.cond.Online && !c.loop.paused
		c.loop.mu.Unlock()
		if !ok || !c.tokenValid() {
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		start := time.Now()
		err := c.runSSE(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil && c.o.Logger != nil {
			c.o.Logger.Debug("sync: realtime poke connection ended", "error", err)
		}
		if time.Since(start) > time.Minute {
			wait = 2 * time.Second
		} else {
			wait = min(wait*2, time.Minute)
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runSSE connects, subscribes and reads until the stream ends.
func (c *Client) runSSE(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/realtime", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	hc := &http.Client{Transport: c.http.Transport} // no overall timeout for a stream
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("realtime: " + res.Status)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	var event string
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if event != "" {
				if err := c.onSSE(ctx, event, data.Bytes()); err != nil {
					return err
				}
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(line[len("data:"):]))
		}
	}
	return sc.Err()
}

func (c *Client) onSSE(ctx context.Context, event string, data []byte) error {
	switch event {
	case "PB_CONNECT":
		var d struct {
			ClientID string `json:"clientId"`
		}
		if err := json.Unmarshal(data, &d); err != nil || d.ClientID == "" {
			return errors.New("realtime: invalid connect message")
		}
		body, _ := json.Marshal(map[string]any{"clientId": d.ClientID, "subscriptions": []string{proto.Topic}})
		_, _, err := c.do(ctx, http.MethodPost, "/api/realtime", map[string]string{"Authorization": "Bearer " + c.Token()}, body)
		if err == nil {
			c.Kick() // catch up on whatever was missed while the stream was down
		}
		return err
	case proto.Topic:
		c.Kick()
	}
	return nil
}
