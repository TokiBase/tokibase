package dbadvise_test

import (
	"slices"
	"testing"

	"github.com/tokibase/tokibase/tools/dbadvise"
)

func TestSortFields(t *testing.T) {
	t.Parallel()

	got := dbadvise.SortFields("-created,+title, @random ,author.name,id")
	if want := []string{"created", "title", "id"}; !slices.Equal(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestFilterFields(t *testing.T) {
	t.Parallel()

	got := dbadvise.FilterFields(`user = "abc" && (status != 'a = b' || n >= 3) && author.name ~ 'x' && @request.auth.id != "" && tags ?= "t"`)
	if want := []string{"user", "status", "n", "tags"}; !slices.Equal(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestLeadingColumns(t *testing.T) {
	t.Parallel()

	got := dbadvise.LeadingColumns([]string{
		"CREATE INDEX `idx_a` ON `t` (`user`, `created`)",
		"CREATE UNIQUE INDEX `idx_b` ON `t` (`Slug`)",
	})
	for _, name := range []string{"id", "user", "slug"} {
		if !got[name] {
			t.Fatalf("expected %q to be indexed: %v", name, got)
		}
	}
	if got["created"] {
		t.Fatalf("created is not a leading column: %v", got)
	}
}
