package kernel_test

import (
	"testing"

	"github.com/tokibase/tokibase/kernel"
)

func TestBaseRecordProxy(t *testing.T) {
	p := kernel.BaseRecordProxy{}

	record := kernel.NewRecord(kernel.NewBaseCollection("test"))
	record.Id = "test"

	p.SetProxyRecord(record)

	if p.ProxyRecord() == nil || p.ProxyRecord().Id != p.Id || p.Id != "test" {
		t.Fatalf("Expected proxy record to be set")
	}
}
