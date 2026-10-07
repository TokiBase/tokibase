//go:build !no_mcp

package mcp

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/security"
)

// DefaultRatePerMin is the default per-agent rate limit.
const DefaultRatePerMin = 120

const (
	hookId = "__tokiMCP__"
	keyPfx = "tka_"
)

// Role is the privilege level of an agent.
type Role string

const (
	RoleReader   Role = "reader"
	RoleWriter   Role = "writer"
	RoleOperator Role = "operator"
)

// Roles lists the valid roles, lowest privilege first.
var Roles = []string{string(RoleReader), string(RoleWriter), string(RoleOperator)}

func (r Role) rank() int {
	switch r {
	case RoleReader:
		return 1
	case RoleWriter:
		return 2
	case RoleOperator:
		return 3
	}
	return 0
}

// ValidRole reports whether s is a known role.
func ValidRole(s string) bool { return Role(s).rank() > 0 }

// Agent is a resolved `_agents` record.
type Agent struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Role        Role     `json:"role"`
	Collections []string `json:"collections"` // empty = all
	RatePerMin  int      `json:"rate_per_min"`
	Enabled     bool     `json:"enabled"`
	Created     string   `json:"created"`

	// invalid is set when the stored `collections` allowlist can not be parsed:
	// the agent then fails closed (every call is denied).
	invalid bool
}

// allows reports whether the agent allowlist covers the collection name.
func (a *Agent) allows(collection string) bool {
	if a.invalid {
		return false
	}
	if len(a.Collections) == 0 {
		return true
	}
	for _, c := range a.Collections {
		if c == collection {
			return true
		}
	}
	return false
}

func agentFromRecord(r *kernel.Record) *Agent {
	a := &Agent{
		ID: r.Id, Name: r.GetString("name"), Role: Role(r.GetString("role")),
		RatePerMin: r.GetInt("rate_per_min"), Enabled: r.GetBool("enabled"),
		Created: r.GetString("created"),
	}
	if err := r.UnmarshalJSONField("collections", &a.Collections); err != nil {
		a.Collections, a.invalid = nil, true
	}
	if a.RatePerMin <= 0 {
		a.RatePerMin = DefaultRatePerMin
	}
	return a
}

// Register creates the `_agents` collection (main DB) at bootstrap when missing.
func Register(app core.App) {
	init := func() {
		if err := EnsureCollection(app); err != nil {
			app.Logger().Error("mcp: failed to initialize the _agents collection", "error", err)
		}
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			return nil
		},
	})
}

// EnsureCollection creates `_agents` if it does not exist (superuser-only rules).
func EnsureCollection(app kernel.App) error {
	if _, err := app.FindCollectionByNameOrId(CollectionName); err == nil {
		return nil
	}
	c := kernel.NewBaseCollection(CollectionName)
	c.System = true
	c.Fields.Add(
		&kernel.TextField{Name: "name", Required: true, Max: 64},
		&kernel.TextField{Name: "key_hash", Required: true, Hidden: true},
		&kernel.SelectField{Name: "role", Required: true, Values: Roles, MaxSelect: 1},
		&kernel.JSONField{Name: "collections"},
		&kernel.NumberField{Name: "rate_per_min"},
		&kernel.BoolField{Name: "enabled"},
		&kernel.AutodateField{Name: "created", OnCreate: true},
		&kernel.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_agents_name", true, "name", "")
	c.AddIndex("idx_agents_key_hash", true, "key_hash", "")
	if err := app.Save(c); err != nil {
		// another process may have created it concurrently
		if _, e2 := app.FindCollectionByNameOrId(CollectionName); e2 == nil {
			return nil
		}
		return err
	}
	return nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// HashKey returns the stored form of an API key.
func HashKey(key string) string { return security.SHA256(key) }

// CreateAgent registers a new agent and returns its API key, which is never
// stored in clear text and can not be shown again.
func CreateAgent(app kernel.App, name string, role Role, collections []string, ratePerMin int) (*Agent, string, error) {
	if !nameRe.MatchString(name) {
		return nil, "", fmt.Errorf("invalid agent name %q (1-63 chars of a-z 0-9 _ . -, starting with a letter or digit)", name)
	}
	if role.rank() == 0 {
		return nil, "", fmt.Errorf("invalid role %q (expected one of %s)", role, strings.Join(Roles, ", "))
	}
	if ratePerMin <= 0 {
		ratePerMin = DefaultRatePerMin
	}
	cols := make([]string, 0, len(collections))
	for _, c := range collections {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if c == CollectionName {
			return nil, "", fmt.Errorf("collection %q can not be granted to an agent", c)
		}
		if _, err := app.FindCollectionByNameOrId(c); err != nil {
			return nil, "", fmt.Errorf("unknown collection %q", c)
		}
		cols = append(cols, c)
	}
	coll, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil, "", errors.New("the _agents collection is missing (is the mcp module registered?)")
	}
	if _, err := app.FindFirstRecordByData(coll, "name", name); err == nil {
		return nil, "", fmt.Errorf("agent %q already exists", name)
	}
	key := keyPfx + security.RandomString(40)
	rec := kernel.NewRecord(coll)
	rec.Set("name", name)
	rec.Set("key_hash", HashKey(key))
	rec.Set("role", string(role))
	rec.Set("collections", cols)
	rec.Set("rate_per_min", ratePerMin)
	rec.Set("enabled", true)
	if err := app.Save(rec); err != nil {
		return nil, "", err
	}
	return agentFromRecord(rec), key, nil
}

// ListAgents returns all agents ordered by name.
func ListAgents(app kernel.App) ([]*Agent, error) {
	coll, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil, nil
	}
	var recs []*kernel.Record
	if err := app.RecordQuery(coll).All(&recs); err != nil {
		return nil, err
	}
	out := make([]*Agent, 0, len(recs))
	for _, r := range recs {
		out = append(out, agentFromRecord(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RevokeAgent disables an agent (the record is kept so the audit trail still
// resolves its name). Running MCP sessions notice on their next call.
func RevokeAgent(app kernel.App, name string) (*Agent, error) {
	coll, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil, errors.New("the _agents collection is missing")
	}
	rec, err := app.FindFirstRecordByData(coll, "name", name)
	if err != nil {
		return nil, fmt.Errorf("agent %q not found", name)
	}
	rec.Set("enabled", false)
	if err := app.Save(rec); err != nil {
		return nil, err
	}
	return agentFromRecord(rec), nil
}

var (
	errBadKey       = errors.New("invalid agent key")
	errBadAllowlist = errors.New("agent configuration is invalid (collections must be a JSON array of names): all calls are denied")
)

// Authenticate resolves an API key to an enabled agent.
func Authenticate(app kernel.App, key string) (*Agent, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errBadKey
	}
	coll, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil, errBadKey
	}
	rec, err := app.FindFirstRecordByData(coll, "key_hash", HashKey(key))
	if err != nil {
		return nil, errBadKey
	}
	a := agentFromRecord(rec)
	if !a.Enabled {
		return nil, errors.New("agent is revoked")
	}
	if a.invalid {
		app.Logger().Error("mcp: agent has a malformed collections allowlist, denying all calls", "agent", a.Name)
		return nil, errBadAllowlist
	}
	return a, nil
}

// reload re-reads the agent so revocation and role changes apply immediately.
func reload(app kernel.App, id string) (*Agent, error) {
	coll, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil, errors.New("agent registry unavailable")
	}
	rec, err := app.FindRecordById(coll, id)
	if err != nil {
		return nil, errors.New("agent no longer exists")
	}
	a := agentFromRecord(rec)
	if !a.Enabled {
		return nil, errors.New("agent is revoked")
	}
	if a.invalid {
		app.Logger().Error("mcp: agent has a malformed collections allowlist, denying all calls", "agent", a.Name)
		return nil, errBadAllowlist
	}
	return a, nil
}
