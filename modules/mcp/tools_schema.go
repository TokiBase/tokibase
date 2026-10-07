//go:build !no_mcp

package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tokibase/tokibase/kernel"
)

type emptyIn struct{}

type describeIn struct {
	Collection string `json:"collection" jsonschema:"collection name or id"`
}

type explainIn struct {
	Collection string `json:"collection" jsonschema:"collection name or id"`
	Operation  string `json:"operation" jsonschema:"list, view, create, update, delete, auth (auth collections) or manage (auth collections)"`
	AsUserID   string `json:"as_user_id,omitempty" jsonschema:"operator only: also evaluate as this auth record (searched in all auth collections)"`
	RecordID   string `json:"record_id,omitempty" jsonschema:"operator only: evaluate the rule against this record (not for create)"`
}

// rulesOf returns every rule of a collection keyed by operation.
func rulesOf(c *kernel.Collection) map[string]*string {
	m := map[string]*string{
		"list": c.ListRule, "view": c.ViewRule, "create": c.CreateRule,
		"update": c.UpdateRule, "delete": c.DeleteRule,
	}
	if c.IsAuth() {
		m["auth"] = c.AuthRule
		m["manage"] = c.ManageRule
	}
	return m
}

// collectionSummary describes a collection; relation targets the agent may not
// touch are masked so their ids do not leak.
func (s *Server) collectionSummary(a *Agent, c *kernel.Collection) map[string]any {
	fields := jsonValue(c.Fields).([]any)
	if a != nil {
		for _, f := range fields {
			m, _ := f.(map[string]any)
			id, _ := m["collectionId"].(string)
			if id == "" {
				continue
			}
			if t, err := s.app.FindCollectionByNameOrId(id); err != nil || !canTouch(a, t, false) {
				m["collectionId"] = "[hidden]"
			}
		}
	}
	return map[string]any{
		"id": c.Id, "name": c.Name, "type": c.Type, "system": c.System,
		"fields": fields, "rules": rulesOf(c), "indexes": c.Indexes,
	}
}

func (s *Server) registerSchemaTools() {
	addTool(s, "schema.list", "List the collections visible to this agent with type, field count, rule state per operation (locked = superusers only, public, expression) and (operators only) record count.",
		RoleReader, false, func(c *call, _ emptyIn) (map[string]any, error) {
			cols, err := s.visibleCollections(c.agent)
			if err != nil {
				return nil, err
			}
			items := make([]map[string]any, 0, len(cols))
			for _, col := range cols {
				states := map[string]string{}
				for k, r := range rulesOf(col) {
					states[k] = ruleState(r)
				}
				item := map[string]any{
					"name": col.Name, "type": col.Type, "system": col.System,
					"fields": len(col.Fields), "rules": states,
				}
				// counts ignore list rules: operators only
				if c.agent.Role == RoleOperator {
					if n, err := s.app.CountRecords(col); err == nil {
						item["records"] = n
					}
				}
				items = append(items, item)
			}
			return map[string]any{"collections": items}, nil
		})

	addTool(s, "schema.describe", "Describe one collection: fields with types and options, all rules, indexes. Operators also get 2 sample records (strings cut to 40 chars, fields named password/token/secret/key redacted).",
		RoleReader, false, func(c *call, in describeIn) (map[string]any, error) {
			col, err := s.collection(c.agent, in.Collection, false)
			if err != nil {
				return nil, err
			}
			c.collection = col.Name
			out := s.collectionSummary(c.agent, col)
			if col.IsView() && c.agent.Role == RoleOperator {
				out["viewQuery"] = col.ViewQuery
			}
			if c.agent.Role == RoleOperator {
				var recs []*kernel.Record
				if err := s.app.RecordQuery(col).Limit(2).All(&recs); err != nil {
					return nil, s.internal(c, "samples", err)
				}
				samples := make([]any, 0, len(recs))
				for _, r := range recs {
					samples = append(samples, sanitize(exportRedacted(r), 40))
				}
				out["samples"] = samples
			} else {
				out["samples"] = "omitted: sample records need role operator"
			}
			return out, nil
		})

	addTool(s, "rule.lint", "Report collection rules that are public (empty string) and not allowlisted in ruleguard.json.",
		RoleReader, false, func(c *call, _ emptyIn) (map[string]any, error) {
			p := getProviders().RuleLint
			if p == nil {
				return nil, errModuleOff("ruleguard")
			}
			res, err := p(s.app)
			if err != nil {
				return nil, s.internal(c, "rule.lint", err)
			}
			v := jsonValue(res)
			if list, ok := v.([]any); ok {
				kept := []any{}
				for _, f := range list {
					m, _ := f.(map[string]any)
					name, _ := m["collection"].(string)
					if col, err := s.app.FindCollectionByNameOrId(name); err != nil || !canTouch(c.agent, col, false) {
						continue
					}
					kept = append(kept, f)
				}
				v = kept
			}
			return map[string]any{"findings": v}, nil
		})

	addTool(s, "rule.explain", "Explain one collection rule: its text, whether it is locked/public/an expression, and (operators, with record_id) whether a guest, the given user and the agent itself would pass it.",
		RoleReader, false, func(c *call, in explainIn) (map[string]any, error) {
			col, err := s.collection(c.agent, in.Collection, false)
			if err != nil {
				return nil, err
			}
			c.collection = col.Name
			op := strings.ToLower(strings.TrimSpace(in.Operation))
			rule, known := rulesOf(col)[op]
			if !known {
				return nil, fmt.Errorf("unknown operation %q for collection %q", in.Operation, col.Name)
			}
			out := map[string]any{
				"collection": col.Name, "operation": op, "rule": rule, "state": ruleState(rule),
			}
			switch ruleState(rule) {
			case "locked":
				out["meaning"] = "null rule: only superusers pass. Agents pass only with role operator."
			case "public":
				out["meaning"] = "empty rule: everyone passes, including guests."
			default:
				out["meaning"] = "filter expression: the request passes when it evaluates to true for the record."
				if strings.Contains(*rule, "@request.auth") {
					out["hint"] = "the rule depends on @request.auth: guests fail it; agents are evaluated with the _agents record as auth (@request.auth.kind = \"agent\", @request.auth.id, @request.auth.agent.role; other @request.auth.* names are empty), operators bypass rules."
				}
			}
			if in.RecordID == "" && in.AsUserID == "" {
				return out, nil
			}
			if c.agent.Role != RoleOperator {
				return nil, fmt.Errorf("record_id/as_user_id evaluation needs role operator (it would reveal which records exist)")
			}
			if in.RecordID == "" {
				return nil, fmt.Errorf("as_user_id needs record_id")
			}
			if op == "create" {
				return nil, fmt.Errorf("a create rule can not be evaluated against an existing record")
			}
			rec, err := s.app.FindRecordById(col, in.RecordID)
			if err != nil {
				return nil, fmt.Errorf("record %q not found in %q", in.RecordID, col.Name)
			}
			c.record = rec.Id
			eval := func(info *kernel.RequestInfo) any {
				ok, err := s.app.CanAccessRecord(rec, info, rule)
				if err != nil {
					s.app.Logger().Warn("mcp: rule evaluation failed", "error", err)
					return map[string]any{"error": "rule evaluation failed"}
				}
				return ok
			}
			res := map[string]any{
				"guest": eval(guestInfo()),
				// operators bypass rules (superuser-like); evaluate as the agent really acts
				"agent": true,
			}
			if in.AsUserID != "" {
				user, ucol := s.findAuthRecord(c.agent, in.AsUserID)
				if user == nil {
					return nil, fmt.Errorf("auth record %q not found in any auth collection", in.AsUserID)
				}
				info := guestInfo()
				info.Auth = user
				res["user"] = eval(info)
				res["user_collection"] = ucol
			}
			out["record_id"] = rec.Id
			out["passes"] = res
			return out, nil
		})
}

func errModuleOff(name string) error {
	return fmt.Errorf("module %s is not enabled in this instance", name)
}

func (s *Server) findAuthRecord(a *Agent, id string) (*kernel.Record, string) {
	cols, err := s.app.FindAllCollections(kernel.CollectionTypeAuth)
	if err != nil {
		return nil, ""
	}
	for _, c := range cols {
		if !canTouch(a, c, false) {
			continue
		}
		if r, err := s.app.FindRecordById(c, id); err == nil {
			return r, c.Name
		}
	}
	return nil, ""
}

// schemaJSON renders the schema resource (no sample data).
func (s *Server) schemaJSON(a *Agent) (string, error) {
	cols, err := s.visibleCollections(a)
	if err != nil {
		return "", err
	}
	items := make([]map[string]any, 0, len(cols))
	for _, c := range cols {
		items = append(items, s.collectionSummary(a, c))
	}
	raw, err := json.MarshalIndent(map[string]any{"collections": items}, "", "  ")
	return string(raw), err
}
