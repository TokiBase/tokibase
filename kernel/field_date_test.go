package kernel_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/types"
)

func TestDateFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, kernel.FieldTypeDate)
}

func TestDateFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.DateField{}

	expected := "TEXT DEFAULT '' NOT NULL"

	if v := f.ColumnType(app); v != expected {
		t.Fatalf("Expected\n%q\ngot\n%q", expected, v)
	}
}

func TestDateFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.DateField{}
	record := kernel.NewRecord(kernel.NewBaseCollection("test"))

	scenarios := []struct {
		raw      any
		expected string
	}{
		{"", ""},
		{"invalid", ""},
		{"2024-01-01 00:11:22.345Z", "2024-01-01 00:11:22.345Z"},
		{time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), "2024-01-02 03:04:05.000Z"},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%#v", i, s.raw), func(t *testing.T) {
			v, err := f.PrepareValue(record, s.raw)
			if err != nil {
				t.Fatal(err)
			}

			vDate, ok := v.(types.DateTime)
			if !ok {
				t.Fatalf("Expected types.DateTime instance, got %T", v)
			}

			if vDate.String() != s.expected {
				t.Fatalf("Expected %v, got %v", s.expected, v)
			}
		})
	}
}

func TestDateFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name        string
		field       *kernel.DateField
		record      func() *kernel.Record
		expectError bool
	}{
		{
			"invalid raw value",
			&kernel.DateField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", 123)
				return record
			},
			true,
		},
		{
			"zero field value (not required)",
			&kernel.DateField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.DateTime{})
				return record
			},
			false,
		},
		{
			"zero field value (required)",
			&kernel.DateField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.DateTime{})
				return record
			},
			true,
		},
		{
			"non-zero field value (required)",
			&kernel.DateField{Name: "test", Required: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", types.NowDateTime())
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

func TestDateFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, kernel.FieldTypeDate)
	testDefaultFieldNameValidation(t, kernel.FieldTypeDate)
	testDefaultFieldHelpValidation[kernel.DateField](t)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name         string
		field        func() *kernel.DateField
		expectErrors []string
	}{
		{
			"zero Min/Max",
			func() *kernel.DateField {
				return &kernel.DateField{
					Id:   "test",
					Name: "test",
				}
			},
			[]string{},
		},
		{
			"non-empty Min with empty Max",
			func() *kernel.DateField {
				return &kernel.DateField{
					Id:   "test",
					Name: "test",
					Min:  types.NowDateTime(),
				}
			},
			[]string{},
		},
		{
			"empty Min non-empty Max",
			func() *kernel.DateField {
				return &kernel.DateField{
					Id:   "test",
					Name: "test",
					Max:  types.NowDateTime(),
				}
			},
			[]string{},
		},
		{
			"Min = Max",
			func() *kernel.DateField {
				date := types.NowDateTime()
				return &kernel.DateField{
					Id:   "test",
					Name: "test",
					Min:  date,
					Max:  date,
				}
			},
			[]string{},
		},
		{
			"Min > Max",
			func() *kernel.DateField {
				min := types.NowDateTime()
				max := min.Add(-5 * time.Second)
				return &kernel.DateField{
					Id:   "test",
					Name: "test",
					Min:  min,
					Max:  max,
				}
			},
			[]string{},
		},
		{
			"Min < Max",
			func() *kernel.DateField {
				max := types.NowDateTime()
				min := max.Add(-5 * time.Second)
				return &kernel.DateField{
					Id:   "test",
					Name: "test",
					Min:  min,
					Max:  max,
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
