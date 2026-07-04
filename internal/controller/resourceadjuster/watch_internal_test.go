/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package resourceadjuster

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	ballastv1 "github.com/tight-line/ballast/api/v1"
)

// These tests live in the internal package so they can exercise the unexported
// resizeRelevantChange predicate directly, covering every closure branch.

func profile(generation int64, meetsThreshold bool) *ballastv1.WorkloadProfile {
	return &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Generation: generation},
		Status:     ballastv1.WorkloadProfileStatus{MeetsThreshold: meetsThreshold},
	}
}

func TestResizeRelevantChangePredicate(t *testing.T) {
	p := resizeRelevantChange()

	// A pure status rewrite (same generation, same readiness) is the per-poll
	// recommendation churn the collector emits; it must be filtered.
	if p.Update(event.UpdateEvent{ObjectOld: profile(3, true), ObjectNew: profile(3, true)}) {
		t.Error("status-only churn should be filtered")
	}

	// A spec edit bumps generation and must pass.
	if !p.Update(event.UpdateEvent{ObjectOld: profile(3, true), ObjectNew: profile(4, true)}) {
		t.Error("generation change should pass")
	}

	// Crossing the readiness boundary must pass so the first resize is not
	// delayed a full interval, in either direction.
	if !p.Update(event.UpdateEvent{ObjectOld: profile(3, false), ObjectNew: profile(3, true)}) {
		t.Error("readiness false->true should pass")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: profile(3, true), ObjectNew: profile(3, false)}) {
		t.Error("readiness true->false should pass")
	}

	// A non-WorkloadProfile update (which the manager never delivers) is
	// admitted rather than dropped.
	if !p.Update(event.UpdateEvent{ObjectOld: &corev1.Pod{}, ObjectNew: &corev1.Pod{}}) {
		t.Error("unexpected object type should be admitted")
	}

	// Create, delete, and generic events pass by default so new profiles are
	// evaluated and deletions reconcile to a NotFound no-op.
	if !p.Create(event.CreateEvent{Object: profile(1, false)}) {
		t.Error("create should pass")
	}
	if !p.Delete(event.DeleteEvent{Object: profile(1, true)}) {
		t.Error("delete should pass")
	}
	if !p.Generic(event.GenericEvent{Object: profile(1, true)}) {
		t.Error("generic should pass")
	}
}
