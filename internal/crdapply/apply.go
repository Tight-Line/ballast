/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

// Package crdapply server-side-applies the operator's embedded CRD manifests.
// It backs the `ballastd apply-crds` subcommand, which the Helm chart runs as
// a pre-install/pre-upgrade hook so CRD changes ship with every release
// (Helm's crds/ directory is install-only and never upgraded).
package crdapply

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// FieldManager is the server-side-apply field manager that owns the CRD specs.
// Every apply-crds run uses the same manager, so repeated or concurrent runs
// never conflict with each other; ForceOwnership reclaims fields from any
// other manager (a manual `kubectl apply`, the original Helm install).
const FieldManager = "ballast-crd-installer"

// Apply server-side-applies every *.yaml manifest in fsys, forcing ownership
// of the fields each manifest declares. Each apply is one atomic API request
// and the operation is idempotent, so no locking is needed: the safety comes
// from SSA semantics, not from assumptions about who invokes it or how often.
// Non-CRD manifests are rejected so a stray file cannot grant this command a
// side door to apply arbitrary objects.
func Apply(ctx context.Context, c client.Client, fsys fs.FS, log logr.Logger) error {
	names, err := fs.Glob(fsys, "*.yaml")
	if err != nil { // coverage:ignore - the glob pattern is constant and valid
		return fmt.Errorf("globbing CRD manifests: %w", err)
	}
	if len(names) == 0 {
		return fmt.Errorf("no CRD manifests found")
	}
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil { // coverage:ignore - reads from an embedded FS cannot fail
			return fmt.Errorf("reading %s: %w", name, err)
		}
		// Unstructured (not the typed CRD struct) so the apply declares exactly
		// the fields present in the manifest: a typed round-trip would add
		// zero-valued fields (creationTimestamp, status) to the patch and this
		// manager would take ownership of fields the manifest never mentions.
		var obj unstructured.Unstructured
		if err := yaml.Unmarshal(data, &obj.Object); err != nil {
			return fmt.Errorf("parsing %s: %w", name, err)
		}
		if obj.GetKind() != "CustomResourceDefinition" {
			return fmt.Errorf("%s: expected a CustomResourceDefinition, got %q", name, obj.GetKind())
		}
		applyCfg := client.ApplyConfigurationFromUnstructured(&obj)
		if err := c.Apply(ctx, applyCfg, client.ForceOwnership, client.FieldOwner(FieldManager)); err != nil {
			return fmt.Errorf("applying %s: %w", obj.GetName(), err)
		}
		log.Info("applied CRD", "name", obj.GetName())
	}
	return nil
}
