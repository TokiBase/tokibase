package crypto

import (
	"fmt"
	"regexp"
	"strings"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/dbutils"
)

// ErrSchemaCode is the validation error code for a refused schema change.
const ErrSchemaCode = "validation_encrypted_field_schema"

// configuredFields returns the encrypted field names of a collection straight
// from the database (never from the cache: a schema guard must not be stale).
func (m *Module) configuredFields(app kernel.App, col *core.Collection) []string {
	recs, err := app.FindAllRecords(FieldsCollection)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range recs {
		ref := r.GetString("collection")
		if ref == col.Id || ref == col.Name {
			out = append(out, r.GetString("field"))
		}
	}
	return out
}

// guardCollectionSave refuses schema changes that would silently break an
// encrypted field (rename, type change, removal, replacement under the same
// name) and views that select encrypted columns. The cure for the first group
// is `toki crypto disable <collection> <field>` before the change.
func (m *Module) guardCollectionSave(e *core.CollectionEvent) error {
	c := e.Collection
	if isCryptoSystem(c.Name) {
		return e.Next()
	}
	if c.IsView() {
		if err := m.checkView(e.App, c.ViewQuery); err != nil {
			return validation.Errors{"viewQuery": validation.NewError(ErrSchemaCode, err.Error())}
		}
		return e.Next()
	}
	if c.IsNew() {
		return e.Next()
	}
	old, err := e.App.FindCollectionByNameOrId(c.Id)
	if err != nil || old == nil {
		return e.Next()
	}
	for _, name := range m.configuredFields(e.App, old) {
		of := old.Fields.GetByName(name)
		if of == nil {
			continue // already dangling: Lint reports it
		}
		nf := c.Fields.GetById(of.GetId())
		msg := ""
		switch {
		case nf == nil && c.Fields.GetByName(name) != nil:
			msg = "the field was replaced by a different one with the same name"
		case nf == nil:
			msg = "the field cannot be deleted"
		case nf.GetName() != name:
			msg = fmt.Sprintf("the field cannot be renamed to %q", nf.GetName())
		case nf.Type() != of.Type():
			msg = fmt.Sprintf("the field type cannot change from %s to %s", of.Type(), nf.Type())
		}
		if msg != "" {
			return validation.Errors{"fields": validation.NewError(ErrSchemaCode,
				fmt.Sprintf("Encrypted field %q of %q: %s. Run `toki crypto disable %s %s --i-understand` first.",
					name, old.Name, msg, old.Name, name))}
		}
	}
	return e.Next()
}

func wordRe(w string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(w) + `($|[^A-Za-z0-9_])`)
}

// viewHits returns "collection.field" when the view query can select an
// encrypted column: it names the table and either the column or a wildcard.
func (m *Module) viewHits(app kernel.App, query string) string {
	recs, err := app.FindAllRecords(FieldsCollection)
	if err != nil {
		return ""
	}
	for _, r := range recs {
		col, err := app.FindCollectionByNameOrId(r.GetString("collection"))
		if err != nil || col == nil {
			continue
		}
		if !wordRe(col.Name).MatchString(query) {
			continue
		}
		field := r.GetString("field")
		if strings.Contains(query, "*") || wordRe(field).MatchString(query) {
			return col.Name + "." + field
		}
	}
	return ""
}

func (m *Module) checkView(app kernel.App, query string) error {
	if hit := m.viewHits(app, query); hit != "" {
		return fmt.Errorf("the view query can select the encrypted field %s: a view returns ciphertext. Select only plain fields, or run `toki crypto disable` first", hit)
	}
	return nil
}

// viewsUsing returns the names of view collections whose query can select
// col.field.
func viewsUsing(app kernel.App, col *core.Collection, field string) []string {
	views, err := app.FindAllCollections(core.CollectionTypeView)
	if err != nil {
		return nil
	}
	var out []string
	for _, v := range views {
		if wordRe(col.Name).MatchString(v.ViewQuery) &&
			(strings.Contains(v.ViewQuery, "*") || wordRe(field).MatchString(v.ViewQuery)) {
			out = append(out, v.Name)
		}
	}
	return out
}

// identityUse explains why a field of an auth collection cannot be encrypted
// ("" when it can).
func identityUse(col *core.Collection, field string) string {
	if !col.IsAuth() {
		return ""
	}
	for _, f := range col.PasswordAuth.IdentityFields {
		if f == field {
			return "it is a password-auth identity field (login compares against it)"
		}
	}
	mf := col.OAuth2.MappedFields
	if field != "" && (mf.Id == field || mf.Name == field || mf.Username == field || mf.AvatarURL == field) {
		return "it is an OAuth2 mapped field"
	}
	if field == core.FieldNameEmail || field == "username" {
		return "it is an auth identity field"
	}
	return ""
}

// indexUse explains why a field covered by an index cannot be encrypted.
func indexUse(col *core.Collection, field string) string {
	for _, ix := range col.Indexes {
		parsed := dbutils.ParseIndex(ix)
		hit := wordRe(field).MatchString(ix)
		for _, c := range parsed.Columns {
			if strings.EqualFold(c.Name, field) {
				hit = true
			}
		}
		if !hit {
			continue
		}
		if parsed.Unique {
			return fmt.Sprintf("it is covered by the unique index %q (uniqueness would apply to random ciphertext)", parsed.IndexName)
		}
		return fmt.Sprintf("it is covered by the index %q (an index over random ciphertext is useless)", parsed.IndexName)
	}
	return ""
}
