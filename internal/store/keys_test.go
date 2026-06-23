package store_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/tight-line/ballast/internal/store"
)

func newTestClient(t *testing.T) store.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestTupleHash_Deterministic(t *testing.T) {
	labels := map[string]string{"app": "billing", "env": "prod"}
	h1 := store.TupleHash(labels)
	h2 := store.TupleHash(labels)
	if h1 != h2 {
		t.Fatalf("TupleHash not deterministic: %q != %q", h1, h2)
	}
}

func TestTupleHash_OrderIndependent(t *testing.T) {
	h1 := store.TupleHash(map[string]string{"a": "1", "b": "2"})
	h2 := store.TupleHash(map[string]string{"b": "2", "a": "1"})
	if h1 != h2 {
		t.Fatalf("TupleHash is order-dependent: %q != %q", h1, h2)
	}
}

func TestTupleHash_DifferentInputsDiffer(t *testing.T) {
	h1 := store.TupleHash(map[string]string{"a": "1"})
	h2 := store.TupleHash(map[string]string{"a": "2"})
	if h1 == h2 {
		t.Fatal("TupleHash collision for different inputs")
	}
}

func TestTupleHash_Format(t *testing.T) {
	h := store.TupleHash(map[string]string{"k": "v"})
	if len(h) != 16 {
		t.Fatalf("expected 16-char hash, got %d: %q", len(h), h)
	}
	for _, ch := range h {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			t.Fatalf("non-hex char in hash: %q", h)
		}
	}
}

func TestMetricKey(t *testing.T) {
	got := store.MetricKey("abc123", "mycontainer", "cpu")
	want := "ballast:metrics:abc123:mycontainer:cpu"
	if got != want {
		t.Fatalf("MetricKey = %q, want %q", got, want)
	}
}

func TestAllKeysForHash(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	hash := store.TupleHash(map[string]string{"app": "api"})
	k1 := store.MetricKey(hash, "app", "cpu")
	k2 := store.MetricKey(hash, "app", "memory")
	otherHash := store.TupleHash(map[string]string{"app": "other"})
	k3 := store.MetricKey(otherHash, "app", "cpu")

	for _, k := range []string{k1, k2, k3} {
		if err := store.AddSample(ctx, c, k, 1000, "100"); err != nil {
			t.Fatalf("AddSample: %v", err)
		}
	}

	keys, err := store.AllKeysForHash(ctx, c, hash)
	if err != nil {
		t.Fatalf("AllKeysForHash: %v", err)
	}
	sort.Strings(keys)
	want := []string{k1, k2}
	sort.Strings(want)
	if len(keys) != len(want) {
		t.Fatalf("AllKeysForHash = %v, want %v", keys, want)
	}
	for i := range keys {
		if keys[i] != want[i] {
			t.Fatalf("AllKeysForHash[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

func TestAllKeysForHash_Empty(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	keys, err := store.AllKeysForHash(ctx, c, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected empty slice, got %v", keys)
	}
}
