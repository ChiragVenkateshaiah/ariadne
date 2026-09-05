package watch

import (
	"context"
	"log/slog"
	"time"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

var namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// deploymentsGVR is looked up directly (rather than searched for in
// Resources) because references.go's cross-referencing needs it by name.
var deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

// Sensor owns the dynamic informer factory and turns raw Kubernetes events
// into published ChangeEvents. It holds no opinion about WHO consumes those
// events -- that is the Broker's and the gRPC server's job (server.go).
type Sensor struct {
	broker  *Broker
	logger  *slog.Logger
	nsSet   *namespaceSet
	factory dynamicinformer.DynamicSharedInformerFactory
}

func NewSensor(client dynamic.Interface, broker *Broker, logger *slog.Logger, resync time.Duration) *Sensor {
	return &Sensor{
		broker:  broker,
		logger:  logger,
		nsSet:   newNamespaceSet(),
		factory: dynamicinformer.NewDynamicSharedInformerFactory(client, resync),
	}
}

// Start registers all informers and blocks until their caches are synced.
// The caller owns ctx's lifetime; cancelling it stops the sensor.
func (s *Sensor) Start(ctx context.Context) error {
	nsInformer := s.factory.ForResource(namespaceGVR).Informer()
	nsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { s.handleNamespace(obj) },
		UpdateFunc: func(_, newObj any) { s.handleNamespace(newObj) },
		DeleteFunc: func(obj any) { s.handleNamespaceDelete(obj) },
	})

	deployInformer := s.factory.ForResource(deploymentsGVR).Informer()

	for _, res := range Resources {
		res := res
		informer := s.factory.ForResource(res.gvr).Informer()
		informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) { s.handleEvent(res, ariadnev1.ChangeOperation_CHANGE_OPERATION_ADDED, nil, obj, informer) },
			UpdateFunc: func(oldObj, newObj any) {
				s.handleEvent(res, ariadnev1.ChangeOperation_CHANGE_OPERATION_MODIFIED, oldObj, newObj, informer)
			},
			DeleteFunc: func(obj any) { s.handleEvent(res, ariadnev1.ChangeOperation_CHANGE_OPERATION_DELETED, obj, nil, informer) },
		})
	}

	s.factory.Start(ctx.Done())
	synced := s.factory.WaitForCacheSync(ctx.Done())
	for gvr, ok := range synced {
		if !ok {
			s.logger.Error("informer cache failed to sync", "gvr", gvr.String())
		}
	}
	s.logger.Info("all informer caches synced", "watched_resources", len(Resources))

	_ = deployInformer // kept started via the factory; read through s.deployments()
	return nil
}

func (s *Sensor) deployments() []*unstructured.Unstructured {
	objs, err := s.factory.ForResource(deploymentsGVR).Lister().List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok {
			out = append(out, u)
		}
	}
	return out
}

func (s *Sensor) handleNamespace(obj any) {
	u, ok := toUnstructured(obj)
	if !ok {
		return
	}
	s.nsSet.set(u.GetName(), u.GetLabels()[WatchLabel] == "true")
}

func (s *Sensor) handleNamespaceDelete(obj any) {
	u, ok := toUnstructured(obj)
	if !ok {
		return
	}
	s.nsSet.set(u.GetName(), false)
}

func toUnstructured(obj any) (*unstructured.Unstructured, bool) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u, true
	}
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if u, ok := tomb.Obj.(*unstructured.Unstructured); ok {
			return u, true
		}
	}
	return nil, false
}

// handleEvent is the single funnel every watched resource's Add/Update/Delete
// passes through. `informer` is accepted per-call (rather than stored once)
// because each GVR has its own; it is unused today but kept in the signature
// since a per-resource cache lookup (e.g. resolving owner references) is the
// natural next extension point.
func (s *Sensor) handleEvent(res watchedResource, op ariadnev1.ChangeOperation, oldObj, newObj any, _ cache.SharedIndexInformer) {
	var before, after *unstructured.Unstructured
	if oldObj != nil {
		before, _ = toUnstructured(oldObj)
	}
	if newObj != nil {
		after, _ = toUnstructured(newObj)
	}
	ref := after
	if ref == nil {
		ref = before
	}
	if ref == nil {
		return
	}
	if !s.nsSet.includes(ref.GetNamespace()) {
		return
	}

	var beforeMap, afterMap map[string]any
	if before != nil {
		beforeMap = before.Object
	}
	if after != nil {
		afterMap = after.Object
	}
	diffs := diffObjects(beforeMap, afterMap)

	// Pure resync noise: an Update fired with no actual field change. Never
	// suppress Add/Delete this way -- those are meaningful even with an
	// empty diff (e.g. deleting an object we never saw modified).
	if op == ariadnev1.ChangeOperation_CHANGE_OPERATION_MODIFIED && len(diffs) == 0 {
		return
	}

	changeClass, hints := classify(res, diffs)

	if changeClass == "CHANGE_CLASS_CONFIG" || changeClass == "CHANGE_CLASS_SECRET" {
		kind := "ConfigMap"
		if changeClass == "CHANGE_CLASS_SECRET" {
			kind = "Secret"
		}
		hints.TouchedWorkloadNames = findReferencingWorkloadNames(s.deployments(), ref.GetNamespace(), kind, ref.GetName())
	}

	provenance := extractProvenance(ref)
	if res.gvr.Resource == "deployments" {
		provenance.ImageBefore = extractContainerImage(beforeMap)
		provenance.ImageAfter = extractContainerImage(afterMap)
	}

	occurredAt := time.Now()
	if op == ariadnev1.ChangeOperation_CHANGE_OPERATION_ADDED {
		occurredAt = ref.GetCreationTimestamp().Time
	}

	ev := &ariadnev1.ChangeEvent{
		Id:          ulid.Make().String(),
		ObservedAt:  timestamppb.Now(),
		OccurredAt:  timestamppb.New(occurredAt),
		Source:      ariadnev1.ChangeSource_CHANGE_SOURCE_KUBERNETES,
		ChangeClass: ariadnev1.ChangeClass(ariadnev1.ChangeClass_value[changeClass]),
		Operation:   op,
		Object: &ariadnev1.ObjectRef{
			ApiVersion:      ref.GetAPIVersion(),
			Kind:            ref.GetKind(),
			Namespace:       ref.GetNamespace(),
			Name:            ref.GetName(),
			Uid:             string(ref.GetUID()),
			ResourceVersion: ref.GetResourceVersion(),
		},
		Diffs:              diffs,
		Provenance:         provenance,
		Hints:              hints,
		Labels:             ref.GetLabels(),
		RawObjectJson:       toStr(afterMap),
		PreviousObjectJson:  toStr(beforeMap),
	}

	s.logger.Info("change detected",
		"class", changeClass, "op", op.String(),
		"kind", ref.GetKind(), "namespace", ref.GetNamespace(), "name", ref.GetName(),
		"fields_changed", len(diffs))

	s.broker.Publish(ev)
}
