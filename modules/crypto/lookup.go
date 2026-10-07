package crypto

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
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
	segs := strings.Split(tok, ".")
	i := 0
	cur := col
	if strings.HasPrefix(segs[0], "@") {
		if stripMod(segs[0]) != "@collection" || len(segs) < 3 {
			return ""
		}
		c, err := m.app.FindCachedCollectionByNameOrId(stripMod(segs[1]))
		if err != nil || c == nil {
			return ""
		}
		cur, i = c, 2
	}
	for ; i < len(segs); i++ {
		name := stripMod(segs[i])
		if cfg, _ := m.fieldsFor(cur.Id); cfg != nil {
			if _, ok := cfg[name]; ok {
				return cur.Name + "." + name
			}
		}
		if f, ok := cur.Fields.GetByName(name).(*core.RelationField); ok && f != nil {
			c, err := m.app.FindCachedCollectionByNameOrId(f.CollectionId)
			if err != nil || c == nil {
				return ""
			}
			cur = c
			continue
		}
		if j := strings.Index(name, "_via_"); j > 0 {
			c, err := m.app.FindCachedCollectionByNameOrId(name[:j])
			if err != nil || c == nil {
				return ""
			}
			cur = c
			continue
		}
		return ""
	}
	return ""
}

// guardList rejects filter/sort expressions that touch an encrypted field.
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
		if err := check("filter", []string{f}); err != nil {
			return err
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
					if e.Request.Method == http.MethodGet && (e.Request.URL.RawQuery != "") {
						if mm := reListPath.FindStringSubmatch(e.Request.URL.Path); mm != nil {
							if col, err := e.App.FindCachedCollectionByNameOrId(mm[1]); err == nil && col != nil {
								if err := m.guardList(e, col); err != nil {
									return err
								}
							}
						}
					}
					return e.Next()
				},
			})
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
	value := e.Request.URL.Query().Get("value")
	if value == "" {
		return e.BadRequestError("Missing value.", nil)
	}
	info, err := e.RequestInfo()
	if err != nil {
		return e.BadRequestError("", err)
	}
	recs, err := FindByBlindIndex(e.App, col.Name, field, value)
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
// the list rule per record).
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
	hs, err := m.blindHMACs(col, field, value)
	if err != nil {
		return nil, err
	}
	if len(hs) == 0 {
		return nil, nil
	}
	ids := []string{}
	err = app.DB().Select("record").From(IndexTable).
		Where(dbx.HashExp{"collection": col.Id, "field": field, "hmac": hs}).
		Limit(1000).Column(&ids)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	recs, err := app.FindRecordsByIds(col.Id, ids)
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

var _ = errors.New
