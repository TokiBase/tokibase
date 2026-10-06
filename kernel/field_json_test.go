package kernel_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/types"
)

func TestJSONFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, kernel.FieldTypeJSON)
}

func TestJSONFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.JSONField{}

	expected := "JSON DEFAULT NULL"

	if v := f.ColumnType(app); v != expected {
		t.Fatalf("Expected\n%q\ngot\n%q", expected, v)
	}
}

func TestJSONFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.JSONField{}
	record := kernel.NewRecord(kernel.NewBaseCollection("test"))

	scenarios := []struct {
		raw      any
		expected string
	}{
		{"null", `null`},
		{"", `""`},
		{"true", `true`},
		{"false", `false`},
		{"test", `"test"`},
		{"123", `123`},
		{"-456", `-456`},
		{"[1,2,3]", `[1,2,3]`},
		{"[1,2,3", `"[1,2,3"`},
		{`{"a":1,"b":2}`, `{"a":1,"b":2}`},
		{`{"a":1,"b":2`, `"{\"a\":1,\"b\":2"`},
		{[]int{1, 2, 3}, `[1,2,3]`},
		{map[string]int{"a": 1, "b": 2}, `{"a":1,"b":2}`},
		{nil, `null`},
		{false, `false`},
		{true, `true`},
		{-78, `-78`},
		{123.456, `123.456`},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%#v", i, s.raw), func(t *testing.T) {
			v, err := f.PrepareValue(record, s.raw)
			if err != nil {
				t.Fatal(err)
			}

			raw, ok := v.(types.JSONRaw)
			if !ok {
				t.Fatalf("Expected string instance, got %T", v)
			}
			rawStr := raw.String()

			if rawStr != s.expected {
				t.Fatalf("Expected\n%#v\ngot\n%#v", s.expected, rawStr)
			}
		})
	}
}

func TestJSONFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name        string
		field       *kernel.JSONField
		record      func() *kernel.Record
		expectError bool
	}{
		{
			"invalid raw value",
			&kernel.JSONField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", 123)
				return record
			},
			true,
		},
		{
			"invalid raw value (v2 semantics)",
			&kernel.JSONField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.JSONRaw(`{"a": 1, "a": 2}`))
				return record
			},
			true,
		},
		{
			"zero field value (not required)",
			&kernel.JSONField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.JSONRaw{})
				return record
			},
			false,
		},
		{
			"zero field value (required)",
			&kernel.JSONField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.JSONRaw{})
				return record
			},
			true,
		},
		{
			"non-zero field value (required)",
			&kernel.JSONField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.JSONRaw("[1,2,3]"))
				return record
			},
			false,
		},
		{
			"non-zero field value (required)",
			&kernel.JSONField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.JSONRaw(`"aaa"`))
				return record
			},
			false,
		},
		{
			"> default MaxSize",
			&kernel.JSONField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.JSONRaw(`"`+strings.Repeat("a", (1<<20))+`"`))
				return record
			},
			true,
		},
		{
			"> MaxSize",
			&kernel.JSONField{Name: "test", MaxSize: 5},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.JSONRaw(`"aaaa"`))
				return record
			},
			true,
		},
		{
			"<= MaxSize",
			&kernel.JSONField{Name: "test", MaxSize: 5},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.JSONRaw(`"aaa"`))
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

func TestJSONFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, kernel.FieldTypeJSON)
	testDefaultFieldNameValidation(t, kernel.FieldTypeJSON)
	testDefaultFieldHelpValidation[kernel.JSONField](t)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name         string
		field        func() *kernel.JSONField
		expectErrors []string
	}{
		{
			"MaxSize < 0",
			func() *kernel.JSONField {
				return &kernel.JSONField{
					Id:      "test",
					Name:    "test",
					MaxSize: -1,
				}
			},
			[]string{"maxSize"},
		},
		{
			"MaxSize = 0",
			func() *kernel.JSONField {
				return &kernel.JSONField{
					Id:   "test",
					Name: "test",
				}
			},
			[]string{},
		},
		{
			"MaxSize > 0",
			func() *kernel.JSONField {
				return &kernel.JSONField{
					Id:      "test",
					Name:    "test",
					MaxSize: 1,
				}
			},
			[]string{},
		},
		{
			"MaxSize > safe json int",
			func() *kernel.JSONField {
				return &kernel.JSONField{
					Id:      "test",
					Name:    "test",
					MaxSize: 1 << 53,
				}
			},
			[]string{"maxSize"},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			errs := s.field().ValidateSettings(context.Background(), app, collection)

			tests.TestValidationErrors(t, errs, s.expectErrors)
		})
	}
}

func TestJSONFieldCalculateMaxBodySize(t *testing.T) {
	testApp, _ := tests.NewTestApp()
	defer testApp.Cleanup()

	scenarios := []struct {
		field    *kernel.JSONField
		expected int64
	}{
		{&kernel.JSONField{}, kernel.DefaultJSONFieldMaxSize},
		{&kernel.JSONField{MaxSize: 10}, 10},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%d", i, s.field.MaxSize), func(t *testing.T) {
			result := s.field.CalculateMaxBodySize()

			if result != s.expected {
				t.Fatalf("Expected %d, got %d", s.expected, result)
			}
		})
	}
}
