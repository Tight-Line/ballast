package policy

import corev1 "k8s.io/api/core/v1"

// InputForPod builds the resolver Input describing one pod.
//
// Both the admission webhook and the workloadwatcher resolve policy, and they
// must reach the same answer for the same pod: if they diverge, a pod is admitted
// with one policy's recommendations and then measured and resized under another,
// with nothing logging a conflict because each path resolved successfully on its
// own terms. Sharing this constructor is what keeps them from drifting apart.
func InputForPod(pod *corev1.Pod) Input {
	return Input{
		Namespace:   pod.Namespace,
		OwnerKind:   directOwnerKind(pod),
		Labels:      pod.Labels,
		Annotations: pod.Annotations,
	}
}

// directOwnerKind returns the kind of the pod's controlling ownerReference, or ""
// when the pod has no controller (a standalone pod), in which case only policies
// with no Kinds selector match.
func directOwnerKind(pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind
		}
	}
	return ""
}
