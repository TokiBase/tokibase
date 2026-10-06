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

func TestSelectFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, kernel.FieldTypeSelect)
}

func TestSelectFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		name     string
		field    *kernel.SelectField
		expected string
	}{
		{
			"single (zero)",
			&kernel.SelectField{},
			"TEXT DEFAULT '' NOT NULL",
		},
		{
			"single",
			&kernel.SelectField{MaxSelect: 1},
			"TEXT DEFAULT '' NOT NULL",
		},
		{
			"multiple",
			&kernel.SelectField{MaxSelect: 2},
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

func TestSelectFieldIsMultiple(t *testing.T) {
	scenarios := []struct {
		name     string
		field    *kernel.SelectField
		expected bool
	}{
		{
			"single (zero)",
			&kernel.SelectField{},
			false,
		},
		{
			"single",
			&kernel.SelectField{MaxSelect: 1},
			false,
		},
		{
			"multiple (>1)",
			&kernel.SelectField{MaxSelect: 2},
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

func TestSelectFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	record := kernel.NewRecord(kernel.NewBaseCollection("test"))

	scenarios := []struct {
		raw      any
		field    *kernel.SelectField
		expected string
	}{
		// single
		{nil, &kernel.SelectField{}, `""`},
		{"", &kernel.SelectField{}, `""`},
		{123, &kernel.SelectField{}, `"123"`},
		{"a", &kernel.SelectField{}, `"a"`},
		{`["a"]`, &kernel.SelectField{}, `"a"`},
		{[]string{}, &kernel.SelectField{}, `""`},
		{[]string{"a", "b"}, &kernel.SelectField{}, `"b"`},

		// multiple
		{nil, &kernel.SelectField{MaxSelect: 2}, `[]`},
		{"", &kernel.SelectField{MaxSelect: 2}, `[]`},
		{123, &kernel.SelectField{MaxSelect: 2}, `["123"]`},
		{"a", &kernel.SelectField{MaxSelect: 2}, `["a"]`},
		{`["a"]`, &kernel.SelectField{MaxSelect: 2}, `["a"]`},
		{[]string{}, &kernel.SelectField{MaxSelect: 2}, `[]`},
		{[]string{"a", "b", "c"}, &kernel.SelectField{MaxSelect: 2}, `["a","b","c"]`},
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

func TestSelectFieldDriverValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		raw      any
		field    *kernel.SelectField
		expected string
	}{
		// single
		{nil, &kernel.SelectField{}, `""`},
		{"", &kernel.SelectField{}, `""`},
		{123, &kernel.SelectField{}, `"123"`},
		{"a", &kernel.SelectField{}, `"a"`},
		{`["a"]`, &kernel.SelectField{}, `"a"`},
		{[]string{}, &kernel.SelectField{}, `""`},
		{[]string{"a", "b"}, &kernel.SelectField{}, `"b"`},

		// multiple
		{nil, &kernel.SelectField{MaxSelect: 2}, `[]`},
		{"", &kernel.SelectField{MaxSelect: 2}, `[]`},
		{123, &kernel.SelectField{MaxSelect: 2}, `["123"]`},
		{"a", &kernel.SelectField{MaxSelect: 2}, `["a"]`},
		{`["a"]`, &kernel.SelectField{MaxSelect: 2}, `["a"]`},
		{[]string{}, &kernel.SelectField{MaxSelect: 2}, `[]`},
		{[]string{"a", "b", "c"}, &kernel.SelectField{MaxSelect: 2}, `["a","b","c"]`},
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

func TestSelectFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	values := []string{"a", "b", "c"}

	scenarios := []struct {
		name        string
		field       *kernel.SelectField
		record      func() *kernel.Record
		expectError bool
	}{
		// single
		{
			"[single] zero field value (not required)",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 1},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "")
				return record
			},
			false,
		},
		{
			"[single] zero field value (required)",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 1, Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "")
				return record
			},
			true,
		},
		{
			"[single] unknown value",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 1},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "unknown")
				return record
			},
			true,
		},
		{
			"[single] known value",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 1},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "a")
				return record
			},
			false,
		},
		{
			"[single] > MaxSelect",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 1},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", []string{"a", "b"})
				return record
			},
			true,
		},

		// multiple
		{
			"[multiple] zero field value (not required)",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 2},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", []string{})
				return record
			},
			false,
		},
		{
			"[multiple] zero field value (required)",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 2, Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", []string{})
				return record
			},
			true,
		},
		{
			"[multiple] unknown value",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 2},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", []string{"a", "unknown"})
				return record
			},
			true,
		},
		{
			"[multiple] known value",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 2},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", []string{"a", "b"})
				return record
			},
			false,
		},
		{
			"[multiple] > MaxSelect",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 2},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", []string{"a", "b", "c"})
				return record
			},
			true,
		},
		{
			"[multiple] > MaxSelect (duplicated values)",
			&kernel.SelectField{Name: "test", Values: values, MaxSelect: 2},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", []string{"a", "b", "b", "a"})
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

func TestSelectFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, kernel.FieldTypeSelect)
	testDefaultFieldNameValidation(t, kernel.FieldTypeSelect)
	testDefaultFieldHelpValidation[kernel.SelectField](t)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		name         string
		field        func() *kernel.SelectField
		expectErrors []string
	}{
		{
			"zero minimal",
			func() *kernel.SelectField {
				return &kernel.SelectField{
					Id:   "test",
					Name: "test",
				}
			},
			[]string{"values"},
		},
		{
			"MaxSelect > Values length",
			func() *kernel.SelectField {
				return &kernel.SelectField{
					Id:        "test",
					Name:      "test",
					Values:    []string{"a", "b"},
					MaxSelect: 3,
				}
			},
			[]string{"maxSelect"},
		},
		{
			"MaxSelect <= Values length",
			func() *kernel.SelectField {
				return &kernel.SelectField{
					Id:        "test",
					Name:      "test",
					Values:    []string{"a", "b"},
					MaxSelect: 2,
				}
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			field := s.field()

			collection := kernel.NewBaseCollection("test_collection")
			collection.Fields.Add(field)

			errs := field.ValidateSettings(context.Background(), app, collection)

			tests.TestValidationErrors(t, errs, s.expectErrors)
		})
	}
}

func TestSelectFieldFindSetter(t *testing.T) {
	values := []string{"a", "b", "c", "d"}

	scenarios := []struct {
		name      string
		key       string
		value     any
		field     *kernel.SelectField
		hasSetter bool
		expected  string
	}{
		{
			"no match",
			"example",
			"b",
			&kernel.SelectField{Name: "test", MaxSelect: 1, Values: values},
			false,
			"",
		},
		{
			"exact match (single)",
			"test",
			"b",
			&kernel.SelectField{Name: "test", MaxSelect: 1, Values: values},
			true,
			`"b"`,
		},
		{
			"exact match (multiple)",
			"test",
			[]string{"a", "b"},
			&kernel.SelectField{Name: "test", MaxSelect: 2, Values: values},
			true,
			`["a","b"]`,
		},
		{
			"append (single)",
			"test+",
			"b",
			&kernel.SelectField{Name: "test", MaxSelect: 1, Values: values},
			true,
			`"b"`,
		},
		{
			"append (multiple)",
			"test+",
			[]string{"a"},
			&kernel.SelectField{Name: "test", MaxSelect: 2, Values: values},
			true,
			`["c","d","a"]`,
		},
		{
			"prepend (single)",
			"+test",
			"b",
			&kernel.SelectField{Name: "test", MaxSelect: 1, Values: values},
			true,
			`"d"`, // the last of the existing values
		},
		{
			"prepend (multiple)",
			"+test",
			[]string{"a"},
			&kernel.SelectField{Name: "test", MaxSelect: 2, Values: values},
			true,
			`["a","c","d"]`,
		},
		{
			"subtract (single)",
			"test-",
			"d",
			&kernel.SelectField{Name: "test", MaxSelect: 1, Values: values},
			true,
			`"c"`,
		},
		{
			"subtract (multiple)",
			"test-",
			[]string{"unknown", "c"},
			&kernel.SelectField{Name: "test", MaxSelect: 2, Values: values},
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
