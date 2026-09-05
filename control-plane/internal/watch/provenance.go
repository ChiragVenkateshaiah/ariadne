package watch

import (
	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// extractProvenance pulls what the object itself can honestly tell us about
// its own change. Two things are deliberately NOT populated here:
//
//   - actor/actor_kind (the real identity that issued the write): a live
//     object's managedFields records the field MANAGER (a tool: "kubectl",
//     "argocd-application-controller"), never the human or ServiceAccount
//     that invoked it. That identity only exists in the API server's audit
//     log. Faking it from the manager name would look like provenance while
//     actually being a guess -- the correct source is
//     LogCollector.QueryAuditLog, correlated by timestamp, not this sensor.
//   - git_commit_sha/git_ref/commit_message: only populated if the deployer
//     annotated the object (ariadne.dev/git-sha etc.) -- a real, common CI
//     practice, but never fabricated when absent.
func extractProvenance(obj *unstructured.Unstructured) *ariadnev1.Provenance {
	p := &ariadnev1.Provenance{ActorKind: "unknown"}

	fields := obj.GetManagedFields()
	if len(fields) > 0 {
		// Prefer the manager that most recently applied/updated spec-shaped
		// content over a stale first entry.
		p.Manager = fields[len(fields)-1].Manager
	}

	annotations := obj.GetAnnotations()
	p.GitCommitSha = annotations["ariadne.dev/git-sha"]
	p.GitRef = annotations["ariadne.dev/git-ref"]
	p.CommitMessage = annotations["ariadne.dev/commit-message"]
	p.ChangeRequest = annotations["ariadne.dev/change-request"]

	return p
}

// extractContainerImage returns the first container's image from a
// Deployment/StatefulSet-shaped spec, or "" if the path doesn't exist -- used
// to populate Provenance.image_before / image_after for workload changes.
func extractContainerImage(obj map[string]any) string {
	containers, found, err := unstructured.NestedSlice(obj, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		return ""
	}
	c, ok := containers[0].(map[string]any)
	if !ok {
		return ""
	}
	image, _, _ := unstructured.NestedString(c, "image")
	return image
}
