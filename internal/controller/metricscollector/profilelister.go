/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package metricscollector

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/metrics"
)

// profileLister adapts a controller-runtime client into a metrics.ProfileLister,
// letting the ballast.profiles gauge observe the current set of WorkloadProfiles
// without pulling Kubernetes client dependencies into internal/metrics.
type profileLister struct {
	reader client.Reader
}

// NewProfileLister returns a metrics.ProfileLister backed by the given reader
// (typically the manager cache).
func NewProfileLister(c client.Reader) metrics.ProfileLister {
	return &profileLister{reader: c}
}

// ListProfiles lists all WorkloadProfiles and maps each to a metrics.ProfileSnapshot
// carrying its identity-tuple labels and readiness state.
func (l *profileLister) ListProfiles(ctx context.Context) ([]metrics.ProfileSnapshot, error) {
	var list ballastv1.WorkloadProfileList
	if err := l.reader.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing WorkloadProfiles for profiles gauge: %w", err)
	}
	snaps := make([]metrics.ProfileSnapshot, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		snaps = append(snaps, metrics.ProfileSnapshot{
			Labels: p.Status.TupleLabels,
			Ready:  p.Status.MeetsThreshold,
		})
	}
	return snaps, nil
}
