package logs

import (
	"context"
	"fmt"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
)

// collectK8sEvents fetches Events involving one object (a pod that served a
// failing test, typically), filtered client-side to `window` -- the Events
// API's field selectors don't support time-range filtering directly.
func collectK8sEvents(ctx context.Context, client kubernetes.Interface, obj *ariadnev1.ObjectRef,
	window *ariadnev1.TimeWindow) ([]*ariadnev1.K8SEvent, error) {

	selector := fields.SelectorFromSet(fields.Set{
		"involvedObject.name":      obj.Name,
		"involvedObject.namespace": obj.Namespace,
	})
	list, err := client.CoreV1().Events(obj.Namespace).List(ctx, metav1.ListOptions{FieldSelector: selector.String()})
	if err != nil {
		return nil, fmt.Errorf("list events for %s/%s: %w", obj.Namespace, obj.Name, err)
	}

	var out []*ariadnev1.K8SEvent
	for _, e := range list.Items {
		ts := e.LastTimestamp.Time
		if ts.IsZero() {
			ts = e.EventTime.Time
		}
		if window.GetStart() != nil && ts.Before(window.Start.AsTime()) {
			continue
		}
		if window.GetEnd() != nil && ts.After(window.End.AsTime()) {
			continue
		}
		out = append(out, &ariadnev1.K8SEvent{
			Timestamp: timestamppb.New(ts),
			InvolvedObject: &ariadnev1.ObjectRef{
				ApiVersion: e.InvolvedObject.APIVersion, Kind: e.InvolvedObject.Kind,
				Namespace: e.InvolvedObject.Namespace, Name: e.InvolvedObject.Name,
				Uid: string(e.InvolvedObject.UID),
			},
			Type: e.Type, Reason: e.Reason, Message: e.Message,
			Count: e.Count, ReportingComponent: e.ReportingController,
		})
	}
	return out, nil
}
