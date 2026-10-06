package kernel_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

func TestEditorFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, kernel.FieldTypeEditor)
}

func TestEditorFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.EditorField{}

	expected := "TEXT DEFAULT '' NOT NULL"

	if v := f.ColumnType(app); v != expected {
		t.Fatalf("Expected\n%q\ngot\n%q", expected, v)
	}
}

func TestEditorFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.EditorField{}
	record := kernel.NewRecord(kernel.NewBaseCollection("test"))

	scenarios := []struct {
		raw      any
		expected string
	}{
		{"", ""},
		{"test", "test"},
		{false, "false"},
		{true, "true"},
		{123.456, "123.456"},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%#v", i, s.raw), func(t *testing.T) {
			v, err := f.PrepareValue(record, s.raw)
			if err != nil {
				t.Fatal(err)
			}

			vStr, ok := v.(string)
			if !ok {
				t.Fatalf("Expected string instance, got %T", v)
			}

			if vStr != s.expected {
				t.Fatalf("Expected %q, got %q", s.expected, v)
			}
		})
	}
}

func TestEditorFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name        string
		field       *kernel.EditorField
		record      func() *kernel.Record
		expectError bool
	}{
		{
			"invalid raw value",
			&kernel.EditorField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", 123)
				return record
			},
			true,
		},
		{
			"zero field value (not required)",
			&kernel.EditorField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "")
				return record
			},
			false,
		},
		{
			"zero field value (required)",
			&kernel.EditorField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "")
				return record
			},
			true,
		},
		{
			"non-zero field value (required)",
			&kernel.EditorField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "abc")
				return record
			},
			false,
		},
		{
			"> default MaxSize",
			&kernel.EditorField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", strings.Repeat("a", 1+(5<<20)))
				return record
			},
			true,
		},
		{
			"> MaxSize",
			&kernel.EditorField{Name: "test", Required: true, MaxSize: 5},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "abcdef")
				return record
			},
			true,
		},
		{
			"<= MaxSize",
			&kernel.EditorField{Name: "test", Required: true, MaxSize: 5},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "abcde")
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

func TestEditorFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, kernel.FieldTypeEditor)
	testDefaultFieldNameValidation(t, kernel.FieldTypeEditor)
	testDefaultFieldHelpValidation[kernel.EditorField](t)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name         string
		field        func() *kernel.EditorField
		expectErrors []string
	}{
		{
			"< 0 MaxSize",
			func() *kernel.EditorField {
				return &kernel.EditorField{
					Id:      "test",
					Name:    "test",
					MaxSize: -1,
				}
			},
			[]string{"maxSize"},
		},
		{
			"= 0 MaxSize",
			func() *kernel.EditorField {
				return &kernel.EditorField{
					Id:   "test",
					Name: "test",
				}
			},
			[]string{},
		},
		{
			"> 0 MaxSize",
			func() *kernel.EditorField {
				return &kernel.EditorField{
					Id:      "test",
					Name:    "test",
					MaxSize: 1,
				}
			},
			[]string{},
		},
		{
			"MaxSize > safe json int",
			func() *kernel.EditorField {
				return &kernel.EditorField{
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

func TestEditorFieldCalculateMaxBodySize(t *testing.T) {
	testApp, _ := tests.NewTestApp()
	defer testApp.Cleanup()

	scenarios := []struct {
		field    *kernel.EditorField
		expected int64
	}{
		{&kernel.EditorField{}, kernel.DefaultEditorFieldMaxSize},
		{&kernel.EditorField{MaxSize: 10}, 10},
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
