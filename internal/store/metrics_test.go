package store_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/tight-line/ballast/internal/store"
)

func TestAddSampleAndQueryAll(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	for _, v := range []string{"100", "200", "300"} {
		if err := store.AddSample(ctx, c, key, 1000, v, 0); err != nil {
			t.Fatalf("AddSample: %v", err)
		}
	}

	got, err := store.QueryAll(ctx, c, key)
	if err != nil {
		t.Fatalf("QueryAll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(got), got)
	}
	if got[0] != "100" || got[1] != "200" || got[2] != "300" {
		t.Errorf("unexpected values: %v", got)
	}
}

func TestQueryAll_Empty(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	got, err := store.QueryAll(ctx, c, "ballast:metrics:test:app:cpu")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestAddSample_SameValueNotDeduplicated(t *testing.T) {
	// Regression: sorted sets silently deduplicated identical values.
	// A list must store one entry per AddSample call.
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	for i := 0; i < 5; i++ {
		if err := store.AddSample(ctx, c, key, int64(i*1000), "1", 0); err != nil {
			t.Fatalf("AddSample: %v", err)
		}
	}

	got, err := store.QueryAll(ctx, c, key)
	if err != nil {
		t.Fatalf("QueryAll: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("expected 5 entries (one per call), got %d", len(got))
	}
}

func TestAddSample_ReservoirCap(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	for i := int64(1); i <= 7; i++ {
		if err := store.AddSample(ctx, c, key, i*1000, strconv.FormatInt(i, 10), 5); err != nil {
			t.Fatalf("AddSample: %v", err)
		}
	}

	n, err := store.SampleCount(ctx, c, key)
	if err != nil || n != 5 {
		t.Errorf("expected cap=5, got count=%d err=%v", n, err)
	}

	got, _ := store.QueryAll(ctx, c, key)
	if got[0] != "3" {
		t.Errorf("oldest remaining = %q, want \"3\"", got[0])
	}
}

func TestAddSample_CapZeroNoTrim(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	for i := int64(1); i <= 10; i++ {
		if err := store.AddSample(ctx, c, key, i*1000, strconv.FormatInt(i, 10), 0); err != nil {
			t.Fatalf("AddSample: %v", err)
		}
	}

	n, _ := store.SampleCount(ctx, c, key)
	if n != 10 {
		t.Errorf("expected 10 entries with cap=0, got %d", n)
	}
}

func TestFirstSeenMs(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	ms, err := store.FirstSeenMs(ctx, c, key)
	if err != nil || ms != 0 {
		t.Fatalf("empty key: ms=%d err=%v", ms, err)
	}

	_ = store.AddSample(ctx, c, key, 1000, "v", 0)
	_ = store.AddSample(ctx, c, key, 5000, "v", 0)
	_ = store.AddSample(ctx, c, key, 9000, "v", 0)

	ms, err = store.FirstSeenMs(ctx, c, key)
	if err != nil {
		t.Fatalf("FirstSeenMs: %v", err)
	}
	if ms != 1000 {
		t.Errorf("first_seen = %d, want 1000", ms)
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

	_ = store.AddSample(ctx, c, key, 1, "a", 0)
	_ = store.AddSample(ctx, c, key, 2, "b", 0)
	_ = store.AddSample(ctx, c, key, 3, "c", 0)

	n, err = store.SampleCount(ctx, c, key)
	if err != nil || n != 3 {
		t.Fatalf("expected 3, got count=%d err=%v", n, err)
	}
}

func TestDeleteKey(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	key := "ballast:metrics:test:app:cpu"

	_ = store.AddSample(ctx, c, key, 1000, "v", 0)
	if err := store.DeleteKey(ctx, c, key); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}

	n, _ := store.SampleCount(ctx, c, key)
	if n != 0 {
		t.Errorf("key still has %d entries after delete", n)
	}
}
