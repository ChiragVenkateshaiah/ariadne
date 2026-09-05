package watch

import (
	"testing"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRefineDeploymentClass_ScaleOnly(t *testing.T) {
	diffs := []*ariadnev1.FieldDiff{{Path: "spec.replicas", Before: "1", After: "3"}}
	class, scaleOnly := refineDeploymentClass(diffs)
	if class != "CHANGE_CLASS_SCALING" || !scaleOnly {
		t.Fatalf("expected pure replica change to classify as SCALING, got class=%s scaleOnly=%v", class, scaleOnly)
	}
}

func TestRefineDeploymentClass_ImageOnly(t *testing.T) {
	diffs := []*ariadnev1.FieldDiff{
		{Path: "spec.template.spec.containers.0.image", Before: "app:v1", After: "app:v2"},
	}
	class, scaleOnly := refineDeploymentClass(diffs)
	if class != "CHANGE_CLASS_WORKLOAD_IMAGE" || scaleOnly {
		t.Fatalf("expected pure image change to classify as WORKLOAD_IMAGE, got class=%s scaleOnly=%v", class, scaleOnly)
	}
}

func TestRefineDeploymentClass_MixedChangeFallsBackToGenericSpec(t *testing.T) {
	diffs := []*ariadnev1.FieldDiff{
		{Path: "spec.template.spec.containers.0.image", Before: "app:v1", After: "app:v2"},
		{Path: "spec.template.spec.containers.0.env", Before: "[]", After: "[{}]"},
	}
	class, scaleOnly := refineDeploymentClass(diffs)
	if class != "CHANGE_CLASS_WORKLOAD_SPEC" || scaleOnly {
		t.Fatalf("expected mixed image+env change to fall back to WORKLOAD_SPEC, got class=%s scaleOnly=%v", class, scaleOnly)
	}
}

func TestRefineDeploymentClass_EmptyDiffsIsGenericSpec(t *testing.T) {
	class, scaleOnly := refineDeploymentClass(nil)
	if class != "CHANGE_CLASS_WORKLOAD_SPEC" || scaleOnly {
		t.Fatalf("expected no diffs to default to WORKLOAD_SPEC, got class=%s scaleOnly=%v", class, scaleOnly)
	}
}

func TestClassify_SecurityHintPropagatesFromResourceTable(t *testing.T) {
	res := watchedResource{
		gvr:          schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
		changeClass:  "CHANGE_CLASS_NETWORK_POLICY",
		securityHint: true,
	}
	_, hints := classify(res, []*ariadnev1.FieldDiff{{Path: "spec.podSelector"}})
	if !hints.AffectsSecurityPosture {
		t.Fatal("expected NetworkPolicy changes to set affects_security_posture")
	}
}

func TestClassify_ConfigChangeSetsAffectsConfiguration(t *testing.T) {
	res := watchedResource{
		gvr:         schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
		changeClass: "CHANGE_CLASS_CONFIG",
	}
	class, hints := classify(res, []*ariadnev1.FieldDiff{{Path: "data.flags"}})
	if class != "CHANGE_CLASS_CONFIG" || !hints.AffectsConfiguration {
		t.Fatalf("expected CONFIG class to set affects_configuration, got class=%s hints=%+v", class, hints)
	}
}

func TestClassify_NoDiffsIsNoise(t *testing.T) {
	res := watchedResource{
		gvr:         schema.GroupVersionResource{Version: "v1", Resource: "services"},
		changeClass: "CHANGE_CLASS_SERVICE",
	}
	_, hints := classify(res, nil)
	if !hints.IsNoise {
		t.Fatal("expected zero diffs to be flagged as noise")
	}
}
