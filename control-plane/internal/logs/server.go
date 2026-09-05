package logs

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const defaultMaxLinesPerPod = 500

// Server implements ariadnev1.LogCollectorServiceServer. It depends on a
// typed Kubernetes clientset (for pods/logs and events -- the sensor uses a
// dynamic client since it only needs generic watch/diff, but reading a
// specific container's logs is a typed, resource-specific API) and,
// optionally, a ChangeStreamService client to fold recent provenance into
// the same bundle a caller would otherwise need two round trips for.
type Server struct {
	ariadnev1.UnimplementedLogCollectorServiceServer
	client        kubernetes.Interface
	changeStream  ariadnev1.ChangeStreamServiceClient // nil is valid: recent-changes enrichment is then skipped
	auditLogPath  string
}

func NewServer(client kubernetes.Interface, changeStream ariadnev1.ChangeStreamServiceClient, auditLogPath string) *Server {
	return &Server{client: client, changeStream: changeStream, auditLogPath: auditLogPath}
}

func (s *Server) CollectEvidence(ctx context.Context, req *ariadnev1.CollectEvidenceRequest) (*ariadnev1.EvidenceBundle, error) {
	bundle := &ariadnev1.EvidenceBundle{
		CorrelationId: req.CorrelationId,
		Window:        req.Window,
	}
	var errs []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	pods, err := s.resolvePods(ctx, req)
	if err != nil {
		mu.Lock()
		errs = append(errs, fmt.Sprintf("resolving pods: %v", err))
		mu.Unlock()
	}

	if req.IncludePodLogs {
		containers := []string{""} // "" -> PodLogOptions default container; good enough for our single-container SUT pods
		for _, pod := range pods {
			for _, container := range containers {
				wg.Add(1)
				go func(pod *ariadnev1.ObjectRef, container string) {
					defer wg.Done()
					pl, err := collectPodLogs(ctx, s.client, pod, container, req.Window,
						maxLinesOrDefault(req.MaxLinesPerPod), req.LogLevelFilter, req.IncludePreviousContainer)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, fmt.Sprintf("logs for %s/%s: %v", pod.Namespace, pod.Name, err))
						return
					}
					bundle.PodLogs = append(bundle.PodLogs, pl)
				}(pod, container)
			}
		}
	}

	if req.IncludeK8SEvents {
		for _, pod := range pods {
			wg.Add(1)
			go func(pod *ariadnev1.ObjectRef) {
				defer wg.Done()
				events, err := collectK8sEvents(ctx, s.client, pod, req.Window)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, fmt.Sprintf("events for %s/%s: %v", pod.Namespace, pod.Name, err))
					return
				}
				bundle.K8SEvents = append(bundle.K8SEvents, events...)
			}(pod)
		}
	}

	if req.IncludeAuditEvents && s.auditLogPath != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			events, truncated, err := readAuditEvents(s.auditLogPath, auditFilter{window: req.Window})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Sprintf("audit log: %v", err))
				return
			}
			bundle.AuditEvents = events
			if truncated {
				bundle.Truncation = &ariadnev1.Truncation{Truncated: true, Reason: "audit query limit reached"}
			}
		}()
	}

	if req.IncludeRecentChanges && s.changeStream != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			relatedTo := pods
			resp, err := s.changeStream.GetRecentChanges(ctx, &ariadnev1.GetRecentChangesRequest{
				Window: widenWindow(req.Window, req.LookbackSeconds), RelatedTo: relatedTo,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Sprintf("recent changes: %v", err))
				return
			}
			bundle.RecentChanges = resp.Events
		}()
	}

	wg.Wait()
	bundle.CollectionErrors = errs
	return bundle, nil
}

func (s *Server) StreamPodLogs(req *ariadnev1.StreamPodLogsRequest, stream ariadnev1.LogCollectorService_StreamPodLogsServer) error {
	opts := &corev1.PodLogOptions{
		Container: req.Container, Follow: req.Follow, Timestamps: true,
	}
	if req.TailLines > 0 {
		tail := int64(req.TailLines)
		opts.TailLines = &tail
	}
	if req.Since != nil {
		t := metav1.NewTime(req.Since.AsTime())
		opts.SinceTime = &t
	}

	rc, err := s.client.CoreV1().Pods(req.Pod.Namespace).GetLogs(req.Pod.Name, opts).Stream(stream.Context())
	if err != nil {
		return fmt.Errorf("stream logs for %s/%s: %w", req.Pod.Namespace, req.Pod.Name, err)
	}
	defer rc.Close()

	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(parseLogLine(string(buf[:n]))); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) QueryAuditLog(_ context.Context, req *ariadnev1.QueryAuditLogRequest) (*ariadnev1.QueryAuditLogResponse, error) {
	if s.auditLogPath == "" {
		return &ariadnev1.QueryAuditLogResponse{Truncation: &ariadnev1.Truncation{Reason: "audit log not configured on this collector"}}, nil
	}
	f := auditFilter{
		window: req.Window, verbs: toSet(req.Verbs), resources: toSet(req.Resources),
		namespaces: toSet(req.Namespaces), userNames: toSet(req.UserNames), responseCodes: toIntSet(req.ResponseCodes),
		serviceAccountsOnly: req.ServiceAccountsOnly, excludeReadOnly: req.ExcludeReadOnly, limit: int(req.Limit),
	}
	events, truncated, err := readAuditEvents(s.auditLogPath, f)
	if err != nil {
		return nil, fmt.Errorf("reading audit log: %w", err)
	}
	resp := &ariadnev1.QueryAuditLogResponse{Events: events, SubjectActivity: aggregateSubjectActivity(events)}
	if truncated {
		resp.Truncation = &ariadnev1.Truncation{Truncated: true, Reason: "query limit reached"}
	}
	return resp, nil
}

// resolvePods expands req.Pods (explicit) and req.Selectors (label-based)
// into one deduplicated ObjectRef list.
func (s *Server) resolvePods(ctx context.Context, req *ariadnev1.CollectEvidenceRequest) ([]*ariadnev1.ObjectRef, error) {
	seen := make(map[string]*ariadnev1.ObjectRef)
	for _, p := range req.Pods {
		seen[p.Namespace+"/"+p.Name] = p
	}
	for _, sel := range req.Selectors {
		list, err := s.client.CoreV1().Pods(sel.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.LabelSelector})
		if err != nil {
			return nil, fmt.Errorf("listing pods for selector %q in %s: %w", sel.LabelSelector, sel.Namespace, err)
		}
		for _, pod := range list.Items {
			ref := &ariadnev1.ObjectRef{ApiVersion: "v1", Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, Uid: string(pod.UID)}
			seen[ref.Namespace+"/"+ref.Name] = ref
		}
	}
	out := make([]*ariadnev1.ObjectRef, 0, len(seen))
	for _, ref := range seen {
		out = append(out, ref)
	}
	return out, nil
}

func maxLinesOrDefault(v int32) int32 {
	if v > 0 {
		return v
	}
	return defaultMaxLinesPerPod
}

func widenWindow(w *ariadnev1.TimeWindow, lookbackSeconds int32) *ariadnev1.TimeWindow {
	if w == nil || lookbackSeconds <= 0 || w.Start == nil {
		return w
	}
	widened := w.Start.AsTime().Add(-time.Duration(lookbackSeconds) * time.Second)
	return &ariadnev1.TimeWindow{Start: timestamppb.New(widened), End: w.End}
}
