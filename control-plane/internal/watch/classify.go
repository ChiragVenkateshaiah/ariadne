package watch

import (
	"strings"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
)

// classify refines a resource's default ChangeClass using the actual diff,
// and computes the cheap deterministic ChangeHints. This is the sensor's one
// piece of "intelligence" -- and it is intelligence in the sense of a fixed
// decision table, not inference. Nothing here calls an LLM or guesses; every
// hint traces to a specific field path.
func classify(res watchedResource, diffs []*ariadnev1.FieldDiff) (string, *ariadnev1.ChangeHints) {
	changeClass := res.changeClass
	hints := &ariadnev1.ChangeHints{
		AffectsSecurityPosture: res.securityHint,
		ChangedFieldCount:      int32(len(diffs)),
		IsNoise:                len(diffs) == 0,
	}

	if res.gvr.Resource == "deployments" {
		changeClass, hints.IsScaleOnly = refineDeploymentClass(diffs)
	}

	switch changeClass {
	case "CHANGE_CLASS_CONFIG", "CHANGE_CLASS_SECRET":
		hints.AffectsConfiguration = true
	case "CHANGE_CLASS_ROUTE", "CHANGE_CLASS_SERVICE":
		hints.AffectsApiSurface = true
	case "CHANGE_CLASS_WORKLOAD_IMAGE", "CHANGE_CLASS_WORKLOAD_SPEC", "CHANGE_CLASS_SCALING":
		hints.AffectsRunningTraffic = true
	}

	return changeClass, hints
}

// refineDeploymentClass distinguishes three cases that all land on the same
// GVR but mean very different things for risk scoring: a pure replica change
// (scaling -- usually low risk), an image-only change (a new build rolled
// out -- track it as WORKLOAD_IMAGE, the class RISK_WEIGHTS treats as
// higher-signal than a generic spec edit), or anything else (WORKLOAD_SPEC).
func refineDeploymentClass(diffs []*ariadnev1.FieldDiff) (class string, scaleOnly bool) {
	if len(diffs) == 0 {
		return "CHANGE_CLASS_WORKLOAD_SPEC", false
	}
	if len(diffs) == 1 && diffs[0].Path == "spec.replicas" {
		return "CHANGE_CLASS_SCALING", true
	}
	imageOnly := true
	sawImage := false
	for _, d := range diffs {
		if strings.Contains(d.Path, "spec.template.spec.containers") && strings.HasSuffix(d.Path, ".image") {
			sawImage = true
			continue
		}
		imageOnly = false
	}
	if imageOnly && sawImage {
		return "CHANGE_CLASS_WORKLOAD_IMAGE", false
	}
	return "CHANGE_CLASS_WORKLOAD_SPEC", false
}
