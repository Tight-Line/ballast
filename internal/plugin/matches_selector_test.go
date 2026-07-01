package plugin_test

import (
	"testing"

	"github.com/tight-line/ballast/internal/plugin"
)

func TestMatchesSelector(t *testing.T) {
	tests := []struct {
		name      string
		podLabels map[string]string
		selector  map[string]string
		want      bool
	}{
		{
			name:      "empty selector matches any pod",
			podLabels: map[string]string{"app": "web"},
			selector:  map[string]string{},
			want:      true,
		},
		{
			name:      "exact match on all keys",
			podLabels: map[string]string{"app": "web", "env": "prod"},
			selector:  map[string]string{"app": "web", "env": "prod"},
			want:      true,
		},
		{
			name:      "value mismatch fails",
			podLabels: map[string]string{"app": "web"},
			selector:  map[string]string{"app": "api"},
			want:      false,
		},
		{
			name:      "missing key fails",
			podLabels: map[string]string{"app": "web"},
			selector:  map[string]string{"app": "web", "env": "prod"},
			want:      false,
		},
		{
			name:      "LabelAbsent matches when key is absent",
			podLabels: map[string]string{"app": "web"},
			selector:  map[string]string{"app": "web", "component": plugin.LabelAbsent},
			want:      true,
		},
		{
			name:      "LabelAbsent fails when key is present",
			podLabels: map[string]string{"app": "web", "component": "server"},
			selector:  map[string]string{"app": "web", "component": plugin.LabelAbsent},
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := plugin.MatchesSelector(tc.podLabels, tc.selector); got != tc.want {
				t.Errorf("MatchesSelector(%v, %v) = %v, want %v", tc.podLabels, tc.selector, got, tc.want)
			}
		})
	}
}
