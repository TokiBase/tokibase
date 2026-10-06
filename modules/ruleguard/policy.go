// Package ruleguard makes public ("") API rules explicit and visible.
//
// In PocketBase an empty string rule means "public to anyone" while null
// means "superusers only". ruleguard keeps an allowlist of the rules that are
// intentionally public and reports every other empty rule, without changing
// the REST contract or the collection JSON shape.
package ruleguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// FileName is the policy file name, relative to the app DataDir.
const FileName = "ruleguard.json"

// Policy values.
const (
	PolicyWarn   = "warn"
	PolicyStrict = "strict"
	PolicyOff    = "off"
)

// Rule kinds.
const (
	KindList   = "list"
	KindView   = "view"
	KindCreate = "create"
	KindUpdate = "update"
	KindDelete = "delete"
	KindManage = "manage"
	KindAuth   = "auth"
)

// Kinds lists every valid rule kind.
var Kinds = []string{KindList, KindView, KindCreate, KindUpdate, KindDelete, KindManage, KindAuth}

// ValidKind reports whether kind is a known rule kind.
func ValidKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Policy is the content of pb_data/ruleguard.json.
type Policy struct {
	Policy string              `json:"policy"`
	Public map[string][]string `json:"public"`
}

// Default returns the policy used when the file is missing.
func Default() Policy {
	return Policy{Policy: PolicyWarn, Public: map[string][]string{}}
}

// Allowed reports whether the rule kind of the collection is allowlisted.
func (p Policy) Allowed(collection, kind string) bool {
	for _, k := range p.Public[collection] {
		if k == kind {
			return true
		}
	}
	return false
}

// Allow adds the kinds to the allowlist of the collection (idempotent).
func (p *Policy) Allow(collection string, kinds ...string) {
	if p.Public == nil {
		p.Public = map[string][]string{}
	}
	for _, kind := range kinds {
		if !p.Allowed(collection, kind) {
			p.Public[collection] = append(p.Public[collection], kind)
		}
	}
	order := map[string]int{}
	for i, k := range Kinds {
		order[k] = i
	}
	sort.Slice(p.Public[collection], func(i, j int) bool {
		return order[p.Public[collection][i]] < order[p.Public[collection][j]]
	})
}

// Validate checks the policy value and the allowlisted kinds.
func (p Policy) Validate() error {
	switch p.Policy {
	case PolicyWarn, PolicyStrict, PolicyOff:
	default:
		return fmt.Errorf("ruleguard: invalid policy %q (expected warn, strict or off)", p.Policy)
	}
	for name, kinds := range p.Public {
		for _, k := range kinds {
			if !ValidKind(k) {
				return fmt.Errorf("ruleguard: invalid rule kind %q for collection %q", k, name)
			}
		}
	}
	return nil
}

// Path returns the policy file path for the data directory.
func Path(dataDir string) string {
	return filepath.Join(dataDir, FileName)
}

// Load reads <dataDir>/ruleguard.json. A missing file yields Default().
func Load(dataDir string) (Policy, error) {
	raw, err := os.ReadFile(Path(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Policy{}, err
	}

	p := Default()
	if err := json.Unmarshal(raw, &p); err != nil {
		return Policy{}, fmt.Errorf("ruleguard: invalid %s: %w", FileName, err)
	}
	if p.Policy == "" {
		p.Policy = PolicyWarn
	}
	if p.Public == nil {
		p.Public = map[string][]string{}
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Save writes the policy to <dataDir>/ruleguard.json (atomically).
func Save(dataDir string, p Policy) error {
	if p.Policy == "" {
		p.Policy = PolicyWarn
	}
	if p.Public == nil {
		p.Public = map[string][]string{}
	}
	if err := p.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, os.ModePerm); err != nil {
		return err
	}
	tmp := Path(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dataDir))
}
