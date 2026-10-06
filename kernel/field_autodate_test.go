package kernel_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

func TestAutodateFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, kernel.FieldTypeAutodate)
}

func TestAutodateFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.AutodateField{}

	expected := "TEXT DEFAULT '' NOT NULL"

	if v := f.ColumnType(app); v != expected {
		t.Fatalf("Expected\n%q\ngot\n%q", expected, v)
	}
}

func TestAutodateFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &kernel.AutodateField{}
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

func TestAutodateFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name        string
		field       *kernel.AutodateField
		record      func() *kernel.Record
		expectError bool
	}{
		{
			"invalid raw value",
			&kernel.AutodateField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", 123)
				return record
			},
			false,
		},
		{
			"missing field value",
			&kernel.AutodateField{Name: "test"},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("abc", true)
				return record
			},
			false,
		},
		{
			"existing field value",
			&kernel.AutodateField{Name: "test"},
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

func TestAutodateFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, kernel.FieldTypeAutodate)
	testDefaultFieldNameValidation(t, kernel.FieldTypeAutodate)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	superusers, err := app.FindCollectionByNameOrId(kernel.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}

	scenarios := []struct {
		name         string
		field        func() *kernel.AutodateField
		expectErrors []string
	}{
		{
			"empty onCreate and onUpdate",
			func() *kernel.AutodateField {
				return &kernel.AutodateField{
					Id:   "test",
					Name: "test",
				}
			},
			[]string{"onCreate", "onUpdate"},
		},
		{
			"with onCreate",
			func() *kernel.AutodateField {
				return &kernel.AutodateField{
					Id:       "test",
					Name:     "test",
					OnCreate: true,
				}
			},
			[]string{},
		},
		{
			"with onUpdate",
			func() *kernel.AutodateField {
				return &kernel.AutodateField{
					Id:       "test",
					Name:     "test",
					OnUpdate: true,
				}
			},
			[]string{},
		},
		{
			"change of a system autodate field",
			func() *kernel.AutodateField {
				created := superusers.Fields.GetByName("created").(*kernel.AutodateField)
				created.OnCreate = !created.OnCreate
				created.OnUpdate = !created.OnUpdate
				return created
			},
			[]string{"onCreate", "onUpdate"},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			errs := s.field().ValidateSettings(context.Background(), app, superusers)

			tests.TestValidationErrors(t, errs, s.expectErrors)
		})
	}
}

func TestAutodateFieldFindSetter(t *testing.T) {
	field := &kernel.AutodateField{Name: "test"}

	collection := kernel.NewBaseCollection("test_collection")
	collection.Fields.Add(field)

	initialDate, err := types.ParseDateTime("2024-01-02 03:04:05.789Z")
	if err != nil {
		t.Fatal(err)
	}

	record := kernel.NewRecord(collection)
	record.SetRaw("test", initialDate)

	t.Run("no matching setter", func(t *testing.T) {
		f := field.FindSetter("abc")
		if f != nil {
			t.Fatal("Expected nil setter")
		}
	})

	t.Run("matching setter", func(t *testing.T) {
		f := field.FindSetter("test")
		if f == nil {
			t.Fatal("Expected non-nil setter")
		}

		f(record, types.NowDateTime()) // should be ignored

		if v := record.GetString("test"); v != "2024-01-02 03:04:05.789Z" {
			t.Fatalf("Expected no value change, got %q", v)
		}
	})
}

func cutMilliseconds(datetime string) string {
	if len(datetime) > 19 {
		return datetime[:19]
	}
	return datetime
}

func TestAutodateFieldIntercept(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	initialDate, err := types.ParseDateTime("2024-01-02 03:04:05.789Z")
	if err != nil {
		t.Fatal(err)
	}

	collection := kernel.NewBaseCollection("test_collection")

	scenarios := []struct {
		name       string
		actionName string
		field      *kernel.AutodateField
		record     func() *kernel.Record
		expected   string
	}{
		{
			"non-matching action",
			"test",
			&kernel.AutodateField{Name: "test", OnCreate: true, OnUpdate: true},
			func() *kernel.Record {
				return kernel.NewRecord(collection)
			},
			"",
		},
		{
			"create with zero value (disabled onCreate)",
			kernel.InterceptorActionCreateExecute,
			&kernel.AutodateField{Name: "test", OnCreate: false, OnUpdate: true},
			func() *kernel.Record {
				return kernel.NewRecord(collection)
			},
			"",
		},
		{
			"create with zero value",
			kernel.InterceptorActionCreateExecute,
			&kernel.AutodateField{Name: "test", OnCreate: true, OnUpdate: true},
			func() *kernel.Record {
				return kernel.NewRecord(collection)
			},
			"{NOW}",
		},
		{
			"create with non-zero value",
			kernel.InterceptorActionCreateExecute,
			&kernel.AutodateField{Name: "test", OnCreate: true, OnUpdate: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", initialDate)
				return record
			},
			initialDate.String(),
		},
		{
			"update with zero value (disabled onUpdate)",
			kernel.InterceptorActionUpdateExecute,
			&kernel.AutodateField{Name: "test", OnCreate: true, OnUpdate: false},
			func() *kernel.Record {
				return kernel.NewRecord(collection)
			},
			"",
		},
		{
			"update with zero value",
			kernel.InterceptorActionUpdateExecute,
			&kernel.AutodateField{Name: "test", OnCreate: true, OnUpdate: true},
			func() *kernel.Record {
				return kernel.NewRecord(collection)
			},
			"{NOW}",
		},
		{
			"update with non-zero value",
			kernel.InterceptorActionUpdateExecute,
			&kernel.AutodateField{Name: "test", OnCreate: true, OnUpdate: true},
			func() *kernel.Record {
				record := kernel.NewRecord(collection)
				record.SetRaw("test", initialDate)
				return record
			},
			initialDate.String(),
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			actionCalls := 0
			record := s.record()

			now := types.NowDateTime().String()
			err := s.field.Intercept(context.Background(), app, record, s.actionName, func() error {
				actionCalls++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			if actionCalls != 1 {
				t.Fatalf("Expected actionCalls %d, got %d", 1, actionCalls)
			}

			expected := cutMilliseconds(strings.ReplaceAll(s.expected, "{NOW}", now))

			v := cutMilliseconds(record.GetString(s.field.GetName()))
			if v != expected {
				t.Fatalf("Expected value %q, got %q", expected, v)
			}
		})
	}
}

func TestAutodateRecordResave(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection, err := app.FindCollectionByNameOrId("demo2")
	if err != nil {
		t.Fatal(err)
	}

	record, err := app.FindRecordById(collection, "llvuca81nly1qls")
	if err != nil {
		t.Fatal(err)
	}

	lastUpdated := record.GetDateTime("updated")

	// save with autogenerated date
	err = app.Save(record)
	if err != nil {
		t.Fatal(err)
	}

	newUpdated := record.GetDateTime("updated")
	if newUpdated.Equal(lastUpdated) {
		t.Fatalf("[0] Expected updated to change, got %v", newUpdated)
	}
	lastUpdated = newUpdated

	// save with custom date
	manualUpdated := lastUpdated.Add(-1 * time.Minute)
	record.SetRaw("updated", manualUpdated)
	err = app.Save(record)
	if err != nil {
		t.Fatal(err)
	}

	newUpdated = record.GetDateTime("updated")
	if !newUpdated.Equal(manualUpdated) {
		t.Fatalf("[1] Expected updated to be the manual set date %v, got %v", manualUpdated, newUpdated)
	}
	lastUpdated = newUpdated

	// save again with autogenerated date
	err = app.Save(record)
	if err != nil {
		t.Fatal(err)
	}

	newUpdated = record.GetDateTime("updated")
	if newUpdated.Equal(lastUpdated) {
		t.Fatalf("[2] Expected updated to change, got %v", newUpdated)
	}
	lastUpdated = newUpdated

	// simulate save failure
	app.OnRecordUpdateExecute(collection.Id).Bind(&hook.Handler[*kernel.RecordEvent]{
		Id: "test_failure",
		Func: func(*kernel.RecordEvent) error {
			return errors.New("test")
		},
		Priority: 9999999999, // as latest as possible
	})

	// save again with autogenerated date (should fail)
	err = app.Save(record)
	if err == nil {
		t.Fatal("Expected save failure")
	}

	// updated should still be set even after save failure
	newUpdated = record.GetDateTime("updated")
	if newUpdated.Equal(lastUpdated) {
		t.Fatalf("[3] Expected updated to change, got %v", newUpdated)
	}
	lastUpdated = newUpdated

	// cleanup the error and resave again
	app.OnRecordUpdateExecute(collection.Id).Unbind("test_failure")

	err = app.Save(record)
	if err != nil {
		t.Fatal(err)
	}

	newUpdated = record.GetDateTime("updated")
	if newUpdated.Equal(lastUpdated) {
		t.Fatalf("[4] Expected updated to change, got %v", newUpdated)
	}
}
