package validation_test

import (
	"testing"

	"github.com/tight-line/ballast/internal/validation"
)

func ann(pairs ...string) map[string]string {
	m := make(map[string]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m
}

func TestValidateAnnotations(t *testing.T) {
	tests := []struct {
		name    string
		ann     map[string]string
		wantErr bool
	}{
		// Valid combinations
		{
			name: "measure only",
			ann:  ann(validation.AnnotationMeasure, "true"),
		},
		{
			name: "measure + apply",
			ann:  ann(validation.AnnotationMeasure, "true", validation.AnnotationApply, "true"),
		},
		{
			name: "measure + apply + resize",
			ann:  ann(validation.AnnotationMeasure, "true", validation.AnnotationApply, "true", validation.AnnotationResize, "true"),
		},
		{
			name: "autoresize only",
			ann:  ann(validation.AnnotationAutoresize, "true"),
		},
		{
			name:    "no ballast annotations",
			ann:     map[string]string{"unrelated": "true"},
			wantErr: false,
		},
		{
			name:    "empty annotations",
			ann:     map[string]string{},
			wantErr: false,
		},
		// Invalid combinations
		{
			name:    "apply without measure",
			ann:     ann(validation.AnnotationApply, "true"),
			wantErr: true,
		},
		{
			name:    "resize without apply",
			ann:     ann(validation.AnnotationMeasure, "true", validation.AnnotationResize, "true"),
			wantErr: true,
		},
		{
			name:    "autoresize + apply",
			ann:     ann(validation.AnnotationAutoresize, "true", validation.AnnotationApply, "true"),
			wantErr: true,
		},
		{
			name:    "autoresize + resize",
			ann:     ann(validation.AnnotationAutoresize, "true", validation.AnnotationResize, "true"),
			wantErr: true,
		},
		{
			name:    "resize without measure (missing apply too)",
			ann:     ann(validation.AnnotationResize, "true"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validation.ValidateAnnotations(tc.ann)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
