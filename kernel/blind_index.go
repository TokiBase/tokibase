package kernel

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/tools/search"
	"github.com/tokibase/tokibase/tools/security"
)

// blindIndexStoreKey is the app store key of the registered [BlindIndexProvider].
const blindIndexStoreKey = "__tokiBlindIndexProvider__"

// MaxBlindIndexLookups bounds how many distinct blind-index lookups one
// resolver (one request filter plus the rules built with it) may perform.
// Identical comparisons (same collection, field, value) are memoized and do not
// count. Realtime subscriptions build a new resolver per event and subscriber,
// so the cap bounds the cost of one evaluation, not of the subscription.
const MaxBlindIndexLookups = 8

// EncryptedFieldError is returned (fail closed) by the field resolver when an
// expression uses an encrypted field in a way that is not supported, or when
// the lookup cannot be performed. It is the single typed error of every entry
// point (list, view, HEAD, geo, realtime, MCP, batch, rules): HTTP layers map
// it to a 400.
type EncryptedFieldError struct {
	Collection string
	Field      string
	Reason     string // short, never contains data
}

func (e *EncryptedFieldError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("Encrypted field %q cannot be used in this expression.", e.Collection+"."+e.Field)
	}
	return fmt.Sprintf("Encrypted field %q cannot be used in this expression: %s.", e.Collection+"."+e.Field, e.Reason)
}

// BlindIndexProvider is the seam through which a module (modules/crypto) makes
// equality comparisons on its blind-indexed fields work in filters and rules
// without the kernel knowing anything about encryption. Modules must not
// import each other, so the module registers an implementation with
// [SetBlindIndexProvider] and the field resolver consults it.
//
// See docs/modules/crypto.md ("Filters, sort, lookup").
type BlindIndexProvider interface {
	// IsEncrypted reports whether collectionId.field is stored encrypted
	// (any mode, any state). An error means the configuration is unavailable:
	// the resolver then rejects comparisons on the field (fail closed).
	IsEncrypted(collectionId, field string) (bool, error)

	// IsBlindIndex reports whether collectionId.field supports equality
	// lookups by value. It must be false while the field is being enabled or
	// disabled (the index is incomplete): the field is then not queryable.
	IsBlindIndex(collectionId, field string) bool

	// IsBlindIndexInactive reports whether collectionId.field is configured as
	// blind-index but its index is incomplete (state enabling/disabling).
	IsBlindIndexInactive(collectionId, field string) bool

	// BlindIndexIDs returns the ids of the records of collection whose field
	// equals value (exact, case sensitive). When enforceVisibility is true the
	// ids of records whose field the caller (info) cannot read must be left
	// out, so that the comparison is not an equality oracle. A value that is
	// itself ciphertext (for instance @request.auth.<encrypted field>) must be
	// rejected with an error.
	BlindIndexIDs(collection *Collection, field, value string, info *RequestInfo, enforceVisibility bool) ([]string, error)
}

// SetBlindIndexProvider registers (or with nil removes) the provider of the app.
func SetBlindIndexProvider(app App, p BlindIndexProvider) {
	if p == nil {
		app.Store().Remove(blindIndexStoreKey)
		return
	}
	app.Store().Set(blindIndexStoreKey, p)
}

func blindIndexProviderOf(app App) BlindIndexProvider {
	if app == nil {
		return nil
	}
	p, _ := app.Store().Get(blindIndexStoreKey).(BlindIndexProvider)
	return p
}

var rePlaceholderOnly = regexp.MustCompile(`^\{:(\w+)\}$`)

// attachBlindIndex wires the encrypted-field handling into result (the
// resolved column identifier of the active table alias).
//
// Supported shape: when a plain blind-index field (no modifier, not through
// a function) is compared with `=`, `!=`, `?=` or `?!=` to a bound non-empty
// string, the identifier becomes
//
//	CASE WHEN <alias>.id IN (<matching ids>) THEN <the bound value> ELSE '' END
//
// (the value is a non-empty string, so the ELSE branch never equals it; a NULL
// there would break the "all related records match" multi-match semantics)
//
// so the unchanged comparison machinery (null handling, multi-match for
// relation paths, parameter binding, both the legacy and the AST emitter)
// compares equal exactly for the matching records.
//
// Any other shape on an encrypted field (other operators, modifiers, another
// field, null, number, empty string) returns an [EncryptedFieldError] from the
// hook when the resolver is strict, i.e. it resolves a client filter (see
// [RecordFieldResolver.SetAllowHiddenFields]). Collection rules are lenient
// for those shapes (they compare against ciphertext as before; `toki crypto
// status` lints them), except that equality on a blind-index field whose index
// is incomplete (enabling/disabling) is always rejected, so a deny style rule
// cannot fail open.
func (r *runner) attachBlindIndex(collection *Collection, fieldName, modifier string, kind rule.RefKind, result *search.ResolverResult) {
	if r.resolver.dryRun {
		return // schema validation must not touch the provider or the data
	}
	p := blindIndexProviderOf(r.resolver.app)
	if p == nil {
		return
	}

	encrypted, cfgErr := p.IsEncrypted(collection.Id, fieldName)
	if cfgErr == nil && !encrypted {
		return
	}
	fail := func(reason string) error {
		return &EncryptedFieldError{Collection: collection.Name, Field: fieldName, Reason: reason}
	}

	strict := !r.resolver.allowHiddenFields || r.resolver.clientFilter
	blind := cfgErr == nil && p.IsBlindIndex(collection.Id, fieldName)
	plain := modifier == "" && kind == rule.RefColumn

	info := r.resolver.requestInfo
	// Rules are resolved with hidden fields allowed (as are superuser
	// filters); a client filter of a non superuser is not.
	enforce := !r.resolver.allowHiddenFields
	idIdentifier := fmt.Sprintf("[[%s.id]]", r.activeTableAlias)
	mm := result.MultiMatchSubQuery
	mmIdIdentifier := ""
	if mm != nil {
		mmIdIdentifier = fmt.Sprintf("[[%s.id]]", r.multiMatchActiveTableAlias)
	}
	resolver := r.resolver

	result.BeforeBuild = func(other *search.ResolverResult, op fexpr.SignOp) error {
		if cfgErr != nil {
			return fail("the encryption configuration is unavailable")
		}

		eqOp := false
		switch op {
		case fexpr.SignEq, fexpr.SignNeq, fexpr.SignAnyEq, fexpr.SignAnyNeq:
			eqOp = true
		}

		value := ""
		literal := false
		if eqOp && plain {
			m := rePlaceholderOnly.FindStringSubmatch(strings.TrimSpace(other.Identifier))
			if m != nil && len(other.Params) == 1 && other.MultiMatchSubQuery == nil {
				if v, ok := other.Params[m[1]].(string); ok && v != "" {
					value, literal = v, true
				}
			}
		}

		if !literal {
			// ciphertexts carry a random nonce: two encrypted columns (also
			// @request.auth.<encrypted field>, a join to the auth record) never
			// compare equal, so "!=" would be true for everybody.
			if eqOp && other.BeforeBuild != nil {
				return fail("cannot be compared with another encrypted field (including @request.auth.<encrypted field>)")
			}
			if strict {
				return fail("only = != ?= ?!= against a non-empty string are supported on a blind-index field")
			}
			return nil // rule: compares ciphertext as before
		}

		if !blind {
			// random mode, or blind-index field whose index is incomplete
			// (enabling/disabling): never treat it as queryable.
			if strict || p.IsBlindIndexInactive(collection.Id, fieldName) {
				return fail("the field is not queryable by value")
			}
			return nil
		}

		ids, err := resolver.blindLookup(p, collection, fieldName, value, info, enforce)
		if err != nil {
			var ef *EncryptedFieldError
			if errors.As(err, &ef) {
				return err
			}
			r.resolver.app.Logger().Warn("blind-index lookup failed", "collection", collection.Name, "field", fieldName, "error", err)
			return fail("the lookup could not be performed")
		}
		if len(ids) == 0 {
			ids = []string{"__no_match__"} // never a record id
		}

		prefix := "bi" + security.PseudorandomString(8)
		params := dbx.Params{}
		holders := make([]string, len(ids))
		for i, id := range ids {
			name := fmt.Sprintf("%s_%d", prefix, i)
			params[name] = id
			holders[i] = "{:" + name + "}"
		}
		in := strings.Join(holders, ",")
		build := func(idCol string) string {
			return fmt.Sprintf("(CASE WHEN %s IN (%s) THEN %s ELSE '' END)", idCol, in, other.Identifier)
		}

		result.Identifier = build(idIdentifier)
		if result.Params == nil {
			result.Params = dbx.Params{}
		}
		for k, v := range params {
			result.Params[k] = v
		}
		if mm != nil {
			mm.ValueIdentifier = build(mmIdIdentifier)
			if mm.Params == nil {
				mm.Params = dbx.Params{}
			}
			for k, v := range params {
				mm.Params[k] = v
			}
		}

		return nil
	}
}

// blindLookup memoizes provider lookups per resolver (collection, field,
// value, visibility mode) and caps the number of distinct lookups.
func (r *RecordFieldResolver) blindLookup(p BlindIndexProvider, col *Collection, field, value string, info *RequestInfo, enforce bool) ([]string, error) {
	key := fmt.Sprintf("%s\x00%s\x00%t\x00%s", col.Id, field, enforce, value)
	if ids, ok := r.blindCache[key]; ok {
		return ids, nil
	}
	if r.blindLookups >= MaxBlindIndexLookups {
		return nil, &EncryptedFieldError{Collection: col.Name, Field: field,
			Reason: fmt.Sprintf("too many blind-index comparisons in one expression (max %d)", MaxBlindIndexLookups)}
	}
	r.blindLookups++
	ids, err := p.BlindIndexIDs(col, field, value, info, enforce)
	if err != nil {
		return nil, err
	}
	if r.blindCache == nil {
		r.blindCache = map[string][]string{}
	}
	r.blindCache[key] = ids
	return ids, nil
}
