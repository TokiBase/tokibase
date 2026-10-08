//go:build !no_sync

package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/pocketbase/dbx"
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Mismatch is a record whose stored row does not match `_sync_meta`.
type Mismatch struct {
	ID     string `json:"id"`
	Reason string `json:"reason"` // no_meta | hash | orphan_meta
}

// CollectionVerify is the verification result of one synced collection.
type CollectionVerify struct {
	Collection string `json:"collection"` // id
	Name       string `json:"name"`
	Records    int    `json:"records"`
	// Digest is sha256 over the sorted (id, hash) pairs computed from the
	// stored records. It is equal on every converged node.
	Digest string `json:"digest"`
	// MetaDigest is the same over `_sync_meta` (what /ack compares).
	MetaDigest string     `json:"meta_digest"`
	Mismatches []Mismatch `json:"mismatches"`
}

// VerifyReport is the output of `toki sync verify`.
type VerifyReport struct {
	Role        string             `json:"role"`
	NodeID      string             `json:"node_id"`
	Pending     int64              `json:"pending"`
	Collections []CollectionVerify `json:"collections"`
	// AgainstHub is set by --against-hub.
	AgainstHub *HubVerify `json:"against_hub,omitempty"`
}

// HubVerify is the answer of the hub to the digests.
type HubVerify struct {
	Checked  bool     `json:"checked"`
	Mismatch []string `json:"mismatch"`
}

// Verify recomputes the canonical hash of every synced record and compares it
// with `_sync_meta`; raw SQL writes (not captured) show up here. It works on a
// stopped or running data dir.
func Verify(app core.App) (*VerifyReport, error) {
	rep := &VerifyReport{Role: string(RoleFromEnv()), Collections: []CollectionVerify{}}
	if !app.HasTable("_changes") || !app.HasTable("_sync_meta") {
		return rep, nil
	}
	if id, ok, _ := (dbState{db: app.DB()}).Get(keyNodeID); ok {
		rep.NodeID = id
	}
	if rep.Role == string(RoleSpoke) {
		_ = app.DB().NewQuery("SELECT COUNT(*) FROM _changes WHERE node={:n} AND status IN ('local','pushed')").
			Bind(dbx.Params{"n": rep.NodeID}).Row(&rep.Pending)
	}
	pc := &policyCache{m: &Module{app: app}}
	cols, err := app.FindAllCollections(core.CollectionTypeBase, core.CollectionTypeAuth)
	if err != nil {
		return nil, err
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
	for _, col := range cols {
		p := pc.For(col)
		if p == nil {
			continue
		}
		cv, err := verifyCollection(app, col, p)
		if err != nil {
			return nil, err
		}
		rep.Collections = append(rep.Collections, *cv)
	}
	return rep, nil
}

func verifyCollection(app core.App, col *core.Collection, p *policy) (*CollectionVerify, error) {
	cv := &CollectionVerify{Collection: col.Id, Name: col.Name, Mismatches: []Mismatch{}}
	meta := map[string][]byte{}
	rows, err := app.DB().NewQuery("SELECT record, hash FROM _sync_meta WHERE collection={:c}").Bind(dbx.Params{"c": col.Id}).Rows()
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var h []byte
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return nil, err
		}
		meta[id] = h
	}
	rows.Close()

	d := newDigester()
	last := ""
	for {
		var recs []*core.Record
		q := app.RecordQuery(col).OrderBy("id ASC").Limit(1000)
		if last != "" {
			q = q.AndWhere(dbx.NewExp("id > {:last}", dbx.Params{"last": last}))
		}
		if err := q.All(&recs); err != nil {
			return nil, err
		}
		if len(recs) == 0 {
			break
		}
		for _, r := range recs {
			h, err := RecordHash(r, p)
			if err != nil {
				return nil, err
			}
			d.add(r.Id, h)
			mh, ok := meta[r.Id]
			switch {
			case !ok:
				cv.Mismatches = append(cv.Mismatches, Mismatch{r.Id, "no_meta"})
			case string(mh) != string(h):
				cv.Mismatches = append(cv.Mismatches, Mismatch{r.Id, "hash"})
			}
			delete(meta, r.Id)
			last = r.Id
		}
	}
	orphans := make([]string, 0, len(meta))
	for id := range meta {
		orphans = append(orphans, id)
	}
	sort.Strings(orphans)
	for _, id := range orphans {
		cv.Mismatches = append(cv.Mismatches, Mismatch{id, "orphan_meta"})
	}
	cv.Records, cv.Digest = d.n, d.sum()
	if cv.MetaDigest, _, err = metaDigest(app.DB(), col.Id); err != nil {
		return nil, err
	}
	return cv, nil
}

func verifyCommand(app core.App) *cobra.Command {
	var asJSON, against bool
	c := &cobra.Command{
		Use:   "verify [--against-hub] [--json]",
		Short: "Per-collection digests; lists records that differ from the sync metadata",
		Long: "Recomputes the canonical hash of every synced record and compares it with _sync_meta " +
			"(raw SQL writes are not captured and show up here). With --against-hub a spoke sends its metadata " +
			"digests to the hub (POST /api/sync/ack) and prints the collections that differ; the hub only compares " +
			"when the spoke has pulled everything, and the spoke should have no pending changes.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := Verify(app)
			if err != nil {
				return err
			}
			if against {
				if err := requireRole(RoleSpoke); err != nil {
					return err
				}
				hv, err := verifyAgainstHub(cmd.Context(), app, rep)
				if err != nil {
					return err
				}
				rep.AgainstHub = hv
			}
			out := cmd.OutOrStdout()
			if asJSON {
				b, _ := json.Marshal(rep)
				fmt.Fprintln(out, string(b))
			} else {
				w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
				fmt.Fprintln(w, "COLLECTION\tRECORDS\tDIGEST\tMISMATCHES")
				for _, cv := range rep.Collections {
					fmt.Fprintf(w, "%s\t%d\t%s\t%d\n", cv.Name, cv.Records, cv.Digest[:16], len(cv.Mismatches))
				}
				_ = w.Flush()
				for _, cv := range rep.Collections {
					for _, mm := range cv.Mismatches {
						fmt.Fprintf(out, "mismatch %s/%s: %s\n", cv.Name, mm.ID, mm.Reason)
					}
				}
				if rep.AgainstHub != nil {
					fmt.Fprintf(out, "hub compared: %v, differing collections: %v\n", rep.AgainstHub.Checked, rep.AgainstHub.Mismatch)
				}
			}
			bad := 0
			for _, cv := range rep.Collections {
				bad += len(cv.Mismatches)
			}
			if bad > 0 || (rep.AgainstHub != nil && len(rep.AgainstHub.Mismatch) > 0) {
				return fmt.Errorf("verify: %d record mismatches, hub differences: %v", bad, hubMismatch(rep))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	c.Flags().BoolVar(&against, "against-hub", false, "spoke: compare the metadata digests with the hub")
	return c
}

func hubMismatch(rep *VerifyReport) []string {
	if rep.AgainstHub == nil {
		return nil
	}
	return rep.AgainstHub.Mismatch
}

func verifyAgainstHub(ctx context.Context, app core.App, rep *VerifyReport) (*HubVerify, error) {
	id, err := proto.LoadOrCreateIdentity(filepath.Join(app.DataDir(), NodeKeyFile), os.Getenv(EnvNodeKey))
	if err != nil {
		return nil, err
	}
	cl, err := client.New(client.Options{App: app, Identity: id, Profile: profileFromEnv(), AppVersion: appVersion()})
	if err != nil {
		return nil, err
	}
	cur, err := client.LoadCursor(app)
	if err != nil || cur == nil {
		return nil, fmt.Errorf("this node is not enrolled")
	}
	digest := map[string]string{}
	for _, cv := range rep.Collections {
		digest[cv.Collection] = cv.MetaDigest
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ar, err := cl.Ack(ctx, cur.PullAfter, digest)
	if err != nil {
		return nil, err
	}
	return &HubVerify{Checked: ar.DigestChecked, Mismatch: ar.DigestMismatch}, nil
}
