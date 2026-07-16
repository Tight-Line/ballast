package validation_test

import (
	"testing"

	"github.com/tight-line/ballast/internal/validation"
)

func modeLabels(v string) map[string]string {
	return map[string]string{validation.LabelMode: v}
}

func TestValidateMode(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{name: "measure", labels: modeLabels(validation.ModeMeasure)},
		{name: "apply", labels: modeLabels(validation.ModeApply)},
		{name: "resize", labels: modeLabels(validation.ModeResize)},
		{name: "no mode label", labels: map[string]string{"unrelated": "true"}},
		{name: "nil labels", labels: nil},
		{name: "unknown value", labels: modeLabels("frobnicate"), wantErr: true},
		{name: "empty value", labels: modeLabels(""), wantErr: true},
		{name: "legacy true value", labels: modeLabels("true"), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validation.ValidateMode(tc.labels)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestMode(t *testing.T) {
	if got := validation.Mode(modeLabels(validation.ModeApply)); got != validation.ModeApply {
		t.Errorf("Mode = %q, want %q", got, validation.ModeApply)
	}
	if got := validation.Mode(nil); got != "" {
		t.Errorf("Mode(nil) = %q, want empty", got)
	}
}

func TestModePredicates(t *testing.T) {
	tests := []struct {
		mode                              string
		enrolled, wantsApply, wantsResize bool
	}{
		{mode: "", enrolled: false, wantsApply: false, wantsResize: false},
		{mode: "frobnicate", enrolled: false, wantsApply: false, wantsResize: false},
		{mode: validation.ModeMeasure, enrolled: true, wantsApply: false, wantsResize: false},
		{mode: validation.ModeApply, enrolled: true, wantsApply: true, wantsResize: false},
		{mode: validation.ModeResize, enrolled: true, wantsApply: true, wantsResize: true},
	}

	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			labels := modeLabels(tc.mode)
			if got := validation.IsEnrolled(labels); got != tc.enrolled {
				t.Errorf("IsEnrolled(%q) = %v, want %v", tc.mode, got, tc.enrolled)
			}
			if got := validation.WantsApply(labels); got != tc.wantsApply {
				t.Errorf("WantsApply(%q) = %v, want %v", tc.mode, got, tc.wantsApply)
			}
			if got := validation.WantsResize(labels); got != tc.wantsResize {
				t.Errorf("WantsResize(%q) = %v, want %v", tc.mode, got, tc.wantsResize)
			}
		})
	}
}
