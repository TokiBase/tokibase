// Package mobile is a gomobile friendly facade over package embed. It uses
// only types gomobile can bind: string, int, []byte, error, structs with
// such fields and interfaces with simple methods.
//
// Build (see mobile/build.sh and docs/EMBED.md):
//
//	gomobile bind -target android -androidapi 24 -tags "<nano tags>" ./mobile
//
// The nano tag set comes from profiles.txt; nothing here is hard coded to it.
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/tokibase/tokibase/embed"
)

// EventCallback receives realtime events (JSON bytes). It is called on a
// background goroutine, so the host must hop to its UI thread itself.
type EventCallback interface {
	OnEvent(data []byte)
}

// Response is the result of Handle.Call.
type Response struct {
	Status      int
	HeadersJSON string
	Body        []byte
}

// Handle is a running TokiBase instance.
type Handle struct {
	inst *embed.Instance

	mu     sync.Mutex
	nextID int
	subs   map[int]func()
}

// Start starts an instance. listen defaults to "127.0.0.1:0" when empty,
// "-" disables the TCP listener (Call and Subscribe only). envJSON is an
// optional JSON object of string values applied as environment variables;
// the keys "profile", "hooksDir", "logLevel" are reserved and select the
// matching embed.Options field instead of being exported.
func Start(dataDir, listen, envJSON string) (*Handle, error) {
	opts := embed.Options{DataDir: dataDir, Listen: listen}
	if envJSON != "" {
		env := map[string]string{}
		if err := json.Unmarshal([]byte(envJSON), &env); err != nil {
			return nil, errors.New("mobile: envJSON must be a JSON object of strings: " + err.Error())
		}
		for _, k := range []string{"profile", "hooksDir", "logLevel"} {
			if v, ok := env[k]; ok {
				switch k {
				case "profile":
					opts.Profile = v
				case "hooksDir":
					opts.HooksDir = v
				case "logLevel":
					opts.LogLevel = v
				}
				delete(env, k)
			}
		}
		opts.Env = env
	}
	inst, err := embed.Start(opts)
	if err != nil {
		return nil, err
	}
	return &Handle{inst: inst, subs: map[int]func(){}}, nil
}

// URL is the loopback base URL ("" when listen was "-").
func (h *Handle) URL() string { return h.inst.URL() }

// Call runs an in-process HTTP request. headersJSON is an optional JSON
// object of string values.
func (h *Handle) Call(method, path, headersJSON string, body []byte) (*Response, error) {
	var headers map[string]string
	if headersJSON != "" {
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			return nil, errors.New("mobile: headersJSON must be a JSON object of strings: " + err.Error())
		}
	}
	status, rh, rb, err := h.inst.Call(method, path, headers, body)
	if err != nil {
		return nil, err
	}
	hj, _ := json.Marshal(rh)
	return &Response{Status: status, HeadersJSON: string(hj), Body: rb}, nil
}

// Superuser creates or updates the superuser (first run setup).
func (h *Handle) Superuser(email, password string) error {
	return h.inst.Superuser(email, password)
}

// Subscribe registers cb for topic (for example "posts/*") and returns an
// id for Unsubscribe.
func (h *Handle) Subscribe(topic string, cb EventCallback) (int, error) {
	return h.register(h.inst.Subscribe(topic, cb.OnEvent)), nil
}

// SubscribeAs is Subscribe with the access of an auth token's record.
func (h *Handle) SubscribeAs(token, topic string, cb EventCallback) (int, error) {
	cancel, err := h.inst.SubscribeAs(token, topic, cb.OnEvent)
	if err != nil {
		return 0, err
	}
	return h.register(cancel), nil
}

func (h *Handle) register(cancel func()) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	h.subs[h.nextID] = cancel
	return h.nextID
}

// Unsubscribe cancels a subscription; unknown ids are ignored.
func (h *Handle) Unsubscribe(id int) {
	h.mu.Lock()
	cancel := h.subs[id]
	delete(h.subs, id)
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Stop shuts the instance down (waits up to 30 seconds).
func (h *Handle) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.inst.Stop(ctx)
}
