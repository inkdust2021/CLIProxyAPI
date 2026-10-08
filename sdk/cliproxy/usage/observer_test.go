package usage

import (
	"context"
	"testing"
)

func TestUsageSynchronousObserver(t *testing.T) {
	called := false
	ctx := WithRecordObserver(context.Background(), func(_ context.Context, record Record) { called = record.AuthID == "fixture" })
	ObserveRecord(ctx, Record{AuthID: "fixture"})
	if !called {
		t.Fatal("record observer did not run synchronously")
	}
}
