package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestIsRestartableInit(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	onFailure := corev1.ContainerRestartPolicy("OnFailure")
	tests := []struct {
		name string
		c    corev1.Container
		want bool
	}{
		{"restartable-init sidecar (Always)", corev1.Container{Name: "otc", RestartPolicy: &always}, true},
		{"run-once init (nil policy)", corev1.Container{Name: "init-db"}, false},
		{"init with non-Always policy", corev1.Container{Name: "x", RestartPolicy: &onFailure}, false},
		{"regular container (nil policy)", corev1.Container{Name: "app"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRestartableInit(tt.c); got != tt.want {
				t.Errorf("IsRestartableInit(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
