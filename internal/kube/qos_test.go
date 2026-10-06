/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestPodQOS(t *testing.T) {
	req := func(cpu, mem string) corev1.ResourceList {
		rl := corev1.ResourceList{}
		if cpu != "" {
			rl[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if mem != "" {
			rl[corev1.ResourceMemory] = resource.MustParse(mem)
		}
		return rl
	}
	cases := []struct {
		name       string
		containers []corev1.Container
		want       corev1.PodQOSClass
	}{
		{
			name:       "no resources anywhere",
			containers: []corev1.Container{{Name: "a"}},
			want:       corev1.PodQOSBestEffort,
		},
		{
			name: "non-qos resources and zero quantities are ignored",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
					corev1.ResourceCPU:              resource.MustParse("0"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
				},
			}}},
			want: corev1.PodQOSBestEffort,
		},
		{
			name: "request only",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: req("100m", ""),
			}}},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "requests equal limits for cpu and memory",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: req("100m", "128Mi"),
				Limits:   req("100m", "128Mi"),
			}}},
			want: corev1.PodQOSGuaranteed,
		},
		{
			name: "cpu limit only is not guaranteed",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: req("100m", ""),
				Limits:   req("100m", ""),
			}}},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "request below limit is not guaranteed",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: req("100m", "128Mi"),
				Limits:   req("200m", "128Mi"),
			}}},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "init container resources count",
			containers: []corev1.Container{
				{Name: "a"},
				{Name: "init", Resources: corev1.ResourceRequirements{Requests: req("10m", "")}},
			},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "two guaranteed containers aggregate",
			containers: []corev1.Container{
				{Name: "a", Resources: corev1.ResourceRequirements{
					Requests: req("100m", "128Mi"),
					Limits:   req("100m", "128Mi"),
				}},
				{Name: "b", Resources: corev1.ResourceRequirements{
					Requests: req("50m", "64Mi"),
					Limits:   req("50m", "64Mi"),
				}},
			},
			want: corev1.PodQOSGuaranteed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PodQOS(tc.containers); got != tc.want {
				t.Errorf("PodQOS = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestStepDown(t *testing.T) {
	cases := []struct {
		name string
		q    string
		res  corev1.ResourceName
		want string
	}{
		{"cpu steps by one millicore", "500m", corev1.ResourceCPU, "499m"},
		{"whole cpu steps by one millicore", "1", corev1.ResourceCPU, "999m"},
		{"memory steps by one byte", "128Mi", corev1.ResourceMemory, "134217727"},
		{"other resources step by one", "1Gi", corev1.ResourceEphemeralStorage, "1073741823"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StepDown(resource.MustParse(tc.q), tc.res)
			if want := resource.MustParse(tc.want); got.Cmp(want) != 0 {
				t.Errorf("StepDown(%s, %s) = %s, want %s", tc.q, tc.res, got.String(), tc.want)
			}
		})
	}
}

func TestIsQOSResource(t *testing.T) {
	for res, want := range map[corev1.ResourceName]bool{
		corev1.ResourceCPU:              true,
		corev1.ResourceMemory:           true,
		corev1.ResourceEphemeralStorage: false,
	} {
		if got := IsQOSResource(res); got != want {
			t.Errorf("IsQOSResource(%s) = %v, want %v", res, got, want)
		}
	}
}
