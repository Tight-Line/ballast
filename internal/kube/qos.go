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
// Recommended is the request that would otherwise have been written, Limit the
// limit it exceeded, and Applied the request actually written: the limit, one
// unit under it when that preserves the pod's QoS class, or a capped step toward
// it. All are in Kubernetes quantity notation.
type RequestClamp struct {
	Container   string
	Resource    string
	Recommended string
	Limit       string
	Applied     string
}

// IsQOSResource reports whether res participates in pod QoS classification.
// Only cpu and memory do; every other resource is ignored by the class.
func IsQOSResource(res corev1.ResourceName) bool {
	return res == corev1.ResourceCPU || res == corev1.ResourceMemory
}

// PodQOS computes the QoS class Kubernetes assigns to a pod with the given
// pod-level resources (pod.Spec.Resources, nil when unset) and containers (pass
// regular and init containers together), following the upstream ComputePodQOS
// algorithm over cpu and memory, the only QoS-relevant resources. When
// pod-level resources are set they alone decide the class, and the containers
// are ignored. BestEffort when nothing sets any cpu/memory request or limit,
// Guaranteed when every container (or the pod level) sets both cpu and memory
// limits and aggregate requests equal aggregate limits, Burstable otherwise.
func PodQOS(podResources *corev1.ResourceRequirements, containers []corev1.Container) corev1.PodQOSClass {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	isGuaranteed := true
	if podResources != nil {
		isGuaranteed = addQOSResources(requests, limits, *podResources)
	} else {
		for _, c := range containers {
			if !addQOSResources(requests, limits, c.Resources) {
				isGuaranteed = false
			}
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

// addQOSResources adds rr's nonzero cpu/memory requests and limits to the
// running totals and reports whether rr sets both a cpu and a memory limit.
func addQOSResources(requests, limits corev1.ResourceList, rr corev1.ResourceRequirements) (bothLimits bool) {
	for name, q := range rr.Requests {
		if !IsQOSResource(name) || q.IsZero() {
			continue
		}
		addQuantity(requests, name, q)
	}
	qosLimits := 0
	for name, q := range rr.Limits {
		if !IsQOSResource(name) || q.IsZero() {
			continue
		}
		qosLimits++
		addQuantity(limits, name, q)
	}
	return qosLimits == 2
}

// addQuantity adds q to the running total for name in list.
func addQuantity(list corev1.ResourceList, name corev1.ResourceName, q resource.Quantity) {
	total := q.DeepCopy()
	if cur, ok := list[name]; ok {
		total.Add(cur)
	}
	list[name] = total
}
