/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package kube

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// RequestClamp records one request Ballast held at a container's limit because
// the request it would otherwise have written exceeded that limit (#119).
// Recommended is the pre-clamp request and Limit the limit it was clamped to,
// both in Kubernetes quantity notation.
type RequestClamp struct {
	Container   string
	Resource    string
	Recommended string
	Limit       string
}

// IsQOSResource reports whether res participates in pod QoS classification.
// Only cpu and memory do; every other resource is ignored by the class.
func IsQOSResource(res corev1.ResourceName) bool {
	return res == corev1.ResourceCPU || res == corev1.ResourceMemory
}

// PodQOS computes the QoS class Kubernetes assigns to a pod built from the
// given containers (pass regular and init containers together), following the
// upstream GetPodQOS algorithm over cpu and memory, the only QoS-relevant
// resources: BestEffort when no container sets any cpu/memory request or
// limit, Guaranteed when every container sets both cpu and memory limits and
// aggregate requests equal aggregate limits, Burstable otherwise.
func PodQOS(containers []corev1.Container) corev1.PodQOSClass {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	isGuaranteed := true
	for _, c := range containers {
		for name, q := range c.Resources.Requests {
			if !IsQOSResource(name) || q.IsZero() {
				continue
			}
			addQuantity(requests, name, q)
		}
		qosLimits := 0
		for name, q := range c.Resources.Limits {
			if !IsQOSResource(name) || q.IsZero() {
				continue
			}
			qosLimits++
			addQuantity(limits, name, q)
		}
		if qosLimits != 2 { // both cpu and memory
			isGuaranteed = false
		}
	}
	if len(requests) == 0 && len(limits) == 0 {
		return corev1.PodQOSBestEffort
	}
	if isGuaranteed {
		for name, req := range requests {
			if lim, ok := limits[name]; !ok || lim.Cmp(req) != 0 {
				isGuaranteed = false
				break
			}
		}
	}
	if isGuaranteed && len(requests) == len(limits) {
		return corev1.PodQOSGuaranteed
	}
	return corev1.PodQOSBurstable
}

// StepDown returns q reduced by the smallest unit Kubernetes distinguishes for
// res: 1m for cpu, and 1 (one byte of memory or storage) for everything else.
// Clamping a request to exactly its limit can make the last unequal
// request/limit pair equal and promote a Burstable pod to Guaranteed; holding
// the request one unit below the limit instead keeps the pod's class.
func StepDown(q resource.Quantity, res corev1.ResourceName) resource.Quantity {
	if res == corev1.ResourceCPU {
		return *resource.NewMilliQuantity(q.MilliValue()-1, resource.DecimalSI)
	}
	return *resource.NewQuantity(q.Value()-1, q.Format)
}

// addQuantity adds q to the running total for name in list.
func addQuantity(list corev1.ResourceList, name corev1.ResourceName, q resource.Quantity) {
	total := q.DeepCopy()
	if cur, ok := list[name]; ok {
		total.Add(cur)
	}
	list[name] = total
}
