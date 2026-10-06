package kernel_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

func TestEmailFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, kernel.FieldTypeEmail)
}

func TestEmailFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.EmailField{}

	expected := "TEXT DEFAULT '' NOT NULL"

	if v := f.ColumnType(app); v != expected {
		t.Fatalf("Expected\n%q\ngot\n%q", expected, v)
	}
}

func TestEmailFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.EmailField{}
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

func TestEmailFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name        string
		field       *kernel.EmailField
		record      func() *kernel.Record
		expectError bool
	}{
		{
			"invalid raw value",
			&kernel.EmailField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", 123)
				return record
			},
			true,
		},
		{
			"zero field value (not required)",
			&kernel.EmailField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "")
				return record
			},
			false,
		},
		{
			"zero field value (required)",
			&kernel.EmailField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "")
				return record
			},
			true,
		},
		{
			"non-zero field value (required)",
			&kernel.EmailField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "test@example.com")
				return record
			},
			false,
		},
		{
			"invalid email",
			&kernel.EmailField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "invalid")
				return record
			},
			true,
		},
		{
			"failed onlyDomains",
			&kernel.EmailField{Name: "test", OnlyDomains: []string{"example.org", "example.net"}},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "test@example.com")
				return record
			},
			true,
		},
		{
			"success onlyDomains",
			&kernel.EmailField{Name: "test", OnlyDomains: []string{"example.org", "example.com"}},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "test@example.com")
				return record
			},
			false,
		},
		{
			"failed exceptDomains",
			&kernel.EmailField{Name: "test", ExceptDomains: []string{"example.org", "example.com"}},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "test@example.com")
				return record
			},
			true,
		},
		{
			"success exceptDomains",
			&kernel.EmailField{Name: "test", ExceptDomains: []string{"example.org", "example.net"}},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "test@example.com")
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

func TestEmailFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, kernel.FieldTypeEmail)
	testDefaultFieldNameValidation(t, kernel.FieldTypeEmail)
	testDefaultFieldHelpValidation[kernel.EmailField](t)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name         string
		field        func() *kernel.EmailField
		expectErrors []string
	}{
		{
			"zero minimal",
			func() *kernel.EmailField {
				return &kernel.EmailField{
					Id:   "test",
					Name: "test",
				}
			},
			[]string{},
		},
		{
			"both onlyDomains and exceptDomains",
			func() *kernel.EmailField {
				return &kernel.EmailField{
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
			func() *kernel.EmailField {
				return &kernel.EmailField{
					Id:          "test",
					Name:        "test",
					OnlyDomains: []string{"example.com", "invalid"},
				}
			},
			[]string{"onlyDomains"},
		},
		{
			"valid onlyDomains",
			func() *kernel.EmailField {
				return &kernel.EmailField{
					Id:          "test",
					Name:        "test",
					OnlyDomains: []string{"example.com", "example.org"},
				}
			},
			[]string{},
		},
		{
			"invalid exceptDomains",
			func() *kernel.EmailField {
				return &kernel.EmailField{
					Id:            "test",
					Name:          "test",
					ExceptDomains: []string{"example.com", "invalid"},
				}
			},
			[]string{"exceptDomains"},
		},
		{
			"valid exceptDomains",
			func() *kernel.EmailField {
				return &kernel.EmailField{
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
