#!/bin/bash
#
# Coverage check script with branch-level exclusions.
#
# Usage:
#   ./scripts/check-coverage.sh              # Local check (fails if uncovered without ignore)
#   ./scripts/check-coverage.sh --codecov    # Generate filtered coverage.out for Codecov
#
# Mark untestable code with: // coverage:ignore - <reason>
#
# The comment must be on the same line as the uncovered code, or the line before.
#
# KUBEBUILDER_ASSETS env var is picked up automatically by envtest-based tests
# when set by the Makefile test-coverage-check target.

set -e

MODULE_PATH="github.com/tight-line/ballast"
COVERAGE_FILE="coverage.out"

echo "Running tests with coverage..."
# Exclude cmd/ (binary entrypoints; tested via envtest/e2e, not unit tests),
# test/ (e2e infrastructure), and e2e packages.
# shellcheck disable=SC2046
PKGS=$(go list ./... | grep -v '/e2e' | grep -v '/test/' | grep -v '/cmd/' || true)

if [[ -z "$PKGS" ]]; then
    echo "Coverage check passed: no testable packages found"
    exit 0
fi

go test -race -coverprofile="$COVERAGE_FILE" -covermode=atomic -tags=ci $PKGS
echo ""

# Get uncovered lines (count=0)
UNCOVERED=$(grep " 0$" "$COVERAGE_FILE" || true)

if [[ -z "$UNCOVERED" ]]; then
    TOTAL=$(go tool cover -func="$COVERAGE_FILE" | grep "^total:" | awk '{print $3}')
    echo "Coverage check passed: $TOTAL"
    exit 0
fi

# Check each uncovered line for ignore comment
ERRORS=""
while IFS= read -r line; do
    [[ -z "$line" ]] && continue

    # Parse: github.com/.../file.go:startLine.col,endLine.col statements 0
    PKG_FILE=$(echo "$line" | cut -d: -f1)
    START_LINE=$(echo "$line" | cut -d: -f2 | cut -d. -f1)

    # Convert package path to file path
    REL_PATH=$(echo "$PKG_FILE" | sed "s|^$MODULE_PATH/||")

    [[ ! -f "$REL_PATH" ]] && continue

    # Check if line or previous line has coverage:ignore
    PREV_LINE=$((START_LINE - 1))
    CONTEXT=$(sed -n "${PREV_LINE},${START_LINE}p" "$REL_PATH" 2>/dev/null || true)

    if ! echo "$CONTEXT" | grep -q "coverage:ignore"; then
        ERRORS="${ERRORS}${REL_PATH}:${START_LINE}\n"
    fi
done <<< "$UNCOVERED"

if [[ -n "$ERRORS" ]]; then
    echo "ERROR: Uncovered code without coverage:ignore comments:" >&2
    echo "" >&2
    echo -e "$ERRORS" | sort -u | grep -v "^$" >&2
    echo "" >&2
    echo "Either add tests or mark with: // coverage:ignore - <reason>" >&2
    exit 1
fi

# For --codecov mode, create filtered coverage where ignored lines show as covered
if [[ "$1" == "--codecov" ]]; then
    head -1 "$COVERAGE_FILE" > coverage.filtered.out
    tail -n +2 "$COVERAGE_FILE" | while IFS= read -r line; do
        COUNT=$(echo "$line" | awk '{print $NF}')
        if [[ "$COUNT" == "0" ]]; then
            PKG_FILE=$(echo "$line" | cut -d: -f1)
            START_LINE=$(echo "$line" | cut -d: -f2 | cut -d. -f1)
            REL_PATH=$(echo "$PKG_FILE" | sed "s|^$MODULE_PATH/||")
            if [[ -f "$REL_PATH" ]]; then
                PREV_LINE=$((START_LINE - 1))
                CONTEXT=$(sed -n "${PREV_LINE},${START_LINE}p" "$REL_PATH" 2>/dev/null || true)
                if echo "$CONTEXT" | grep -q "coverage:ignore"; then
                    echo "$line" | sed 's/ 0$/ 1/'
                    continue
                fi
            fi
        fi
        echo "$line"
    done >> coverage.filtered.out
    echo "Filtered coverage written to coverage.filtered.out"
fi

TOTAL=$(go tool cover -func="$COVERAGE_FILE" | grep "^total:" | awk '{print $3}')
echo "Coverage check passed: $TOTAL"
