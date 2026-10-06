package kernel_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/types"
)

func TestRelationFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, kernel.FieldTypeRelation)
}

func TestRelationFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		name     string
		field    *kernel.RelationField
		expected string
	}{
		{
			"single (zero)",
			&kernel.RelationField{},
			"TEXT DEFAULT '' NOT NULL",
		},
		{
			"single",
			&kernel.RelationField{MaxSelect: 1},
			"TEXT DEFAULT '' NOT NULL",
		},
		{
			"multiple",
			&kernel.RelationField{MaxSelect: 2},
			"JSON DEFAULT '[]' NOT NULL",
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			if v := s.field.ColumnType(app); v != s.expected {
				t.Fatalf("Expected\n%q\ngot\n%q", s.expected, v)
			}
		})
	}
}

func TestRelationFieldIsMultiple(t *testing.T) {
	scenarios := []struct {
		name     string
		field    *kernel.RelationField
		expected bool
	}{
		{
			"zero",
			&kernel.RelationField{},
			false,
		},
		{
			"single",
			&kernel.RelationField{MaxSelect: 1},
			false,
		},
		{
			"multiple",
			&kernel.RelationField{MaxSelect: 2},
			true,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			if v := s.field.IsMultiple(); v != s.expected {
				t.Fatalf("Expected %v, got %v", s.expected, v)
			}
		})
	}
}

func TestRelationFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	record := kernel.NewRecord(kernel.NewBaseCollection("test"))

	scenarios := []struct {
		raw      any
		field    *kernel.RelationField
		expected string
	}{
		// single
		{nil, &kernel.RelationField{MaxSelect: 1}, `""`},
		{"", &kernel.RelationField{MaxSelect: 1}, `""`},
		{123, &kernel.RelationField{MaxSelect: 1}, `"123"`},
		{"a", &kernel.RelationField{MaxSelect: 1}, `"a"`},
		{`["a"]`, &kernel.RelationField{MaxSelect: 1}, `"a"`},
		{[]string{}, &kernel.RelationField{MaxSelect: 1}, `""`},
		{[]string{"a", "b"}, &kernel.RelationField{MaxSelect: 1}, `"b"`},

		// multiple
		{nil, &kernel.RelationField{MaxSelect: 2}, `[]`},
		{"", &kernel.RelationField{MaxSelect: 2}, `[]`},
		{123, &kernel.RelationField{MaxSelect: 2}, `["123"]`},
		{"a", &kernel.RelationField{MaxSelect: 2}, `["a"]`},
		{`["a"]`, &kernel.RelationField{MaxSelect: 2}, `["a"]`},
		{[]string{}, &kernel.RelationField{MaxSelect: 2}, `[]`},
		{[]string{"a", "b", "c"}, &kernel.RelationField{MaxSelect: 2}, `["a","b","c"]`},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%#v_%v", i, s.raw, s.field.IsMultiple()), func(t *testing.T) {
			v, err := s.field.PrepareValue(record, s.raw)
			if err != nil {
				t.Fatal(err)
			}

			vRaw, err := json.Marshal(v, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}

			if string(vRaw) != s.expected {
				t.Fatalf("Expected %q, got %q", s.expected, vRaw)
			}
		})
	}
}

func TestRelationFieldDriverValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		raw      any
		field    *kernel.RelationField
		expected string
	}{
		// single
		{nil, &kernel.RelationField{MaxSelect: 1}, `""`},
		{"", &kernel.RelationField{MaxSelect: 1}, `""`},
		{123, &kernel.RelationField{MaxSelect: 1}, `"123"`},
		{"a", &kernel.RelationField{MaxSelect: 1}, `"a"`},
		{`["a"]`, &kernel.RelationField{MaxSelect: 1}, `"a"`},
		{[]string{}, &kernel.RelationField{MaxSelect: 1}, `""`},
		{[]string{"a", "b"}, &kernel.RelationField{MaxSelect: 1}, `"b"`},

		// multiple
		{nil, &kernel.RelationField{MaxSelect: 2}, `[]`},
		{"", &kernel.RelationField{MaxSelect: 2}, `[]`},
		{123, &kernel.RelationField{MaxSelect: 2}, `["123"]`},
		{"a", &kernel.RelationField{MaxSelect: 2}, `["a"]`},
		{`["a"]`, &kernel.RelationField{MaxSelect: 2}, `["a"]`},
		{[]string{}, &kernel.RelationField{MaxSelect: 2}, `[]`},
		{[]string{"a", "b", "c"}, &kernel.RelationField{MaxSelect: 2}, `["a","b","c"]`},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%#v_%v", i, s.raw, s.field.IsMultiple()), func(t *testing.T) {
			record := kernel.NewRecord(kernel.NewBaseCollection("test"))
			record.SetRaw(s.field.GetName(), s.raw)

			v, err := s.field.DriverValue(record)
			if err != nil {
				t.Fatal(err)
			}

			if s.field.IsMultiple() {
				_, ok := v.(types.JSONArray[string])
				if !ok {
					t.Fatalf("Expected types.JSONArray value, got %T", v)
				}
			} else {
				_, ok := v.(string)
				if !ok {
					t.Fatalf("Expected string value, got %T", v)
				}
			}

			vRaw, err := json.Marshal(v, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}

			if string(vRaw) != s.expected {
				t.Fatalf("Expected %q, got %q", s.expected, vRaw)
			}
		})
	}
}

func TestRelationFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	demo1, err := app.FindCollectionByNameOrId("demo1")
	if err != nil {
		t.Fatal(err)
	}

	scenarios := []struct {
		name        string
		field       *kernel.RelationField
		record      func() *kernel.Record
		expectError bool
	}{
		// single
		{
			"[single] zero field value (not required)",
			&kernel.RelationField{Name: "test", MaxSelect: 1, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", "")
				return record
			},
			false,
		},
		{
			"[single] zero field value (required)",
			&kernel.RelationField{Name: "test", MaxSelect: 1, CollectionId: demo1.Id, Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", "")
				return record
			},
			true,
		},
		{
			"[single] id from other collection",
			&kernel.RelationField{Name: "test", MaxSelect: 1, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", "achvryl401bhse3")
				return record
			},
			true,
		},
		{
			"[single] valid id",
			&kernel.RelationField{Name: "test", MaxSelect: 1, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", "84nmscqy84lsi1t")
				return record
			},
			false,
		},
		{
			"[single] > MaxSelect",
			&kernel.RelationField{Name: "test", MaxSelect: 1, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", []string{"84nmscqy84lsi1t", "al1h9ijdeojtsjy"})
				return record
			},
			true,
		},

		// multiple
		{
			"[multiple] zero field value (not required)",
			&kernel.RelationField{Name: "test", MaxSelect: 2, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", []string{})
				return record
			},
			false,
		},
		{
			"[multiple] zero field value (required)",
			&kernel.RelationField{Name: "test", MaxSelect: 2, CollectionId: demo1.Id, Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", []string{})
				return record
			},
			true,
		},
		{
			"[multiple] id from other collection",
			&kernel.RelationField{Name: "test", MaxSelect: 2, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", []string{"84nmscqy84lsi1t", "achvryl401bhse3"})
				return record
			},
			true,
		},
		{
			"[multiple] valid id",
			&kernel.RelationField{Name: "test", MaxSelect: 2, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", []string{"84nmscqy84lsi1t", "al1h9ijdeojtsjy"})
				return record
			},
			false,
		},
		{
			"[multiple] > MaxSelect",
			&kernel.RelationField{Name: "test", MaxSelect: 2, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", []string{"84nmscqy84lsi1t", "al1h9ijdeojtsjy", "imy661ixudk5izi"})
				return record
			},
			true,
		},
		{
			"[multiple] < MinSelect",
			&kernel.RelationField{Name: "test", MinSelect: 2, MaxSelect: 99, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", []string{"84nmscqy84lsi1t"})
				return record
			},
			true,
		},
		{
			"[multiple] >= MinSelect",
			&kernel.RelationField{Name: "test", MinSelect: 2, MaxSelect: 99, CollectionId: demo1.Id},
			func() *kernel.Record {
				record := kernel.NewRecord(kernel.NewBaseCollection("test_collection"))
				record.SetRaw("test", []string{"84nmscqy84lsi1t", "al1h9ijdeojtsjy", "imy661ixudk5izi"})
				return record
			},
			false,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			err := s.field.ValidateValue(context.Background(), app, s.record())

			hasErr := err != nil
			if hasErr != s.expectError {
				t.Fatalf("Expected hasErr %v, got %v (%v)", s.expectError, hasErr, err)
			}
		})
	}
}

func TestRelationFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, kernel.FieldTypeRelation)
	testDefaultFieldNameValidation(t, kernel.FieldTypeRelation)
	testDefaultFieldHelpValidation[kernel.RelationField](t)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	demo1, err := app.FindCollectionByNameOrId("demo1")
	if err != nil {
		t.Fatal(err)
	}

	scenarios := []struct {
		name         string
		field        func(col *kernel.Collection) *kernel.RelationField
		expectErrors []string
	}{
		{
			"zero minimal",
			func(col *kernel.Collection) *kernel.RelationField {
				return &kernel.RelationField{
					Id:   "test",
					Name: "test",
				}
			},
			[]string{"collectionId"},
		},
		{
			"invalid collectionId",
			func(col *kernel.Collection) *kernel.RelationField {
				return &kernel.RelationField{
					Id:           "test",
					Name:         "test",
					CollectionId: demo1.Name,
				}
			},
			[]string{"collectionId"},
		},
		{
			"valid collectionId",
			func(col *kernel.Collection) *kernel.RelationField {
				return &kernel.RelationField{
					Id:           "test",
					Name:         "test",
					CollectionId: demo1.Id,
				}
			},
			[]string{},
		},
		{
			"base->view",
			func(col *kernel.Collection) *kernel.RelationField {
				return &kernel.RelationField{
					Id:           "test",
					Name:         "test",
					CollectionId: "v9gwnfh02gjq1q0",
				}
			},
			[]string{"collectionId"},
		},
		{
			"view->view",
			func(col *kernel.Collection) *kernel.RelationField {
				col.Type = kernel.CollectionTypeView
				return &kernel.RelationField{
					Id:           "test",
					Name:         "test",
					CollectionId: "v9gwnfh02gjq1q0",
				}
			},
			[]string{},
		},
		{
			"MinSelect < 0",
			func(col *kernel.Collection) *kernel.RelationField {
				return &kernel.RelationField{
					Id:           "test",
					Name:         "test",
					CollectionId: demo1.Id,
					MinSelect:    -1,
				}
			},
			[]string{"minSelect"},
		},
		{
			"MinSelect > 0",
			func(col *kernel.Collection) *kernel.RelationField {
				return &kernel.RelationField{
					Id:           "test",
					Name:         "test",
					CollectionId: demo1.Id,
					MinSelect:    1,
				}
			},
			[]string{"maxSelect"},
		},
		{
			"MaxSelect < MinSelect",
			func(col *kernel.Collection) *kernel.RelationField {
				return &kernel.RelationField{
					Id:           "test",
					Name:         "test",
					CollectionId: demo1.Id,
					MinSelect:    2,
					MaxSelect:    1,
				}
			},
			[]string{"maxSelect"},
		},
		{
			"MaxSelect >= MinSelect",
			func(col *kernel.Collection) *kernel.RelationField {
				return &kernel.RelationField{
					Id:           "test",
					Name:         "test",
					CollectionId: demo1.Id,
					MinSelect:    2,
					MaxSelect:    2,
				}
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			collection := kernel.NewBaseCollection("test_collection")
			collection.Fields.GetByName("id").SetId("test") // set a dummy known id so that it can be replaced

			field := s.field(collection)

			collection.Fields.Add(field)

			errs := field.ValidateSettings(context.Background(), app, collection)

			tests.TestValidationErrors(t, errs, s.expectErrors)
		})
	}
}

func TestRelationFieldFindSetter(t *testing.T) {
	scenarios := []struct {
		name      string
		key       string
		value     any
		field     *kernel.RelationField
		hasSetter bool
		expected  string
	}{
		{
			"no match",
			"example",
			"b",
			&kernel.RelationField{Name: "test", MaxSelect: 1},
			false,
			"",
		},
		{
			"exact match (single)",
			"test",
			"b",
			&kernel.RelationField{Name: "test", MaxSelect: 1},
			true,
			`"b"`,
		},
		{
			"exact match (multiple)",
			"test",
			[]string{"a", "b"},
			&kernel.RelationField{Name: "test", MaxSelect: 2},
			true,
			`["a","b"]`,
		},
		{
			"append (single)",
			"test+",
			"b",
			&kernel.RelationField{Name: "test", MaxSelect: 1},
			true,
			`"b"`,
		},
		{
			"append (multiple)",
			"test+",
			[]string{"a"},
			&kernel.RelationField{Name: "test", MaxSelect: 2},
			true,
			`["c","d","a"]`,
		},
		{
			"prepend (single)",
			"+test",
			"b",
			&kernel.RelationField{Name: "test", MaxSelect: 1},
			true,
			`"d"`, // the last of the existing values
		},
		{
			"prepend (multiple)",
			"+test",
			[]string{"a"},
			&kernel.RelationField{Name: "test", MaxSelect: 2},
			true,
			`["a","c","d"]`,
		},
		{
			"subtract (single)",
			"test-",
			"d",
			&kernel.RelationField{Name: "test", MaxSelect: 1},
			true,
			`"c"`,
		},
		{
			"subtract (multiple)",
			"test-",
			[]string{"unknown", "c"},
			&kernel.RelationField{Name: "test", MaxSelect: 2},
			true,
			`["d"]`,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			collection := kernel.NewBaseCollection("test_collection")
			collection.Fields.Add(s.field)

			setter := s.field.FindSetter(s.key)

			hasSetter := setter != nil
			if hasSetter != s.hasSetter {
				t.Fatalf("Expected hasSetter %v, got %v", s.hasSetter, hasSetter)
			}

			if !hasSetter {
				return
			}

			record := kernel.NewRecord(collection)
			record.SetRaw(s.field.GetName(), []string{"c", "d"})

			setter(record, s.value)

			raw, err := json.Marshal(record.Get(s.field.GetName()), json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			rawStr := string(raw)

			if rawStr != s.expected {
				t.Fatalf("Expected %q, got %q", s.expected, rawStr)
			}
		})
	}
}
