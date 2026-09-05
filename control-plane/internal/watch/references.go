package watch

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// findReferencingWorkloadNames performs a best-effort scan of a namespace's
// cached Deployment objects for ones whose pod template mounts or references
// the given ConfigMap/Secret by name. This is what lets a ConfigMap edit
// arrive with hints.touched_workload_names already populated -- e.g. Act 2 of
// the demo (the pricing-flags ConfigMap) resolves directly to ["pricing-svc"]
// without the brain needing a separate lookup.
//
// Deliberately shallow: it checks volumes, envFrom, and env[].valueFrom, which
// covers how our own SUT and the overwhelming majority of real workloads
// reference config -- it does not chase indirection through, say, an
// init-container-generated file.
func findReferencingWorkloadNames(deployments []*unstructured.Unstructured, namespace, refKind, refName string) []string {
	var names []string
	for _, d := range deployments {
		if d.GetNamespace() != namespace {
			continue
		}
		podSpec, found, _ := unstructured.NestedMap(d.Object, "spec", "template", "spec")
		if !found {
			continue
		}
		if referencesConfigSource(podSpec, refKind, refName) {
			names = append(names, d.GetName())
		}
	}
	return names
}

func referencesConfigSource(podSpec map[string]any, refKind, refName string) bool {
	volKey := "configMap"
	nameField := "name"
	if refKind == "Secret" {
		volKey = "secret"
		nameField = "secretName"
	}

	if volumes, found, _ := unstructured.NestedSlice(podSpec, "volumes"); found {
		for _, v := range volumes {
			vol, ok := v.(map[string]any)
			if !ok {
				continue
			}
			src, found, _ := unstructured.NestedMap(vol, volKey)
			if !found {
				continue
			}
			if n, _, _ := unstructured.NestedString(src, nameField); n == refName {
				return true
			}
		}
	}

	containers, found, _ := unstructured.NestedSlice(podSpec, "containers")
	if !found {
		return false
	}
	envFromKey := "configMapRef"
	envValueKey := "configMapKeyRef"
	if refKind == "Secret" {
		envFromKey = "secretKeyRef"
		envValueKey = "secretKeyRef"
	}
	for _, c := range containers {
		container, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if envFrom, found, _ := unstructured.NestedSlice(container, "envFrom"); found {
			for _, e := range envFrom {
				entry, ok := e.(map[string]any)
				if !ok {
					continue
				}
				ref, found, _ := unstructured.NestedMap(entry, envFromKey)
				if found {
					if n, _, _ := unstructured.NestedString(ref, "name"); n == refName {
						return true
					}
				}
			}
		}
		if env, found, _ := unstructured.NestedSlice(container, "env"); found {
			for _, e := range env {
				entry, ok := e.(map[string]any)
				if !ok {
					continue
				}
				valueFrom, found, _ := unstructured.NestedMap(entry, "valueFrom")
				if !found {
					continue
				}
				ref, found, _ := unstructured.NestedMap(valueFrom, envValueKey)
				if found {
					if n, _, _ := unstructured.NestedString(ref, "name"); n == refName {
						return true
					}
				}
			}
		}
	}
	return false
}
