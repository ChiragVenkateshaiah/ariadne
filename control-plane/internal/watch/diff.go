package watch

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
)

// ignoredPaths are fields that churn on every resync without representing a
// real change (resourceVersion, managedFields, status subresources, ...).
// Diffing these would flood the brain with noise it would have to filter
// back out on every single event -- filtering it once, here, is cheaper and
// makes "is_noise" a fact the sensor can assert rather than something every
// consumer has to re-derive.
var ignoredPaths = map[string]bool{
	"metadata.resourceVersion":  true,
	"metadata.generation":       true,
	"metadata.managedFields":    true,
	"metadata.uid":              true,
	"metadata.selfLink":         true,
	"metadata.creationTimestamp": true,
	"metadata.annotations.kubectl.kubernetes.io/last-applied-configuration": true,
}

func ignored(path string) bool {
	if ignoredPaths[path] {
		return true
	}
	return strings.HasPrefix(path, "status.") || path == "status"
}

// diffObjects computes a flat set of FieldDiffs between two unstructured
// object trees. It deliberately treats arrays as atomic (a changed slice is
// reported wholesale, not element-by-element) -- a real diff algorithm buys
// precision we don't need here, at a complexity cost we do not want in code
// nobody should have to debug at 2am before a demo.
func diffObjects(before, after map[string]any) []*ariadnev1.FieldDiff {
	var diffs []*ariadnev1.FieldDiff
	walk("", before, after, &diffs)
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return diffs
}

func walk(prefix string, before, after map[string]any, out *[]*ariadnev1.FieldDiff) {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	for k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if ignored(path) {
			continue
		}
		bv, bok := before[k]
		av, aok := after[k]

		switch {
		case !bok:
			*out = append(*out, &ariadnev1.FieldDiff{Path: path, After: toStr(av), Op: ariadnev1.DiffOp_DIFF_OP_ADDED})
		case !aok:
			*out = append(*out, &ariadnev1.FieldDiff{Path: path, Before: toStr(bv), Op: ariadnev1.DiffOp_DIFF_OP_REMOVED})
		default:
			bm, bIsMap := bv.(map[string]any)
			am, aIsMap := av.(map[string]any)
			if bIsMap && aIsMap {
				walk(path, bm, am, out)
				continue
			}
			if !reflect.DeepEqual(bv, av) {
				*out = append(*out, &ariadnev1.FieldDiff{
					Path: path, Before: toStr(bv), After: toStr(av), Op: ariadnev1.DiffOp_DIFF_OP_MODIFIED,
				})
			}
		}
	}
}

func toStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
