# ballast — Agent Guide

> This file is the primary entry point for AI agents working on ballast.
> Read DESIGN.md for full system context before making changes.
> Read IMPLEMENTATION_PLAN.md to find the current phase and key files.

## Project Overview

## Quick Start

```bash
make check        # Full gate: lint + coverage + build
make test         # Run tests
make build        # Build bin/ballastd
make fmt          # Format code
make lint-fix     # Auto-fix lint issues
make manifests    # Regenerate CRDs and RBAC from markers
make generate     # Regenerate DeepCopy methods
```

## Key Files

## Architecture

## CRD Types

## Controllers

## Admission Webhook

## Plugin Interface

## Redis Data Model

## Testing Strategy

## Build and CI

## Coding Standards

## Important: Never Edit These (Auto-Generated)

- `config/crd/bases/*.yaml` — from `make manifests`
- `config/rbac/role.yaml` — from `make manifests`
- `api/*/zz_generated.*.go` — from `make generate`
- `PROJECT` — kubebuilder metadata

## Important: Never Remove Scaffold Markers

Do NOT delete `// +kubebuilder:scaffold:*` comments. The kubebuilder CLI injects code at these markers.
