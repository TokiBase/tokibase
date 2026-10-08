package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	suEmail = "loadtest@local.test"
	suPass  = "load-test-pass-0123456789"
	nUsers  = 50
	userPw  = "load-user-pass-0123456789"
	scratch = "load_items"
)

type API struct {
	Base string
	C    *http.Client
	SU   string // superuser token
}

func NewAPI(base string) *API { return &API{Base: base, C: &http.Client{Timeout: 120 * time.Second}} }

// Call sends JSON and decodes the response into out (may be nil). Returns status.
func (a *API) Call(method, path, token string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.Base+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := a.C.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		json.Unmarshal(raw, out)
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s -> %d %s", method, path, resp.StatusCode, trunc(string(raw), 300))
	}
	return resp.StatusCode, nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (a *API) LoginSU() error {
	var r struct{ Token string }
	if _, err := a.Call("POST", "/api/collections/_superusers/auth-with-password", "", map[string]string{"identity": suEmail, "password": suPass}, &r); err != nil {
		return err
	}
	a.SU = r.Token
	return nil
}

func (a *API) Count(coll string) (int64, error) {
	var r struct{ TotalItems int64 }
	_, err := a.Call("GET", "/api/collections/"+coll+"/records?perPage=1", a.SU, nil, &r)
	return r.TotalItems, err
}

// FGRColl is a chosen large collection of the real-data copy.
type FGRColl struct {
	Name    string `json:"name"`
	Records int64  `json:"records"`
	Created bool   `json:"has_created"`
	Expand  string `json:"expand,omitempty"`
	RuleSet bool   `json:"rule_set_to_auth"`
}

type Dataset struct {
	Collections int       `json:"collections"`
	Base, Auth  int       `json:"-"`
	TotalRecs   int64     `json:"total_records"`
	Top         []FGRColl `json:"top"`
}

// ensureSuperuser must run while the server is stopped.
func ensureSuperuser(bin, dir string) error {
	cmd := exec.Command(bin, "superuser", "upsert", suEmail, suPass, "--dir", dir)
	cmd.Env = baseEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("superuser upsert: %v: %s", err, out)
	}
	return nil
}

type collMeta struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	System bool   `json:"system"`
	Fields []struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		CollectionID string `json:"collectionId"`
	} `json:"fields"`
}

// Setup prepares the copy: settings, scratch collection, users, top FGR collections.
func (a *API) Setup(log func(string, ...any)) (*Dataset, []string, error) {
	// neutralise anything that could reach outside this host
	patch := map[string]any{
		"smtp":       map[string]any{"enabled": false},
		"s3":         map[string]any{"enabled": false},
		"backups":    map[string]any{"cron": "", "s3": map[string]any{"enabled": false}},
		"rateLimits": map[string]any{"enabled": false},
		"batch":      map[string]any{"enabled": true, "maxRequests": 1000, "timeout": 120, "maxBodySize": 0},
	}
	if _, err := a.Call("PATCH", "/api/settings", a.SU, patch, nil); err != nil {
		return nil, nil, err
	}
	var list struct{ Items []collMeta }
	if _, err := a.Call("GET", "/api/collections?perPage=1000", a.SU, nil, &list); err != nil {
		return nil, nil, err
	}
	ds := &Dataset{}
	var cands []FGRColl
	byID := map[string]string{}
	for _, c := range list.Items {
		byID[c.ID] = c.Name
	}
	meta := map[string]collMeta{}
	for _, c := range list.Items {
		if c.System || strings.HasPrefix(c.Name, "_") || strings.HasPrefix(c.Name, "load_") {
			continue
		}
		ds.Collections++
		n, err := a.Count(c.Name)
		if err != nil {
			log("count %s: %v", c.Name, err)
			continue
		}
		ds.TotalRecs += n
		if c.Type != "base" {
			continue
		}
		meta[c.Name] = c
		cands = append(cands, FGRColl{Name: c.Name, Records: n})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Records > cands[j].Records })
	if len(cands) > 3 {
		cands = cands[:3]
	}
	for i := range cands {
		m := meta[cands[i].Name]
		for _, f := range m.Fields {
			if f.Name == "created" && f.Type == "autodate" {
				cands[i].Created = true
			}
			if f.Type == "relation" && cands[i].Expand == "" && byID[f.CollectionID] != "" {
				cands[i].Expand = f.Name
			}
		}
		// require login for list, so the rule engine is on the path
		if _, err := a.Call("PATCH", "/api/collections/"+m.ID, a.SU, map[string]any{"listRule": "@request.auth.id != ''"}, nil); err != nil {
			log("WARN could not set listRule on %s: %v", m.Name, err)
		} else {
			cands[i].RuleSet = true
		}
	}
	ds.Top = cands

	// auth collection + users
	if _, err := a.Call("POST", "/api/collections", a.SU, map[string]any{"name": "load_users", "type": "auth"}, nil); err != nil && !strings.Contains(err.Error(), "400") {
		return nil, nil, err
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var uerr error
	var umu sync.Mutex
	for i := 0; i < nUsers; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			body := map[string]any{"email": fmt.Sprintf("u%02d@load.test", i), "password": userPw, "passwordConfirm": userPw, "verified": true}
			if _, err := a.Call("POST", "/api/collections/load_users/records", a.SU, body, nil); err != nil && !strings.Contains(err.Error(), "400") {
				umu.Lock()
				uerr = err
				umu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if uerr != nil {
		return nil, nil, uerr
	}
	tokens, err := a.userTokens()
	if err != nil {
		return nil, nil, err
	}
	if err := a.ResetItems(); err != nil {
		return nil, nil, err
	}
	return ds, tokens, nil
}

func (a *API) userTokens() ([]string, error) {
	toks := make([]string, nUsers)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var first error
	var mu sync.Mutex
	for i := 0; i < nUsers; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			var r struct{ Token string }
			if _, err := a.Call("POST", "/api/collections/load_users/auth-with-password", "", map[string]string{"identity": fmt.Sprintf("u%02d@load.test", i), "password": userPw}, &r); err != nil {
				mu.Lock()
				first = err
				mu.Unlock()
				return
			}
			toks[i] = r.Token
		}(i)
	}
	wg.Wait()
	return toks, first
}

// Tokens re-authenticates all users (used after a server restart is not needed: JWTs stay valid; kept for token expiry).
func (a *API) Tokens() ([]string, error) { return a.userTokens() }

// ResetItems drops and recreates the scratch collection (empty).
func (a *API) ResetItems() error {
	a.Call("DELETE", "/api/collections/"+scratch, a.SU, nil, nil)
	rule := "@request.auth.id != ''"
	body := map[string]any{
		"name": scratch, "type": "base",
		"listRule": rule, "viewRule": rule, "createRule": rule,
		"fields": []map[string]any{
			{"name": "title", "type": "text"},
			{"name": "n", "type": "number"},
			{"name": "tag", "type": "text"},
			{"name": "body", "type": "text"},
			{"name": "created", "type": "autodate", "onCreate": true},
		},
		"indexes": []string{"CREATE INDEX idx_load_items_tag ON " + scratch + " (tag)"},
	}
	_, err := a.Call("POST", "/api/collections", a.SU, body, nil)
	return err
}

const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"

func randStr(r *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alnum[r.Intn(len(alnum))]
	}
	return string(b)
}

func itemBody(r *rand.Rand, title string) []byte {
	b, _ := json.Marshal(map[string]any{"title": title, "n": r.Intn(1000000), "tag": fmt.Sprintf("t%d", r.Intn(100)), "body": randStr(r, 200)})
	return b
}
