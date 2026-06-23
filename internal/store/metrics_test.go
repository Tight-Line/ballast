package store_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/tight-line/ballast/internal/store"
)

func TestAddSampleAndQueryWindow(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	for _, s := range []struct {
		ts  int64
		val string
	}{
		{1000, "100"},
		{2000, "200"},
		{3000, "300"},
		{4000, "400"},
	} {
		if err := store.AddSample(ctx, c, key, s.ts, s.val); err != nil {
			t.Fatalf("AddSample: %v", err)
		}
	}

	got, err := store.QueryWindow(ctx, c, key, 2000, 3000)
	if err != nil {
		t.Fatalf("QueryWindow: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d: %v", len(got), got)
	}
	if got[0].TimestampMs != 2000 || got[0].Value != "200" {
		t.Errorf("got[0] = %+v, want {2000 200}", got[0])
	}
	if got[1].TimestampMs != 3000 || got[1].Value != "300" {
		t.Errorf("got[1] = %+v, want {3000 300}", got[1])
	}
}

func TestQueryWindow_Empty(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	got, err := store.QueryWindow(ctx, c, "ballast:metrics:test:app:cpu", 0, 9999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestExpireOlderThan(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	for _, ts := range []int64{1000, 2000, 3000, 4000} {
		_ = store.AddSample(ctx, c, key, ts, strconv.FormatInt(ts, 10))
	}

	if err := store.ExpireOlderThan(ctx, c, key, 3000); err != nil {
		t.Fatalf("ExpireOlderThan: %v", err)
	}

	remaining, _ := store.QueryWindow(ctx, c, key, 0, 9999)
	if len(remaining) != 2 {
		t.Fatalf("expected 2 remaining, got %d: %v", len(remaining), remaining)
	}
	for _, sv := range remaining {
		if sv.TimestampMs < 3000 {
			t.Errorf("sample with ts %d survived cutoff 3000", sv.TimestampMs)
		}
	}
}

func TestSampleCount(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	n, err := store.SampleCount(ctx, c, key)
	if err != nil || n != 0 {
		t.Fatalf("empty key: count=%d err=%v", n, err)
	}

	_ = store.AddSample(ctx, c, key, 1, "a")
	_ = store.AddSample(ctx, c, key, 2, "b")
	_ = store.AddSample(ctx, c, key, 3, "c")

	n, err = store.SampleCount(ctx, c, key)
	if err != nil || n != 3 {
		t.Fatalf("expected 3, got count=%d err=%v", n, err)
	}
}

func TestTimeRange(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	first, last, err := store.TimeRange(ctx, c, key)
	if err != nil || first != 0 || last != 0 {
		t.Fatalf("empty key: first=%d last=%d err=%v", first, last, err)
	}

	_ = store.AddSample(ctx, c, key, 1000, "a")
	_ = store.AddSample(ctx, c, key, 5000, "b")
	_ = store.AddSample(ctx, c, key, 3000, "c")

	first, last, err = store.TimeRange(ctx, c, key)
	if err != nil {
		t.Fatalf("TimeRange: %v", err)
	}
	if first != 1000 {
		t.Errorf("first = %d, want 1000", first)
	}
	if last != 5000 {
		t.Errorf("last = %d, want 5000", last)
	}
}

func TestDeleteKey(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	_ = store.AddSample(ctx, c, key, 1000, "v")
	if err := store.DeleteKey(ctx, c, key); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}

	n, _ := store.SampleCount(ctx, c, key)
	if n != 0 {
		t.Errorf("key still has %d entries after delete", n)
	}
}

func TestEnforceReservoirCap(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	for i := int64(1); i <= 7; i++ {
		_ = store.AddSample(ctx, c, key, i*1000, strconv.FormatInt(i, 10))
	}

	if err := store.EnforceReservoirCap(ctx, c, key, 5); err != nil {
		t.Fatalf("EnforceReservoirCap: %v", err)
	}

	n, _ := store.SampleCount(ctx, c, key)
	if n != 5 {
		t.Errorf("after cap=5, count = %d, want 5", n)
	}

	remaining, _ := store.QueryWindow(ctx, c, key, 0, 99999)
	if remaining[0].TimestampMs != 3000 {
		t.Errorf("oldest remaining ts = %d, want 3000", remaining[0].TimestampMs)
	}
}

func TestEnforceReservoirCap_NopWhenUnderCap(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	for i := int64(1); i <= 3; i++ {
		_ = store.AddSample(ctx, c, key, i*1000, strconv.FormatInt(i, 10))
	}

	if err := store.EnforceReservoirCap(ctx, c, key, 5); err != nil {
		t.Fatalf("EnforceReservoirCap: %v", err)
	}

	n, _ := store.SampleCount(ctx, c, key)
	if n != 3 {
		t.Errorf("count changed to %d, expected 3", n)
	}
}
