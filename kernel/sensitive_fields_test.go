package kernel_test

import (
	"testing"

	"github.com/tokibase/tokibase/kernel"
)

func TestSensitiveRegistry(t *testing.T) {
	kernel.RegisterSensitiveField("c1", "b")
	kernel.RegisterSensitiveField("c1", "a")
	if !kernel.IsSensitive("c1", "a") || kernel.IsSensitive("c1", "z") || kernel.IsSensitive("c2", "a") {
		t.Fatal("IsSensitive")
	}
	if got := kernel.SensitiveFieldsOf("c1"); len(got) != 2 || got[0] != "a" {
		t.Fatalf("%v", got)
	}
	kernel.UnregisterSensitiveField("c1", "a")
	kernel.UnregisterSensitiveField("c1", "b")
	if kernel.IsSensitive("c1", "a") || kernel.SensitiveFieldsOf("c1") != nil {
		t.Fatal("unregister")
	}
}

func TestRedactExport(t *testing.T) {
	col := kernel.NewBaseCollection("rx")
	col.Id = "rx_id_1"
	col.Fields.Add(&kernel.TextField{Name: "secret"}, &kernel.TextField{Name: "plain"})
	rec := kernel.NewRecord(col)
	rec.Set("secret", "hunter2")
	rec.Set("plain", "ok")
	kernel.RegisterSensitiveField(col.Id, "secret")
	defer kernel.UnregisterSensitiveField(col.Id, "secret")

	ex := kernel.RedactExport(rec, rec.PublicExport(), "")
	if ex["secret"] != kernel.SensitiveMarker || ex["plain"] != "ok" {
		t.Fatalf("%v", ex)
	}
	rec.Set("secret", "")
	if ex := kernel.RedactExport(rec, rec.PublicExport(), ""); ex["secret"] != "" {
		t.Fatalf("empty stays empty: %v", ex)
	}
}
