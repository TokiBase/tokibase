//go:build !no_crypto

package crypto

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
)

const maxLookup = 100

var (
	reLiteral = regexp.MustCompile(`'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"`)
	reIdent   = regexp.MustCompile(`@?[A-Za-z_][A-Za-z0-9_]*(?::[A-Za-z_][A-Za-z0-9_]*)?(?:\.[A-Za-z_][A-Za-z0-9_]*(?::[A-Za-z_][A-Za-z0-9_]*)?)*`)
)

// tokens returns the identifier paths of a filter/sort/rule expression
// (string literals removed, numbers ignored).
func tokens(expr string) []string {
	s := reLiteral.ReplaceAllString(expr, "''")
	var out []string
	for _, loc := range reIdent.FindAllStringIndex(s, -1) {
		if loc[0] > 0 {
			p := s[loc[0]-1]
			if p == '_' || (p >= '0' && p <= '9') || (p >= 'a' && p <= 'z') || (p >= 'A' && p <= 'Z') {
				continue // tail of a number such as 1e5
			}
		}
		out = append(out, s[loc[0]:loc[1]])
	}
	return out
}

func stripMod(seg string) string {
	if i := strings.IndexByte(seg, ':'); i >= 0 {
		return seg[:i]
	}
	return seg
}

// hitsEncrypted resolves one identifier path starting at col (following
// relations, back-relations and @collection.x) and returns the first encrypted
// "collection.field" it touches.
func (m *Module) hitsEncrypted(col *core.Collection, tok string) string {
	hit, _, _ := m.encryptedHit(col, tok)
	return hit
}

// encryptedHit is hitsEncrypted that also reports whether the hit field is
// a blind-index one and whether it is the plain final segment of the path
// (no modifier, nothing after it).
func (m *Module) encryptedHit(col *core.Collection, tok string) (hit string, blind, final bool) {
	segs := strings.Split(tok, ".")
	i := 0
	cur := col
	if strings.HasPrefix(segs[0], "@") {
		if stripMod(segs[0]) != "@collection" || len(segs) < 3 {
			return "", false, false
		}
		c, err := m.app.FindCachedCollectionByNameOrId(stripMod(segs[1]))
		if err != nil || c == nil {
			return "", false, false
		}
		cur, i = c, 2
	}
	for ; i < len(segs); i++ {
		name := stripMod(segs[i])
		cfg, cerr := m.fieldsFor(cur.Id)
		if cerr != nil {
			// configuration unavailable: fail closed, treat the field as encrypted
			return cur.Name + "." + name, false, false
		}
		if cfg != nil {
			if mode, ok := cfg[name]; ok {
				return cur.Name + "." + name, mode == ModeBlindIndex,
					i == len(segs)-1 && name == segs[i]
			}
		}
		if f, ok := cur.Fields.GetByName(name).(*core.RelationField); ok && f != nil {
			c, err := m.app.FindCachedCollectionByNameOrId(f.CollectionId)
			if err != nil || c == nil {
				return "", false, false
			}
			cur = c
			continue
		}
		if j := strings.Index(name, "_via_"); j > 0 {
			c, err := m.app.FindCachedCollectionByNameOrId(name[:j])
			if err != nil || c == nil {
				return "", false, false
			}
			cur = c
			continue
		}
		return "", false, false
	}
	return "", false, false
}

// filterViolation returns the first encrypted "collection.field" a filter
// uses in a way the equality rewrite does not support, or "". The supported
// shape is `<blind-index field> (= | != | ?= | ?!=) "<non-empty string>"`
// (either side). If the filter does not parse, any encrypted field is a
// violation (the request would fail later anyway).
func (m *Module) filterViolation(col *core.Collection, expr string) string {
	ast, err := rule.Parse(expr)
	if err != nil {
		for _, tok := range tokens(expr) {
			if hit := m.hitsEncrypted(col, tok); hit != "" {
				return hit
			}
		}
		return ""
	}
	return m.groupViolation(col, ast.Root)
}

func (m *Module) groupViolation(col *core.Collection, g *rule.Group) string {
	if g == nil {
		return ""
	}
	for _, it := range g.Items {
		switch n := it.Node.(type) {
		case *rule.Group:
			if v := m.groupViolation(col, n); v != "" {
				return v
			}
		case *rule.Comparison:
			if v := m.comparisonViolation(col, n); v != "" {
				return v
			}
		}
	}
	return ""
}

func (m *Module) comparisonViolation(col *core.Collection, c *rule.Comparison) string {
	eq := c.Op == rule.OpEq || c.Op == rule.OpNeq || c.Op == rule.OpAnyEq || c.Op == rule.OpAnyNeq
	side := func(o, other rule.Operand) string {
		id, ok := o.(*rule.Ident)
		if !ok {
			if call, isCall := o.(*rule.Call); isCall {
				for _, a := range call.Args {
					if v := m.operandViolation(col, a); v != "" {
						return v
					}
				}
			}
			return ""
		}
		hit, blind, final := m.encryptedHit(col, id.Name)
		if hit == "" {
			return ""
		}
		if lit, ok := other.(*rule.Literal); eq && blind && final && ok &&
			lit.Kind == rule.LiteralString && lit.Value != "" {
			return ""
		}
		return hit
	}
	if v := side(c.Left, c.Right); v != "" {
		return v
	}
	return side(c.Right, c.Left)
}

// operandViolation: inside a function call any encrypted field is rejected.
func (m *Module) operandViolation(col *core.Collection, o rule.Operand) string {
	switch x := o.(type) {
	case *rule.Ident:
		return m.hitsEncrypted(col, x.Name)
	case *rule.Call:
		for _, a := range x.Args {
			if v := m.operandViolation(col, a); v != "" {
				return v
			}
		}
	}
	return ""
}

func encryptedFieldResponse(e *core.RequestEvent, param, msg string) error {
	return e.BadRequestError("Failed to load the records.", validation.Errors{
		param: validation.NewError(ErrCode, msg),
	})
}

// asEncryptedFieldError finds the kernel's typed error behind err, also when a
// handler wrapped it into a router.ApiError.
func asEncryptedFieldError(err error) *kernel.EncryptedFieldError {
	var ef *kernel.EncryptedFieldError
	if errors.As(err, &ef) {
		return ef
	}
	var ae *router.ApiError
	if errors.As(err, &ae) {
		if raw, ok := ae.RawData().(error); ok && errors.As(raw, &ef) {
			return ef
		}
	}
	return nil
}

// guardList is the HTTP pre-check of the records list endpoint. The shape rule
// itself is enforced by the kernel field resolver (kernel.EncryptedFieldError),
// which covers every path; this check additionally covers what the resolver
// hook cannot see (sort, function arguments, unparsable filters) and answers
// before any query runs.
func (m *Module) guardList(e *core.RequestEvent, col *core.Collection) error {
	q := e.Request.URL.Query()
	check := func(param string, exprs []string) error {
		for _, ex := range exprs {
			for _, tok := range tokens(ex) {
				if hit := m.hitsEncrypted(col, tok); hit != "" {
					return e.BadRequestError("Failed to load the records.", validation.Errors{
						param: validation.NewError(ErrCode,
							fmt.Sprintf("Encrypted field %q cannot be used in %s.", hit, param)),
					})
				}
			}
		}
		return nil
	}
	if f := q.Get("filter"); f != "" {
		if hit := m.filterViolation(col, f); hit != "" {
			return e.BadRequestError("Failed to load the records.", validation.Errors{
				"filter": validation.NewError(ErrCode,
					fmt.Sprintf("Encrypted field %q cannot be used in filter.", hit)),
			})
		}
	}
	if s := q.Get("sort"); s != "" {
		parts := strings.Split(s, ",")
		for i, p := range parts {
			parts[i] = strings.TrimLeft(strings.TrimSpace(p), "+-")
		}
		if err := check("sort", parts); err != nil {
			return err
		}
	}
	return nil
}

var reListPath = regexp.MustCompile(`^/api/collections/([^/]+)/records/?$`)

func (m *Module) bindHTTP() {
	m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId,
		Func: func(se *core.ServeEvent) error {
			se.Router.Bind(&hook.Handler[*core.RequestEvent]{
				Id: hookId, Priority: -1 << 19,
				Func: func(e *core.RequestEvent) error {
					// GET patterns also serve HEAD
					if (e.Request.Method == http.MethodGet || e.Request.Method == http.MethodHead) && e.Request.URL.RawQuery != "" {
						if mm := reListPath.FindStringSubmatch(e.Request.URL.Path); mm != nil {
							if col, err := e.App.FindCachedCollectionByNameOrId(mm[1]); err == nil && col != nil {
								if err := m.guardList(e, col); err != nil {
									return err
								}
							}
						}
					}
					// the kernel resolver rejects unsupported shapes (filters of
					// every endpoint, view/list rules, batch...) with a typed
					// error: answer it with the documented 400 payload.
					err := e.Next()
					if ef := asEncryptedFieldError(err); ef != nil {
						return encryptedFieldResponse(e, "filter", ef.Error())
					}
					return err
				},
			})
			se.Router.POST("/api/crypto/lookup/{collection}/{field}", m.lookupHandler)
			// GET is kept for compatibility, but the value then travels in the URL
			// and is stored in the request log: prefer POST.
			se.Router.GET("/api/crypto/lookup/{collection}/{field}", m.lookupHandler)
			return se.Next()
		},
	})
}

func (m *Module) lookupHandler(e *core.RequestEvent) error {
	col, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || col == nil {
		return e.NotFoundError("", nil)
	}
	field := e.Request.PathValue("field")
	cfg, _ := m.fieldsFor(col.Id)
	if cfg[field] != ModeBlindIndex {
		return e.NotFoundError("The field is not a blind-index field.", nil)
	}
	info, err := e.RequestInfo()
	if err != nil {
		return e.BadRequestError("Invalid request body, expected {\"value\": \"...\"}.", nil)
	}
	var value string
	if e.Request.Method == http.MethodPost {
		switch v := info.Body["value"].(type) {
		case string:
			value = v
		case float64:
			value = strconv.FormatFloat(v, 'f', -1, 64)
		}
	} else {
		value = e.Request.URL.Query().Get("value")
	}
	if value == "" {
		return e.BadRequestError("Missing value.", nil)
	}
	recs, err := FindByBlindIndex(e.App, col.Name, field, value)
	if errors.Is(err, errTooManyMatches) {
		return e.BadRequestError("Too many matches.", nil)
	}
	if err != nil {
		return e.InternalServerError("", err)
	}
	items := make([]*core.Record, 0, len(recs))
	for _, r := range recs {
		if len(items) >= maxLookup {
			break
		}
		ok, err := e.App.CanAccessRecord(r, info, col.ListRule)
		if err == nil && ok {
			items = append(items, r)
		}
	}
	if err := apis.EnrichRecords(e, items); err != nil {
		return err
	}
	// Enrichment applies hidden fields and field-level read permissions
	// (fieldperm). A caller who cannot read the looked-up field must not learn
	// that a record has a given value in it: drop what does not export it.
	visible := items[:0]
	for _, r := range items {
		if _, ok := r.PublicExport()[field]; ok {
			visible = append(visible, r)
		}
	}
	items = visible
	return e.JSON(http.StatusOK, map[string]any{"items": items, "totalItems": len(items)})
}

// BlindIndexValue returns the index HMACs of value for every usable key
// version of the collection (a lookup tries all of them while a rotation is
// in progress).
func (m *Module) blindHMACs(col *core.Collection, field, value string) ([]any, error) {
	k, err := m.keysFor(col.Id)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, dek := range k.deks {
		ik, err := indexKey(dek, col.Id)
		if err != nil {
			return nil, err
		}
		out = append(out, blindHMAC(ik, field, value))
	}
	return out, nil
}

// FindByBlindIndex returns the records whose blind-index field equals value
// (exact, case sensitive). Records are returned as stored: encrypted fields
// still hold ciphertext until [Decrypt] is called. No collection rule is
// applied; callers must check access themselves (the HTTP endpoint evaluates
// the list rule per record). More than 1000 index rows for the value is an
// error (errTooManyMatches), never a silent truncation.
func FindByBlindIndex(app core.App, collection, field, value string) ([]*core.Record, error) {
	m := From(app)
	if m == nil || !m.Active() {
		return nil, ErrNoMasterKey
	}
	col, err := app.FindCachedCollectionByNameOrId(collection)
	if err != nil {
		return nil, fmt.Errorf("collection %q not found", collection)
	}
	cfg, err := m.fieldsFor(col.Id)
	if err != nil {
		return nil, err
	}
	if cfg[field] != ModeBlindIndex {
		return nil, fmt.Errorf("%s.%s is not a blind-index field", col.Name, field)
	}
	return m.findByBlindIndex(col, field, value, maxFilterMatches)
}

// errTooManyMatches: the value matches more index rows than allowed. The
// message is deliberately generic (it must not reveal how many records hold
// the value).
var errTooManyMatches = errors.New("too many matches")

// findByBlindIndex is FindByBlindIndex for an already resolved blind-index
// field. The cap is checked on the index rows (LIMIT limit+1) before any
// record is loaded or decrypted; every hit is then verified.
func (m *Module) findByBlindIndex(col *core.Collection, field, value string, limit int) ([]*core.Record, error) {
	hs, err := m.blindHMACs(col, field, value)
	if err != nil {
		return nil, err
	}
	if len(hs) == 0 {
		return nil, nil
	}
	ids := []string{}
	err = m.app.DB().Select("record").From(IndexTable).
		Where(dbx.HashExp{"collection": col.Id, "field": field, "hmac": hs}).
		Limit(int64(limit) + 1).Column(&ids)
	if err != nil {
		return nil, err
	}
	if len(ids) > limit {
		return nil, errTooManyMatches
	}
	if len(ids) == 0 {
		return nil, nil
	}
	recs, err := m.app.FindRecordsByIds(col.Id, ids)
	if err != nil {
		return nil, err
	}
	isJSON := isJSONField(col, field)
	out := recs[:0]
	for _, r := range recs { // verify: an index row may be stale
		p, err := m.openStored(col, field, r.Id, storedOf(r, field), isJSON)
		if err != nil || p != value {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// maxFilterMatches bounds how many index rows one equality comparison of a
// filter or rule may match (the ids are inlined as bound parameters).
const maxFilterMatches = 1000

// indexProvider plugs the blind index into the kernel field resolver
// (kernel.BlindIndexProvider): `field = "v"` in a filter or rule is rewritten
// to a comparison against the ids found through the index.
type indexProvider struct{ m *Module }

func (p indexProvider) IsEncrypted(collectionId, field string) (bool, error) {
	cfg, err := p.m.fieldsFor(collectionId)
	if err != nil {
		return false, err
	}
	_, ok := cfg[field]
	return ok, nil
}

// IsBlindIndex is true only for a blind-index field in steady state. While the
// field is being enabled or disabled the index is incomplete, so equality
// (and above all "!=") would give wrong answers: it is not queryable then.
func (p indexProvider) IsBlindIndex(collectionId, field string) bool {
	cfg, err := p.m.fieldsFor(collectionId)
	return err == nil && cfg[field] == ModeBlindIndex && p.m.stateOf(collectionId, field) == ""
}

func (p indexProvider) IsBlindIndexInactive(collectionId, field string) bool {
	cfg, err := p.m.fieldsFor(collectionId)
	return err == nil && cfg[field] == ModeBlindIndex && p.m.stateOf(collectionId, field) != ""
}

func (p indexProvider) BlindIndexIDs(col *core.Collection, field, value string, info *core.RequestInfo, enforce bool) ([]string, error) {
	m := p.m
	if !m.Active() {
		return nil, ErrNoMasterKey
	}
	fail := func(reason string) error {
		return &kernel.EncryptedFieldError{Collection: col.Name, Field: field, Reason: reason}
	}
	if IsCiphertext(value) {
		// e.g. @request.auth.<encrypted field>: the auth record is loaded raw
		return nil, fail("the compared value is ciphertext (an encrypted field of the auth record or of a request value cannot be used)")
	}
	recs, err := m.findByBlindIndex(col, field, value, maxFilterMatches)
	if errors.Is(err, errTooManyMatches) {
		return nil, fail("too many matches")
	}
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		if enforce && !m.fieldVisible(r, field, info) {
			continue
		}
		ids = append(ids, r.Id)
	}
	return ids, nil
}

// fieldVisible applies the visibility rule of the lookup endpoint: the field
// must survive the enrich hooks (fieldperm read rules, hooks hiding the
// field) and not be a hidden field. Errors fail closed.
func (m *Module) fieldVisible(r *core.Record, field string, info *core.RequestInfo) bool {
	ev := new(core.RecordEnrichEvent)
	ev.App = m.app
	ev.Record = r
	ev.RequestInfo = info
	if err := m.app.OnRecordEnrich().Trigger(ev); err != nil {
		return false
	}
	_, ok := r.PublicExport()[field]
	return ok
}
