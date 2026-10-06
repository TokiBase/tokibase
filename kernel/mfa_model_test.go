package kernel_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/types"
)

func TestNewMFA(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	mfa := kernel.NewMFA(app)

	if mfa.Collection().Name != kernel.CollectionNameMFAs {
		t.Fatalf("Expected record with %q collection, got %q", kernel.CollectionNameMFAs, mfa.Collection().Name)
	}
}

func TestMFAProxyRecord(t *testing.T) {
	t.Parallel()

	record := kernel.NewRecord(kernel.NewBaseCollection("test"))
	record.Id = "test_id"

	mfa := kernel.MFA{}
	mfa.SetProxyRecord(record)

	if mfa.ProxyRecord() == nil || mfa.ProxyRecord().Id != record.Id {
		t.Fatalf("Expected proxy record with id %q, got %v", record.Id, mfa.ProxyRecord())
	}
}

func TestMFARecordRef(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	mfa := kernel.NewMFA(app)

	testValues := []string{"test_1", "test2", ""}
	for i, testValue := range testValues {
		t.Run(fmt.Sprintf("%d_%q", i, testValue), func(t *testing.T) {
			mfa.SetRecordRef(testValue)

			if v := mfa.RecordRef(); v != testValue {
				t.Fatalf("Expected getter %q, got %q", testValue, v)
			}

			if v := mfa.GetString("recordRef"); v != testValue {
				t.Fatalf("Expected field value %q, got %q", testValue, v)
			}
		})
	}
}

func TestMFACollectionRef(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	mfa := kernel.NewMFA(app)

	testValues := []string{"test_1", "test2", ""}
	for i, testValue := range testValues {
		t.Run(fmt.Sprintf("%d_%q", i, testValue), func(t *testing.T) {
			mfa.SetCollectionRef(testValue)

			if v := mfa.CollectionRef(); v != testValue {
				t.Fatalf("Expected getter %q, got %q", testValue, v)
			}

			if v := mfa.GetString("collectionRef"); v != testValue {
				t.Fatalf("Expected field value %q, got %q", testValue, v)
			}
		})
	}
}

func TestMFAMethod(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	mfa := kernel.NewMFA(app)

	testValues := []string{"test_1", "test2", ""}
	for i, testValue := range testValues {
		t.Run(fmt.Sprintf("%d_%q", i, testValue), func(t *testing.T) {
			mfa.SetMethod(testValue)

			if v := mfa.Method(); v != testValue {
				t.Fatalf("Expected getter %q, got %q", testValue, v)
			}

			if v := mfa.GetString("method"); v != testValue {
				t.Fatalf("Expected field value %q, got %q", testValue, v)
			}
		})
	}
}

func TestMFACreated(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	mfa := kernel.NewMFA(app)

	if v := mfa.Created().String(); v != "" {
		t.Fatalf("Expected empty created, got %q", v)
	}

	now := types.NowDateTime()
	mfa.SetRaw("created", now)

	if v := mfa.Created().String(); v != now.String() {
		t.Fatalf("Expected %q created, got %q", now.String(), v)
	}
}

func TestMFAUpdated(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	mfa := kernel.NewMFA(app)

	if v := mfa.Updated().String(); v != "" {
		t.Fatalf("Expected empty updated, got %q", v)
	}

	now := types.NowDateTime()
	mfa.SetRaw("updated", now)

	if v := mfa.Updated().String(); v != now.String() {
		t.Fatalf("Expected %q updated, got %q", now.String(), v)
	}
}

func TestMFAHasExpired(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	now := types.NowDateTime()

	mfa := kernel.NewMFA(app)
	mfa.SetRaw("created", now.Add(-5*time.Minute))

	scenarios := []struct {
		maxElapsed time.Duration
		expected   bool
	}{
		{0 * time.Minute, true},
		{3 * time.Minute, true},
		{5 * time.Minute, true},
		{6 * time.Minute, false},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%s", i, s.maxElapsed.String()), func(t *testing.T) {
			result := mfa.HasExpired(s.maxElapsed)

			if result != s.expected {
				t.Fatalf("Expected %v, got %v", s.expected, result)
			}
		})
	}
}

func TestMFAPreValidate(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	mfasCol, err := app.FindCollectionByNameOrId(kernel.CollectionNameMFAs)
	if err != nil {
		t.Fatal(err)
	}

	user, err := app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no proxy record", func(t *testing.T) {
		mfa := &kernel.MFA{}

		if err := app.Validate(mfa); err == nil {
			t.Fatal("Expected collection validation error")
		}
	})

	t.Run("non-MFA collection", func(t *testing.T) {
		mfa := &kernel.MFA{}
		mfa.SetProxyRecord(kernel.NewRecord(kernel.NewBaseCollection("invalid")))
		mfa.SetRecordRef(user.Id)
		mfa.SetCollectionRef(user.Collection().Id)
		mfa.SetMethod("test123")

		if err := app.Validate(mfa); err == nil {
			t.Fatal("Expected collection validation error")
		}
	})

	t.Run("MFA collection", func(t *testing.T) {
		mfa := &kernel.MFA{}
		mfa.SetProxyRecord(kernel.NewRecord(mfasCol))
		mfa.SetRecordRef(user.Id)
		mfa.SetCollectionRef(user.Collection().Id)
		mfa.SetMethod("test123")

		if err := app.Validate(mfa); err != nil {
			t.Fatalf("Expected nil validation error, got %v", err)
		}
	})
}

func TestMFAValidateHook(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	user, err := app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	demo1, err := app.FindRecordById("demo1", "84nmscqy84lsi1t")
	if err != nil {
		t.Fatal(err)
	}

	scenarios := []struct {
		name         string
		mfa          func() *kernel.MFA
		expectErrors []string
	}{
		{
			"empty",
			func() *kernel.MFA {
				return kernel.NewMFA(app)
			},
			[]string{"collectionRef", "recordRef", "method"},
		},
		{
			"non-auth collection",
			func() *kernel.MFA {
				mfa := kernel.NewMFA(app)
				mfa.SetCollectionRef(demo1.Collection().Id)
				mfa.SetRecordRef(demo1.Id)
				mfa.SetMethod("test123")
				return mfa
			},
			[]string{"collectionRef"},
		},
		{
			"missing record id",
			func() *kernel.MFA {
				mfa := kernel.NewMFA(app)
				mfa.SetCollectionRef(user.Collection().Id)
				mfa.SetRecordRef("missing")
				mfa.SetMethod("test123")
				return mfa
			},
			[]string{"recordRef"},
		},
		{
			"valid ref",
			func() *kernel.MFA {
				mfa := kernel.NewMFA(app)
				mfa.SetCollectionRef(user.Collection().Id)
				mfa.SetRecordRef(user.Id)
				mfa.SetMethod("test123")
				return mfa
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			errs := app.Validate(s.mfa())
			tests.TestValidationErrors(t, errs, s.expectErrors)
		})
	}
}

func TestMFAClearOnPasswordChange(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	user1, err := app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	user2, err := app.FindAuthRecordByEmail("users", "test2@example.com")
	if err != nil {
		t.Fatal(err)
	}

	mfasToCreate := map[*kernel.Record]int{
		user1: 3,
		user2: 2,
	}
	for user, total := range mfasToCreate {
		for range total {
			mfa := kernel.NewMFA(app)
			mfa.SetCollectionRef(user.Collection().Id)
			mfa.SetRecordRef(user.Id)
			mfa.SetMethod(kernel.MFAMethodPassword)
			if err := app.Save(mfa); err != nil {
				t.Fatal(err)
			}
		}
	}

	// update both users
	err = app.Save(user1)
	if err != nil {
		t.Fatal(err)
	}

	user2.SetRandomPassword()
	err = app.Save(user2)
	if err != nil {
		t.Fatal(err)
	}

	expectedMFAs := map[*kernel.Record]int{
		user1: 3,
		user2: 0,
	}

	for user, expected := range expectedMFAs {
		mfas, err := app.FindAllMFAsByRecord(user)
		if err != nil {
			t.Fatal(err)
		}

		if len(mfas) != expected {
			t.Fatalf("Expected %d MFAs, got %d", expected, len(mfas))
		}
	}
}
