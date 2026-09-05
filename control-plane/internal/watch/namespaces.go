package watch

import "sync"

// WatchLabel marks which namespaces the sensor pays attention to. Cluster-
// scoped resources (ClusterRole, ClusterRoleBinding) have no namespace and
// are always included -- an RBAC change at that scope is relevant regardless
// of which namespaces are "the app".
const WatchLabel = "ariadne.dev/watched"

// namespaceSet is a live, thread-safe view of which namespaces currently
// carry WatchLabel=true, kept in sync by the sensor's Namespace informer (see
// sensor.go -- Namespace is watched via the same dynamic-informer machinery
// as everything else, just never emitted as a ChangeEvent itself). This
// exists so the demo's manifest-level decision (deploy/sut/00-namespace.yaml
// labels "travel") is the single source of truth for scope -- nothing here
// hardcodes a namespace name.
type namespaceSet struct {
	mu   sync.RWMutex
	name map[string]bool
}

func newNamespaceSet() *namespaceSet {
	return &namespaceSet{name: make(map[string]bool)}
}

func (s *namespaceSet) set(ns string, watched bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if watched {
		s.name[ns] = true
	} else {
		delete(s.name, ns)
	}
}

func (s *namespaceSet) includes(ns string) bool {
	if ns == "" {
		return true // cluster-scoped object
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name[ns]
}
