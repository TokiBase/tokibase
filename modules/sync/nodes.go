//go:build !no_sync

package sync

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/types"
)

// NodesCollection is the hub's system collection of enrolled devices.
const NodesCollection = "_sync_nodes"

// Node status values.
const (
	NodePending     = "pending"
	NodeActive      = "active"
	NodeStale       = "stale"
	NodeRebootstrap = "rebootstrap"
	NodeRevoked     = "revoked"
)

// Profiles a node can have.
var nodeProfiles = []string{"nano", "edge", "solo", "team", "cluster"}

// EnrollTTL is how long a one-time enrollment code is valid.
const EnrollTTL = 24 * time.Hour

var nodeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// EnsureNodesCollection creates the `_sync_nodes` system collection (superusers
// only, rules null). It is idempotent.
func EnsureNodesCollection(app core.App) error {
	if c, _ := app.FindCollectionByNameOrId(NodesCollection); c != nil {
		changed := false
		for _, name := range []string{"sig_ts_floor", "max_skew_ms"} { // created by an older build
			if c.Fields.GetByName(name) == nil {
				c.Fields.Add(&core.NumberField{Name: name})
				changed = true
			}
		}
		if c.Fields.GetByName("key_report") == nil {
			c.Fields.Add(&core.JSONField{Name: "key_report", MaxSize: 65536, Hidden: true})
			changed = true
		}
		if changed {
			return app.Save(c)
		}
		return nil
	}
	c := core.NewBaseCollection(NodesCollection)
	c.System = true
	c.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 63},
		&core.SelectField{Name: "profile", MaxSelect: 1, Values: nodeProfiles},
		&core.SelectField{Name: "status", Required: true, MaxSelect: 1, Values: []string{NodePending, NodeActive, NodeStale, NodeRebootstrap, NodeRevoked}},
		&core.TextField{Name: "enroll_hash", Hidden: true, Max: 128},
		&core.DateField{Name: "enroll_expires"},
		&core.TextField{Name: "pubkey", Max: 128},
		&core.TextField{Name: "kx_pubkey", Max: 128},
		&core.TextField{Name: "cert_serial", Max: 64},
		&core.DateField{Name: "cert_expires"},
		&core.JSONField{Name: "params", MaxSize: 16384},
		&core.TextField{Name: "actor_collection", Max: 100},
		&core.TextField{Name: "actor_record", Max: 100},
		&core.NumberField{Name: "pulled_seq"},
		&core.NumberField{Name: "pushed_origin_seq"},
		&core.NumberField{Name: "schema_version"},
		&core.NumberField{Name: "clock_offset_ms"},
		// sig_ts_floor is the highest handshake ts accepted so far (unix ms);
		// older or equal timestamps are refused, so a restart can not replay.
		&core.NumberField{Name: "sig_ts_floor"},
		// max_skew_ms is the largest clock skew seen from the node (|offset| at a
		// handshake or a push), a metric of /api/health
		&core.NumberField{Name: "max_skew_ms"},
		&core.DateField{Name: "last_seen"},
		&core.DateField{Name: "revoked_at"},
		&core.TextField{Name: "app_version", Max: 64},
		// key_report: per encrypted collection, the newest data key version handed to the node
		// and the versions its unsent changes use (retire guard, see RetireBlockers)
		&core.JSONField{Name: "key_report", MaxSize: 65536, Hidden: true},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_sync_nodes_name", true, "[[name]]", "")
	c.AddIndex("idx_sync_nodes_status", false, "[[status]]", "")
	return app.Save(c)
}

// NewEnrollCode returns a code of 8 groups of 4 base32 characters.
func NewEnrollCode() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b) // 32 chars
	parts := make([]string, 0, 8)
	for i := 0; i < len(s); i += 4 {
		parts = append(parts, s[i:i+4])
	}
	return strings.Join(parts, "-"), nil
}

// HashEnrollCode is the stored form of a code: sha256 hex of the code without
// separators, upper case.
func HashEnrollCode(code string) string {
	n := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	h := sha256.Sum256([]byte(n))
	return hex.EncodeToString(h[:])
}

// EnrollOptions are the inputs of CreateEnrollment.
type EnrollOptions struct {
	Name    string
	Profile string
	Params  map[string]string
	// Actor is the service actor "collection/id" (optional).
	Actor string
	// AllowSuperuserActor is required for a superuser as service actor: it
	// bypasses every collection rule for all changes the node pushes as "node".
	AllowSuperuserActor bool
	// CLI marks the audit entry as made by an operator on the command line.
	CLI bool
}

// CreateEnrollment adds a `pending` node and returns it with the one-time code.
// The code is only returned here; the hub stores its hash.
func CreateEnrollment(app core.App, o EnrollOptions) (*core.Record, string, error) {
	name := strings.TrimSpace(o.Name)
	if !nodeNameRe.MatchString(name) {
		return nil, "", errors.New("node name must be 1-63 chars of letters, digits, '.', '_' or '-'")
	}
	ok := false
	for _, p := range nodeProfiles {
		if p == o.Profile {
			ok = true
		}
	}
	if !ok {
		return nil, "", fmt.Errorf("profile must be one of %s", strings.Join(nodeProfiles, ", "))
	}
	col, err := app.FindCollectionByNameOrId(NodesCollection)
	if err != nil {
		return nil, "", errf("%s not found (is TOKI_SYNC_ROLE=hub?)", NodesCollection)
	}
	if ex, _ := app.FindFirstRecordByFilter(col, "name={:n}", dbx.Params{"n": name}); ex != nil {
		return nil, "", fmt.Errorf("a node named %q already exists", name)
	}
	actorKind := ""
	rec := core.NewRecord(col)
	rec.Set("name", name)
	rec.Set("profile", o.Profile)
	rec.Set("status", NodePending)
	if len(o.Params) > 0 {
		rec.Set("params", o.Params)
	}
	if o.Actor != "" {
		c, id, found := strings.Cut(o.Actor, "/")
		if !found || c == "" || id == "" {
			return nil, "", errors.New("actor must be <collection>/<record id>")
		}
		if isSuperusersRef(app, c) && !o.AllowSuperuserActor {
			return nil, "", errors.New("a superuser as service actor bypasses all rules for everything this node pushes as itself; repeat with --allow-superuser-actor if that is intended")
		}
		rec.Set("actor_collection", c)
		rec.Set("actor_record", id)
		actorKind = "regular"
		if isSuperusersRef(app, c) {
			actorKind = kernel.AuthKindSuperuser
		}
	}
	code, err := NewEnrollCode()
	if err != nil {
		return nil, "", err
	}
	rec.Set("enroll_hash", HashEnrollCode(code))
	rec.Set("enroll_expires", types.NowDateTime().Add(EnrollTTL))
	if err := app.Save(rec); err != nil {
		return nil, "", err
	}
	emit(AuditNodeEnroll, NodesCollection, rec.Id, map[string]any{
		"name": name, "profile": o.Profile, "stage": "created", "cli": o.CLI,
		"service_actor": o.Actor, "service_actor_kind": actorKind, "warning": warnIf(actorKind == kernel.AuthKindSuperuser, "superuser service actor: rules are bypassed"),
	})
	return rec, code, nil
}

// FindNode looks a node up by id or name.
func FindNode(app core.App, ref string) (*core.Record, error) {
	if r, err := app.FindRecordById(NodesCollection, ref); err == nil {
		return r, nil
	}
	return app.FindFirstRecordByFilter(NodesCollection, "name={:n}", dbx.Params{"n": ref})
}

// RevokeNode sets a node `revoked`. Every node request checks the status, so
// session tokens stop working at once. Revoking twice is a no-op.
//
// The write is a targeted UPDATE keyed by the node NAME (unique and stable:
// enrollment renames the row id from the pending id to the key-derived id, the
// name never changes), never a Save of a previously loaded record, so it can
// neither miss a renamed row nor overwrite a concurrent change. It fails when
// no row was changed and the node is not revoked already.
func RevokeNode(app core.App, ref string, cli bool) (*core.Record, error) {
	rec, err := FindNode(app, ref)
	if err != nil {
		return nil, fmt.Errorf("node %q not found", ref)
	}
	if rec.GetString("status") == NodeRevoked {
		return rec, nil
	}
	name := rec.GetString("name")
	now := types.NowDateTime().String()
	res, err := app.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET status={:r}, revoked_at={:t}, updated={:t}, enroll_hash='' WHERE name={:n} AND status!={:r}").
		Bind(dbx.Params{"r": NodeRevoked, "t": now, "n": name}).Execute()
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	cur, ferr := FindNode(app, name)
	if ferr != nil {
		return nil, fmt.Errorf("node %q not found", ref)
	}
	if n == 0 {
		if cur.GetString("status") == NodeRevoked {
			return cur, nil // revoked by someone else in the meantime
		}
		return nil, fmt.Errorf("node %q was not revoked (no row changed), try again", ref)
	}
	emit(AuditNodeRevoke, NodesCollection, cur.Id, map[string]any{"name": name, "cli": cli})
	retireReservations(app, cur.Id) // PR8: the ranges of a revoked node are dead (design §7.5)
	revokeNodeCerts(app, cur.Id)
	return cur, nil
}

// constEq compares two strings in constant time.
func constEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// isSuperusersRef tells whether a collection reference is the superusers collection.
func isSuperusersRef(app core.App, ref string) bool {
	if ref == core.CollectionNameSuperusers {
		return true
	}
	c, err := app.FindCachedCollectionByNameOrId(core.CollectionNameSuperusers)
	return err == nil && c != nil && c.Id == ref
}

func warnIf(cond bool, msg string) string {
	if cond {
		return msg
	}
	return ""
}
