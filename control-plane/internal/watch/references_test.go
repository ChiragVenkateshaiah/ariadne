package watch

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func deployment(name, namespace string, podSpec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     map[string]any{"template": map[string]any{"spec": podSpec}},
	}}
}

func TestReferencesConfigSource_VolumeMount(t *testing.T) {
	podSpec := map[string]any{
		"volumes": []any{
			map[string]any{"name": "flags", "configMap": map[string]any{"name": "pricing-flags"}},
		},
	}
	if !referencesConfigSource(podSpec, "ConfigMap", "pricing-flags") {
		t.Fatal("expected volume-mounted ConfigMap to be detected as a reference")
	}
	if referencesConfigSource(podSpec, "ConfigMap", "other-config") {
		t.Fatal("expected a differently-named ConfigMap to NOT match")
	}
}

func TestReferencesConfigSource_SecretVolume(t *testing.T) {
	podSpec := map[string]any{
		"volumes": []any{
			map[string]any{"name": "tls", "secret": map[string]any{"secretName": "tls-cert"}},
		},
	}
	if !referencesConfigSource(podSpec, "Secret", "tls-cert") {
		t.Fatal("expected secret volume to be detected as a reference")
	}
}

func TestReferencesConfigSource_EnvFrom(t *testing.T) {
	podSpec := map[string]any{
		"containers": []any{
			map[string]any{"envFrom": []any{
				map[string]any{"configMapRef": map[string]any{"name": "app-config"}},
			}},
		},
	}
	if !referencesConfigSource(podSpec, "ConfigMap", "app-config") {
		t.Fatal("expected envFrom.configMapRef to be detected as a reference")
	}
}

func TestReferencesConfigSource_EnvValueFrom(t *testing.T) {
	podSpec := map[string]any{
		"containers": []any{
			map[string]any{"env": []any{
				map[string]any{"name": "DB_PASSWORD", "valueFrom": map[string]any{
					"secretKeyRef": map[string]any{"name": "db-creds", "key": "password"},
				}},
			}},
		},
	}
	if !referencesConfigSource(podSpec, "Secret", "db-creds") {
		t.Fatal("expected env[].valueFrom.secretKeyRef to be detected as a reference")
	}
}

func TestReferencesConfigSource_NoMatch(t *testing.T) {
	podSpec := map[string]any{"containers": []any{map[string]any{"image": "nginx"}}}
	if referencesConfigSource(podSpec, "ConfigMap", "anything") {
		t.Fatal("expected a pod spec with no config references to match nothing")
	}
}

func TestFindReferencingWorkloadNames_FiltersByNamespaceAndMatches(t *testing.T) {
	deployments := []*unstructured.Unstructured{
		deployment("pricing-svc", "travel", map[string]any{
			"volumes": []any{map[string]any{"configMap": map[string]any{"name": "pricing-flags"}}},
		}),
		deployment("search-api", "travel", map[string]any{}), // no reference
		deployment("pricing-svc", "other-ns", map[string]any{ // same ref, wrong namespace
			"volumes": []any{map[string]any{"configMap": map[string]any{"name": "pricing-flags"}}},
		}),
	}

	names := findReferencingWorkloadNames(deployments, "travel", "ConfigMap", "pricing-flags")
	if len(names) != 1 || names[0] != "pricing-svc" {
		t.Fatalf("expected exactly [\"pricing-svc\"] in the travel namespace, got %+v", names)
	}
}
