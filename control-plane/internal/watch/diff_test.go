package watch

import (
	"testing"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
)

func TestDiffObjects_AddedRemovedModified(t *testing.T) {
	before := map[string]any{"a": "1", "b": "2"}
	after := map[string]any{"b": "3", "c": "4"}

	diffs := diffObjects(before, after)
	byPath := map[string]*ariadnev1.FieldDiff{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}

	if len(diffs) != 3 {
		t.Fatalf("expected 3 diffs, got %d: %+v", len(diffs), diffs)
	}
	if d := byPath["a"]; d == nil || d.Op != ariadnev1.DiffOp_DIFF_OP_REMOVED || d.Before != "1" {
		t.Errorf("expected 'a' removed with before=1, got %+v", d)
	}
	if d := byPath["b"]; d == nil || d.Op != ariadnev1.DiffOp_DIFF_OP_MODIFIED || d.Before != "2" || d.After != "3" {
		t.Errorf("expected 'b' modified 2->3, got %+v", d)
	}
	if d := byPath["c"]; d == nil || d.Op != ariadnev1.DiffOp_DIFF_OP_ADDED || d.After != "4" {
		t.Errorf("expected 'c' added with after=4, got %+v", d)
	}
}

func TestDiffObjects_NoChange(t *testing.T) {
	obj := map[string]any{"a": "1", "nested": map[string]any{"x": "y"}}
	diffs := diffObjects(obj, obj)
	if len(diffs) != 0 {
		t.Fatalf("expected no diffs for identical objects, got %+v", diffs)
	}
}

func TestDiffObjects_NestedMapRecursion(t *testing.T) {
	before := map[string]any{"spec": map[string]any{"replicas": "1"}}
	after := map[string]any{"spec": map[string]any{"replicas": "2"}}

	diffs := diffObjects(before, after)
	if len(diffs) != 1 || diffs[0].Path != "spec.replicas" {
		t.Fatalf("expected one diff at spec.replicas, got %+v", diffs)
	}
}

func TestDiffObjects_IgnoresNoisyMetadataFields(t *testing.T) {
	before := map[string]any{
		"metadata": map[string]any{"resourceVersion": "100", "name": "foo"},
		"status":   map[string]any{"phase": "Pending"},
	}
	after := map[string]any{
		"metadata": map[string]any{"resourceVersion": "200", "name": "foo"},
		"status":   map[string]any{"phase": "Running"},
	}

	diffs := diffObjects(before, after)
	if len(diffs) != 0 {
		t.Fatalf("expected resourceVersion/status churn to be filtered out as noise, got %+v", diffs)
	}
}

func TestDiffObjects_ArraysAreAtomicNotElementwise(t *testing.T) {
	before := map[string]any{"containers": []any{"a", "b"}}
	after := map[string]any{"containers": []any{"a", "c"}}

	diffs := diffObjects(before, after)
	if len(diffs) != 1 || diffs[0].Path != "containers" {
		t.Fatalf("expected exactly one whole-array diff at 'containers', got %+v", diffs)
	}
}

func TestDiffObjects_HandlesNilMaps(t *testing.T) {
	// ADDED event: before is nil (no prior object). DELETED event: after is nil.
	diffs := diffObjects(nil, map[string]any{"a": "1"})
	if len(diffs) != 1 || diffs[0].Op != ariadnev1.DiffOp_DIFF_OP_ADDED {
		t.Fatalf("expected one ADDED diff from nil before, got %+v", diffs)
	}

	diffs = diffObjects(map[string]any{"a": "1"}, nil)
	if len(diffs) != 1 || diffs[0].Op != ariadnev1.DiffOp_DIFF_OP_REMOVED {
		t.Fatalf("expected one REMOVED diff from nil after, got %+v", diffs)
	}
}
