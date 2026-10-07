//go:build !no_mcp

package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/search"
)

const (
	maxPerPage = 200
	maxBatch   = 500
	// planThreshold: batches affecting more records than this need a confirm_token.
	planThreshold = 100
)

type queryIn struct {
	Collection string `json:"collection" jsonschema:"collection name or id"`
	Filter     string `json:"filter,omitempty" jsonschema:"PocketBase filter expression"`
	Sort       string `json:"sort,omitempty" jsonschema:"comma separated fields, - prefix for DESC"`
	Page       int    `json:"page,omitempty" jsonschema:"1-based page, default 1"`
	PerPage    int    `json:"perPage,omitempty" jsonschema:"items per page, default 30, max 200"`
	Expand     string `json:"expand,omitempty" jsonschema:"comma separated relation fields to expand"`
	Fields     string `json:"fields,omitempty" jsonschema:"comma separated top level fields to keep"`
}

type getIn struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
	Expand     string `json:"expand,omitempty"`
	Fields     string `json:"fields,omitempty"`
}

type createIn struct {
	Collection string         `json:"collection"`
	Data       map[string]any `json:"data" jsonschema:"field values; field modifiers like tags+ are accepted"`
	Reason     string         `json:"reason" jsonschema:"why this change is made (audited)"`
}

type updateIn struct {
	Collection string         `json:"collection"`
	ID         string         `json:"id"`
	Data       map[string]any `json:"data"`
	Reason     string         `json:"reason"`
}

type deleteIn struct {
	Collection   string `json:"collection"`
	ID           string `json:"id"`
	Reason       string `json:"reason"`
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"omit to get a plan and a token valid 5 minutes; pass it back unchanged to execute"`
}

// BatchOp is one operation of records.batch.
type BatchOp struct {
	Action     string         `json:"action" jsonschema:"create, update or delete"`
	Collection string         `json:"collection"`
	ID         string         `json:"id,omitempty" jsonschema:"required for update and delete"`
	Data       map[string]any `json:"data,omitempty"`
}

type batchIn struct {
	Ops          []BatchOp `json:"ops" jsonschema:"up to 500 operations, applied atomically"`
	Reason       string    `json:"reason"`
	ConfirmToken string    `json:"confirm_token,omitempty" jsonschema:"required to execute when the batch has any delete or more than 100 operations"`
}

func needReason(r string) (string, error) {
	r = strings.TrimSpace(r)
	if len(r) < 3 {
		return "", errors.New("reason is required for writes (at least 3 characters): say why this change is made")
	}
	return r, nil
}

// ruleFor picks the rule of an operation.
func ruleFor(c *kernel.Collection, op string) *string {
	if op == "view" {
		return c.ViewRule
	}
	return c.ListRule
}

func (s *Server) registerRecordTools() {
	addTool(s, "records.query", "List records of a collection. Non-operator agents are evaluated as guest against the collection list rule (locked rule = no access); operators bypass rules. Hidden fields are never returned.",
		RoleReader, false, func(c *call, in queryIn) (map[string]any, error) {
			col, err := s.collection(c.agent, in.Collection, false)
			if err != nil {
				return nil, err
			}
			c.collection = col.Name
			c.set("filter", in.Filter)
			if err := s.checkTraversal(c.agent, col, in.Expand, in.Filter, in.Sort); err != nil {
				return nil, err
			}
			rule := col.ListRule
			if rule == nil && c.agent.Role != RoleOperator {
				return nil, fmt.Errorf("collection %q has a locked list rule (superusers only); agents need role operator", col.Name)
			}
			perPage := in.PerPage
			if perPage <= 0 {
				perPage = 30
			}
			if perPage > maxPerPage {
				return nil, fmt.Errorf("perPage must be <= %d", maxPerPage)
			}
			page := in.Page
			if page <= 0 {
				page = 1
			}

			ctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
			defer cancel()
			query := s.app.RecordQuery(col).WithContext(ctx)
			resolver := kernel.NewRecordFieldResolver(s.app, col, guestInfo(), true)
			if c.agent.Role != RoleOperator && rule != nil && *rule != "" {
				expr, err := search.FilterData(*rule).BuildExpr(resolver)
				if err != nil {
					return nil, s.internal(c, "list rule", err)
				}
				query.AndWhere(expr)
			}
			resolver.SetAllowHiddenFields(false)
			provider := search.NewProvider(resolver).Query(query)
			if !col.IsView() {
				provider.CountCol("_rowid_")
			}
			q := url.Values{}
			q.Set("page", strconv.Itoa(page))
			q.Set("perPage", strconv.Itoa(perPage))
			if in.Filter != "" {
				q.Set("filter", in.Filter)
			}
			if in.Sort != "" {
				q.Set("sort", in.Sort)
			}
			recs := []*kernel.Record{}
			res, err := provider.ParseAndExec(q.Encode(), &recs)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
					return nil, fmt.Errorf("query timed out after %s: narrow the filter", queryTimeout)
				}
				s.app.Logger().Warn("mcp: query failed", "tool", c.tool, "error", err)
				return nil, errors.New("query failed: invalid filter, sort or parameters (details are in the server log)")
			}
			if in.Expand != "" {
				s.expand(c.agent, recs, in.Expand)
			}
			s.enrich(c.agent, recs)
			items := make([]any, 0, len(recs))
			for _, r := range recs {
				items = append(items, exportRecord(r, in.Fields))
			}
			return map[string]any{
				"page": res.Page, "perPage": res.PerPage, "totalItems": res.TotalItems,
				"totalPages": res.TotalPages, "items": items,
			}, nil
		})

	addTool(s, "records.get", "Fetch one record by id. Non-operator agents are evaluated as guest against the view rule.",
		RoleReader, false, func(c *call, in getIn) (map[string]any, error) {
			col, err := s.collection(c.agent, in.Collection, false)
			if err != nil {
				return nil, err
			}
			c.collection, c.record = col.Name, in.ID
			if err := s.checkTraversal(c.agent, col, in.Expand, "", ""); err != nil {
				return nil, err
			}
			notFound := fmt.Errorf("record %q not found or not accessible in %q", in.ID, col.Name)
			rec, err := s.app.FindRecordById(col, in.ID)
			if err != nil {
				return nil, notFound
			}
			if ok, err := s.canRead(c.agent, rec, ruleFor(col, "view")); err != nil || !ok {
				return nil, notFound
			}
			if in.Expand != "" {
				s.expand(c.agent, []*kernel.Record{rec}, in.Expand)
			}
			s.enrich(c.agent, []*kernel.Record{rec})
			return map[string]any{"record": exportRecord(rec, in.Fields)}, nil
		})

	addTool(s, "records.create", "Create a record. Needs role writer or operator and the collection in the agent allowlist. The change is audited with the reason.",
		RoleWriter, true, func(c *call, in createIn) (map[string]any, error) {
			col, err := s.collection(c.agent, in.Collection, true)
			if err != nil {
				return nil, err
			}
			c.collection = col.Name
			reason, err := needReason(in.Reason)
			if err != nil {
				return nil, err
			}
			c.set("reason", reason)
			c.set("data", sanitize(in.Data, 200))
			rec, err := s.createRecord(c, c.agent, s.app, col, in.Data)
			if err != nil {
				return nil, err
			}
			c.record = rec.Id
			return map[string]any{"record": s.exportVisible(c.agent, col, rec)}, nil
		})

	addTool(s, "records.update", "Update a record. Needs role writer or operator and the collection in the agent allowlist. The change is audited with the reason.",
		RoleWriter, true, func(c *call, in updateIn) (map[string]any, error) {
			col, err := s.collection(c.agent, in.Collection, true)
			if err != nil {
				return nil, err
			}
			c.collection, c.record = col.Name, in.ID
			reason, err := needReason(in.Reason)
			if err != nil {
				return nil, err
			}
			c.set("reason", reason)
			c.set("data", sanitize(in.Data, 200))
			rec, err := s.updateRecord(c, c.agent, s.app, col, in.ID, in.Data)
			if err != nil {
				return nil, err
			}
			return map[string]any{"record": s.exportVisible(c.agent, col, rec)}, nil
		})

	addTool(s, "records.delete", "Delete a record in two steps: call without confirm_token to get a plan (what would be deleted, cascade warnings) and a token valid 5 minutes, then call again with the same arguments and the token.",
		RoleWriter, true, func(c *call, in deleteIn) (map[string]any, error) {
			col, err := s.collection(c.agent, in.Collection, true)
			if err != nil {
				return nil, err
			}
			c.collection, c.record = col.Name, in.ID
			reason, err := needReason(in.Reason)
			if err != nil {
				return nil, err
			}
			c.set("reason", reason)
			rec, err := s.app.FindRecordById(col, in.ID)
			if err != nil {
				return nil, fmt.Errorf("record %q not found in %q", in.ID, col.Name)
			}
			warnings, err := s.cascadeCheck(c.agent, col)
			if err != nil {
				if errors.Is(err, errCascadeDenied) {
					return nil, err
				}
				return nil, s.internal(c, "cascade check", err)
			}
			args := map[string]string{"collection": col.Name, "id": rec.Id}
			if in.ConfirmToken == "" {
				c.set("dry_run", true)
				// the preview obeys the same read check as records.get
				preview := any(map[string]any{"id": rec.Id, "collection": col.Name})
				if ok, err := s.canRead(c.agent, rec, col.ViewRule); err == nil && ok {
					s.enrich(c.agent, []*kernel.Record{rec})
					preview = sanitize(jsonValue(rec.PublicExport()), 40)
				}
				return map[string]any{
					"confirm_required": true,
					"plan": map[string]any{
						"action": "delete", "collection": col.Name, "id": rec.Id,
						"preview":  preview,
						"warnings": warnings,
					},
					"confirm_token": s.issuePlan(c.agent, "records.delete", args),
					"expires_in":    int(planTTL.Seconds()),
					"next":          s.nextHint("records.delete"),
				}, nil
			}
			if err := s.consumePlan(c.agent, "records.delete", in.ConfirmToken, args); err != nil {
				return nil, err
			}
			c.set("before", sanitize(jsonValue(rec.PublicExport()), 200))
			if err := s.app.Delete(rec); err != nil {
				return nil, s.internal(c, "delete failed", err)
			}
			return map[string]any{"deleted": true, "collection": col.Name, "id": in.ID}, nil
		})

	addTool(s, "records.batch", "Apply create/update/delete operations atomically (all or nothing). Batches with any delete or more than 100 operations are two-step: without confirm_token you get a plan and a token valid 5 minutes; pass the same ops and the token to execute.",
		RoleWriter, true, func(c *call, in batchIn) (map[string]any, error) {
			reason, err := needReason(in.Reason)
			if err != nil {
				return nil, err
			}
			c.set("reason", reason)
			if len(in.Ops) == 0 || len(in.Ops) > maxBatch {
				return nil, fmt.Errorf("ops must contain 1 to %d operations", maxBatch)
			}
			hasDelete := false
			cascadeChecked := map[string]bool{}
			counts := map[string]int{}
			for i, op := range in.Ops {
				op.Action = strings.ToLower(op.Action)
				in.Ops[i] = op
				col, err := s.collection(c.agent, op.Collection, true)
				if err != nil {
					return nil, fmt.Errorf("op %d: %w", i, err)
				}
				switch op.Action {
				case "create":
				case "update", "delete":
					if op.ID == "" {
						return nil, fmt.Errorf("op %d: id is required for %s", i, op.Action)
					}
					if _, err := s.app.FindRecordById(col, op.ID); err != nil {
						return nil, fmt.Errorf("op %d: record %q not found in %q", i, op.ID, col.Name)
					}
					if op.Action == "delete" {
						hasDelete = true
						if !cascadeChecked[col.Id] {
							if _, err := s.cascadeCheck(c.agent, col); err != nil {
								if errors.Is(err, errCascadeDenied) {
									return nil, fmt.Errorf("op %d: %w", i, err)
								}
								return nil, s.internal(c, "cascade check", err)
							}
							cascadeChecked[col.Id] = true
						}
					}
				default:
					return nil, fmt.Errorf("op %d: unknown action %q (create, update, delete)", i, op.Action)
				}
				if op.Action != "delete" {
					if err := checkData(col, op.Data); err != nil {
						return nil, fmt.Errorf("op %d: %w", i, err)
					}
				}
				counts[col.Name+" "+op.Action]++
			}
			c.set("ops", len(in.Ops))
			c.set("summary", counts)
			needPlan := hasDelete || len(in.Ops) > planThreshold
			if needPlan && in.ConfirmToken == "" {
				c.set("dry_run", true)
				return map[string]any{
					"confirm_required": true,
					"plan": map[string]any{
						"operations": len(in.Ops), "summary": counts, "has_delete": hasDelete,
					},
					"confirm_token": s.issuePlan(c.agent, "records.batch", in.Ops),
					"expires_in":    int(planTTL.Seconds()),
					"next":          s.nextHint("records.batch"),
				}, nil
			}
			if needPlan {
				if err := s.consumePlan(c.agent, "records.batch", in.ConfirmToken, in.Ops); err != nil {
					return nil, err
				}
			}
			results := make([]map[string]any, 0, len(in.Ops))
			err = s.app.RunInTransaction(func(tx kernel.App) error {
				for i, op := range in.Ops {
					col, err := tx.FindCollectionByNameOrId(strings.TrimSpace(op.Collection))
					if err != nil {
						return fmt.Errorf("op %d: collection not found", i)
					}
					r := map[string]any{"action": op.Action, "collection": col.Name}
					switch op.Action {
					case "create":
						rec, err := s.createRecord(c, c.agent, tx, col, op.Data)
						if err != nil {
							return fmt.Errorf("op %d: %w", i, err)
						}
						r["id"] = rec.Id
					case "update":
						rec, err := s.updateRecord(c, c.agent, tx, col, op.ID, op.Data)
						if err != nil {
							return fmt.Errorf("op %d: %w", i, err)
						}
						r["id"] = rec.Id
					case "delete":
						rec, err := tx.FindRecordById(col, op.ID)
						if err != nil {
							return fmt.Errorf("op %d: record not found", i)
						}
						if err := tx.Delete(rec); err != nil {
							return fmt.Errorf("op %d: %w", i, s.internal(c, "delete failed", err))
						}
						r["id"] = op.ID
					}
					results = append(results, r)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("batch rolled back: %w", err)
			}
			c.record = "batch"
			c.set("results", capList(results, 50))
			return map[string]any{"applied": len(results), "results": results}, nil
		})
}

func capList[T any](l []T, n int) []T {
	if len(l) > n {
		return l[:n]
	}
	return l
}

// checkData rejects unknown fields (modifiers like "tags+" / "+tags" / "tags-" are accepted).
func checkData(col *kernel.Collection, data map[string]any) error {
	for k := range data {
		base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(k, "+"), "+"), "-")
		if col.Fields.GetByName(base) == nil {
			return fmt.Errorf("unknown field %q in collection %q", k, col.Name)
		}
	}
	return nil
}

var errCascadeDenied = errors.New("delete denied: it would cascade into collections outside the agent allowlist; an operator must delete this record")

func (s *Server) createRecord(c *call, a *Agent, app kernel.App, col *kernel.Collection, data map[string]any) (*kernel.Record, error) {
	if err := checkData(col, data); err != nil {
		return nil, err
	}
	rec := kernel.NewRecord(col)
	for k, v := range data {
		rec.Set(k, v)
	}
	if err := s.writeGuard(a, col, rec, data, true); err != nil {
		return nil, err
	}
	if err := app.Save(rec); err != nil {
		return nil, s.userErr(c, "create failed", err)
	}
	return rec, nil
}

func (s *Server) updateRecord(c *call, a *Agent, app kernel.App, col *kernel.Collection, id string, data map[string]any) (*kernel.Record, error) {
	if err := checkData(col, data); err != nil {
		return nil, err
	}
	if _, ok := data["id"]; ok {
		return nil, errors.New("the id of a record can not be changed")
	}
	rec, err := app.FindRecordById(col, id)
	if err != nil {
		return nil, fmt.Errorf("record %q not found in %q", id, col.Name)
	}
	for k, v := range data {
		rec.Set(k, v)
	}
	if err := s.writeGuard(a, col, rec, data, false); err != nil {
		return nil, err
	}
	if err := app.Save(rec); err != nil {
		return nil, s.userErr(c, "update failed", err)
	}
	return rec, nil
}

// expand resolves relations; for non-operators the related records must pass
// the allowlist and the view rule evaluated as guest.
func (s *Server) expand(a *Agent, recs []*kernel.Record, expand string) {
	// the allowlist and reserved collection rules apply to every role (the
	// paths were already validated by checkTraversal); operators only skip
	// the view rule
	fetch := func(rc *kernel.Collection, ids []string) ([]*kernel.Record, error) {
		if !canTouch(a, rc, false) {
			return nil, nil
		}
		if a.Role != RoleOperator && rc.ViewRule == nil {
			return nil, nil
		}
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		q := s.app.RecordQuery(rc).AndWhere(dbx.In(rc.Name+".id", args...))
		if a.Role != RoleOperator && *rc.ViewRule != "" {
			resolver := kernel.NewRecordFieldResolver(s.app, rc, guestInfo(), true)
			expr, err := search.FilterData(*rc.ViewRule).BuildExpr(resolver)
			if err != nil {
				return nil, err
			}
			q.AndWhere(expr)
			if err := resolver.UpdateQuery(q); err != nil {
				return nil, err
			}
		}
		out := []*kernel.Record{}
		return out, q.All(&out)
	}
	paths := []string{}
	for _, p := range strings.Split(expand, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	s.app.ExpandRecords(recs, paths, fetch)
}
