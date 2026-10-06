package kernel_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

func TestURLFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, kernel.FieldTypeURL)
}

func TestURLFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.URLField{}

	expected := "TEXT DEFAULT '' NOT NULL"

	if v := f.ColumnType(app); v != expected {
		t.Fatalf("Expected\n%q\ngot\n%q", expected, v)
	}
}

func TestURLFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.URLField{}
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

func TestURLFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name        string
		field       *kernel.URLField
		record      func() *kernel.Record
		expectError bool
	}{
		{
			"invalid raw value",
			&kernel.URLField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", 123)
				return record
			},
			true,
		},
		{
			"zero field value (not required)",
			&kernel.URLField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "")
				return record
			},
			false,
		},
		{
			"zero field value (required)",
			&kernel.URLField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "")
				return record
			},
			true,
		},
		{
			"non-zero field value (required)",
			&kernel.URLField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "https://example.com")
				return record
			},
			false,
		},
		{
			"invalid url",
			&kernel.URLField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "invalid")
				return record
			},
			true,
		},
		{
			"failed onlyDomains",
			&kernel.URLField{Name: "test", OnlyDomains: []string{"example.org", "example.net"}},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "https://example.com")
				return record
			},
			true,
		},
		{
			"success onlyDomains",
			&kernel.URLField{Name: "test", OnlyDomains: []string{"example.org", "example.com"}},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "https://example.com")
				return record
			},
			false,
		},
		{
			"failed exceptDomains",
			&kernel.URLField{Name: "test", ExceptDomains: []string{"example.org", "example.com"}},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "https://example.com")
				return record
			},
			true,
		},
		{
			"success exceptDomains",
			&kernel.URLField{Name: "test", ExceptDomains: []string{"example.org", "example.net"}},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "https://example.com")
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

func TestURLFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, kernel.FieldTypeURL)
	testDefaultFieldNameValidation(t, kernel.FieldTypeURL)
	testDefaultFieldHelpValidation[kernel.URLField](t)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name         string
		field        func() *kernel.URLField
		expectErrors []string
	}{
		{
			"zero minimal",
			func() *kernel.URLField {
				return &kernel.URLField{
					Id:   "test",
					Name: "test",
				}
			},
			[]string{},
		},
		{
			"both onlyDomains and exceptDomains",
			func() *kernel.URLField {
				return &kernel.URLField{
					Id:            "test",
					Name:          "test",
					OnlyDomains:   []string{"example.com"},
					ExceptDomains: []string{"example.org"},
				}
			},
			[]string{"onlyDomains", "exceptDomains"},
		},
		{
			"invalid onlyDomains",
			func() *kernel.URLField {
				return &kernel.URLField{
					Id:          "test",
					Name:        "test",
					OnlyDomains: []string{"example.com", "invalid"},
				}
			},
			[]string{"onlyDomains"},
		},
		{
			"valid onlyDomains",
			func() *kernel.URLField {
				return &kernel.URLField{
					Id:          "test",
					Name:        "test",
					OnlyDomains: []string{"example.com", "example.org"},
				}
			},
			[]string{},
		},
		{
			"invalid exceptDomains",
			func() *kernel.URLField {
				return &kernel.URLField{
					Id:            "test",
					Name:          "test",
					ExceptDomains: []string{"example.com", "invalid"},
				}
			},
			[]string{"exceptDomains"},
		},
		{
			"valid exceptDomains",
			func() *kernel.URLField {
				return &kernel.URLField{
					Id:            "test",
					Name:          "test",
					ExceptDomains: []string{"example.com", "example.org"},
				}
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			errs := s.field().ValidateSettings(context.Background(), app, collection)

			tests.TestValidationErrors(t, errs, s.expectErrors)
		})
	}
}
