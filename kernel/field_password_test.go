package kernel_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"golang.org/x/crypto/bcrypt"
)

func TestPasswordFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, kernel.FieldTypePassword)
}

func TestPasswordFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.PasswordField{}

	expected := "TEXT DEFAULT '' NOT NULL"

	if v := f.ColumnType(app); v != expected {
		t.Fatalf("Expected\n%q\ngot\n%q", expected, v)
	}
}

func TestPasswordFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.PasswordField{}
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

			pv, ok := v.(*kernel.PasswordFieldValue)
			if !ok {
				t.Fatalf("Expected PasswordFieldValue instance, got %T", v)
			}

			if pv.Hash != s.expected {
				t.Fatalf("Expected %q, got %q", s.expected, v)
			}
		})
	}
}

func TestPasswordFieldDriverValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.PasswordField{Name: "test"}

	err := errors.New("example_err")

	scenarios := []struct {
		raw      any
		expected *kernel.PasswordFieldValue
	}{
		{123, &kernel.PasswordFieldValue{}},
		{"abc", &kernel.PasswordFieldValue{}},
		{"$2abc", &kernel.PasswordFieldValue{Hash: "$2abc"}},
		{&kernel.PasswordFieldValue{Hash: "test", LastError: err}, &kernel.PasswordFieldValue{Hash: "test", LastError: err}},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%v", i, s.raw), func(t *testing.T) {
			record := kernel.NewRecord(kernel.NewBaseCollection("test"))
			record.SetRaw(f.GetName(), s.raw)

			v, err := f.DriverValue(record)

			vStr, ok := v.(string)
			if !ok {
				t.Fatalf("Expected string instance, got %T", v)
			}

			var errStr string
			if err != nil {
				errStr = err.Error()
			}

			var expectedErrStr string
			if s.expected.LastError != nil {
				expectedErrStr = s.expected.LastError.Error()
			}

			if errStr != expectedErrStr {
				t.Fatalf("Expected error %q, got %q", expectedErrStr, errStr)
			}

			if vStr != s.expected.Hash {
				t.Fatalf("Expected hash %q, got %q", s.expected.Hash, vStr)
			}
		})
	}
}

func TestPasswordFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name        string
		field       *kernel.PasswordField
		record      func() *kernel.Record
		expectError bool
	}{
		{
			"invalid raw value",
			&kernel.PasswordField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", "123")
				return record
			},
			true,
		},
		{
			"zero field value (not required)",
			&kernel.PasswordField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{})
				return record
			},
			false,
		},
		{
			"zero field value (required)",
			&kernel.PasswordField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{})
				return record
			},
			true,
		},
		{
			"empty hash but non-empty plain password (required)",
			&kernel.PasswordField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Plain: "test"})
				return record
			},
			true,
		},
		{
			"non-empty hash (required)",
			&kernel.PasswordField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Hash: "test"})
				return record
			},
			false,
		},
		{
			"with LastError",
			&kernel.PasswordField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{LastError: errors.New("test")})
				return record
			},
			true,
		},
		{
			"< Min",
			&kernel.PasswordField{Name: "test", Min: 3},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Plain: "аб"}) // multi-byte chars test
				return record
			},
			true,
		},
		{
			">= Min",
			&kernel.PasswordField{Name: "test", Min: 3},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Plain: "абв"}) // multi-byte chars test
				return record
			},
			false,
		},
		{
			"> default Max",
			&kernel.PasswordField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Plain: strings.Repeat("a", 72)})
				return record
			},
			true,
		},
		{
			"<= default Max",
			&kernel.PasswordField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Plain: strings.Repeat("a", 71)})
				return record
			},
			false,
		},
		{
			"> Max",
			&kernel.PasswordField{Name: "test", Max: 2},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Plain: "абв"}) // multi-byte chars test
				return record
			},
			true,
		},
		{
			"<= Max",
			&kernel.PasswordField{Name: "test", Max: 2},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Plain: "аб"}) // multi-byte chars test
				return record
			},
			false,
		},
		{
			"non-matching pattern",
			&kernel.PasswordField{Name: "test", Pattern: `\d+`},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Plain: "abc"})
				return record
			},
			true,
		},
		{
			"matching pattern",
			&kernel.PasswordField{Name: "test", Pattern: `\d+`},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", &kernel.PasswordFieldValue{Plain: "123"})
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

func TestPasswordFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, kernel.FieldTypePassword)
	testDefaultFieldNameValidation(t, kernel.FieldTypePassword)
	testDefaultFieldHelpValidation[kernel.PasswordField](t)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		name         string
		field        func(col *kernel.Collection) *kernel.PasswordField
		expectErrors []string
	}{
		{
			"zero minimal",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
				}
			},
			[]string{},
		},
		{
			"invalid pattern",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:      "test",
					Name:    "test",
					Pattern: "(invalid",
				}
			},
			[]string{"pattern"},
		},
		{
			"valid pattern",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:      "test",
					Name:    "test",
					Pattern: `\d+`,
				}
			},
			[]string{},
		},
		{
			"Min < 0",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
					Min:  -1,
				}
			},
			[]string{"min"},
		},
		{
			"Min > 71",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
					Min:  72,
				}
			},
			[]string{"min"},
		},
		{
			"valid Min",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
					Min:  5,
				}
			},
			[]string{},
		},
		{
			"Max < Min",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
					Min:  2,
					Max:  1,
				}
			},
			[]string{"max"},
		},
		{
			"Min > Min",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
					Min:  2,
					Max:  3,
				}
			},
			[]string{},
		},
		{
			"Max > 71",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
					Max:  72,
				}
			},
			[]string{"max"},
		},
		{
			"cost < bcrypt.MinCost",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
					Cost: bcrypt.MinCost - 1,
				}
			},
			[]string{"cost"},
		},
		{
			"cost > bcrypt.MaxCost",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
					Cost: bcrypt.MaxCost + 1,
				}
			},
			[]string{"cost"},
		},
		{
			"valid cost",
			func(col *kernel.Collection) *kernel.PasswordField {
				return &kernel.PasswordField{
					Id:   "test",
					Name: "test",
					Cost: 12,
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

func TestPasswordFieldFindSetter(t *testing.T) {
	scenarios := []struct {
		name      string
		key       string
		value     any
		field     *kernel.PasswordField
		hasSetter bool
		expected  string
	}{
		{
			"no match",
			"example",
			"abc",
			&kernel.PasswordField{Name: "test"},
			false,
			"",
		},
		{
			"exact match",
			"test",
			"abc",
			&kernel.PasswordField{Name: "test"},
			true,
			`"abc"`,
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

func TestPasswordFieldFindGetter(t *testing.T) {
	scenarios := []struct {
		name      string
		key       string
		field     *kernel.PasswordField
		hasGetter bool
		expected  string
	}{
		{
			"no match",
			"example",
			&kernel.PasswordField{Name: "test"},
			false,
			"",
		},
		{
			"field name match",
			"test",
			&kernel.PasswordField{Name: "test"},
			true,
			"test_plain",
		},
		{
			"field name hash modifier",
			"test:hash",
			&kernel.PasswordField{Name: "test"},
			true,
			"test_hash",
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			collection := kernel.NewBaseCollection("test_collection")
			collection.Fields.Add(s.field)

			getter := s.field.FindGetter(s.key)

			hasGetter := getter != nil
			if hasGetter != s.hasGetter {
				t.Fatalf("Expected hasGetter %v, got %v", s.hasGetter, hasGetter)
			}

			if !hasGetter {
				return
			}

			record := kernel.NewRecord(collection)
			record.SetRaw(s.field.GetName(), &kernel.PasswordFieldValue{Hash: "test_hash", Plain: "test_plain"})

			result := getter(record)

			if result != s.expected {
				t.Fatalf("Expected %q, got %#v", s.expected, result)
			}
		})
	}
}
