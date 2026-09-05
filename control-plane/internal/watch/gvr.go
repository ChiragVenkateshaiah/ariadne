// Package watch implements the Cluster Sensor: it watches a fixed set of
// Kubernetes resource kinds via a dynamic informer factory, normalizes every
// add/update/delete into a ChangeEvent, and classifies it cheaply and
// deterministically -- no LLM in this path. Interpretation of WHY a change
// matters is the brain's job (see docs/ARCHITECTURE.md); this package's only
// contract is: never miss a change, never lie about what changed.
package watch

import "k8s.io/apimachinery/pkg/runtime/schema"

// watchedResource pairs a GVR with the ChangeClass it deterministically maps
// to. One resource kind is always exactly one ChangeClass -- ambiguity (e.g.
// "is this ConfigMap change actually a feature flag or just log verbosity")
// is exactly the judgment the brain's LLM layer exists to make, not this one.
type watchedResource struct {
	gvr          schema.GroupVersionResource
	changeClass  string
	securityHint bool // affects_security_posture
}

// Resources is the fixed watch list. Extending coverage (e.g. StatefulSet,
// CronJob) means adding one line here plus one ChangeClass in the proto --
// nothing else in this package needs to change, by design.
var Resources = []watchedResource{
	{gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, changeClass: "CHANGE_CLASS_WORKLOAD_SPEC"},
	{gvr: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, changeClass: "CHANGE_CLASS_CONFIG"},
	{gvr: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}, changeClass: "CHANGE_CLASS_SECRET"},
	{gvr: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}, changeClass: "CHANGE_CLASS_SERVICE"},
	{gvr: schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, changeClass: "CHANGE_CLASS_ROUTE"},
	{gvr: schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}, changeClass: "CHANGE_CLASS_NETWORK_POLICY", securityHint: true},
	{gvr: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}, changeClass: "CHANGE_CLASS_RBAC", securityHint: true},
	{gvr: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, changeClass: "CHANGE_CLASS_RBAC", securityHint: true},
	{gvr: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, changeClass: "CHANGE_CLASS_RBAC", securityHint: true},
	{gvr: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, changeClass: "CHANGE_CLASS_RBAC", securityHint: true},
	{gvr: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, changeClass: "CHANGE_CLASS_RBAC", securityHint: true},
	{gvr: schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}, changeClass: "CHANGE_CLASS_SCALING"},
}
