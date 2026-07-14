// Package kube holds small, dependency-free helpers for classifying Kubernetes
// pod-spec objects that several Ballast subsystems share.
package kube

import corev1 "k8s.io/api/core/v1"

// IsRestartableInit reports whether c is a restartable-init ("native sidecar")
// container: an init container with restartPolicy: Always (KEP-753). Unlike
// run-to-completion init containers, these run for the whole pod lifetime and
// are legitimate right-sizing targets, exactly like regular containers — so the
// measure, apply, and resize lanes treat them as first-class. The caller is
// responsible for only passing entries from pod.Spec.InitContainers; the field
// is meaningless (and always nil) on regular containers.
func IsRestartableInit(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}
