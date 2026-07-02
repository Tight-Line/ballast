/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

// Package crds embeds the generated CRD manifests so the operator binary can
// apply them itself (`ballastd apply-crds`). Helm installs the chart's crds/
// directory only on first install and never upgrades it; a pre-install and
// pre-upgrade hook runs this subcommand to keep the cluster's CRDs in sync on
// every release operation.
package crds

import "embed"

// FS holds every generated CRD manifest in this directory. controller-gen
// regenerates the *.yaml files in place (make manifests), so the embedded
// copies are always current at build time.
//
//go:embed *.yaml
var FS embed.FS
