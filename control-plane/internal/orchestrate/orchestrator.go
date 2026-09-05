// Package orchestrate implements OrchestratorService: it turns a
// ValidatorTask into a Kubernetes Job, waits for it, and extracts the
// runner's own result payload from its stdout. It deliberately never parses
// spec_json for validator-specific meaning -- that stays opaque all the way
// to the runner image (see proto/ariadne/v1/validation.proto's comment) --
// so the Intent Spec format, or any other validator's config shape, can
// change without recompiling this package.
package orchestrate

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// resultSentinel is the contract every runner image honors: one line of
// JSON on stdout, prefixed by this marker, carrying whatever that
// validator's result shape is. The orchestrator extracts it verbatim into
// ValidatorResult.result_payload_json without understanding its contents.
const resultSentinel = "###ARIADNE-RESULT###"

const defaultTimeoutSeconds = 60
const defaultMaxParallelism = 4

type Server struct {
	ariadnev1.UnimplementedOrchestratorServiceServer
	client           kubernetes.Interface
	defaultNamespace string
}

func NewServer(client kubernetes.Interface, defaultNamespace string) *Server {
	return &Server{client: client, defaultNamespace: defaultNamespace}
}

func (s *Server) RunValidators(req *ariadnev1.RunValidatorsRequest, stream ariadnev1.OrchestratorService_RunValidatorsServer) error {
	ns := req.Namespace
	if ns == "" {
		ns = s.defaultNamespace
	}
	maxParallel := int(req.MaxParallelism)
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallelism
	}

	ctx := stream.Context()
	sem := make(chan struct{}, maxParallel)
	results := make(chan *ariadnev1.ValidatorResult, len(req.Tasks))
	var wg sync.WaitGroup

	for _, task := range req.Tasks {
		task := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- s.runOneTask(ctx, ns, req.RunId, task)
		}()
	}
	go func() { wg.Wait(); close(results) }()

	for res := range results {
		if err := stream.Send(res); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) runOneTask(ctx context.Context, ns, runID string, task *ariadnev1.ValidatorTask) *ariadnev1.ValidatorResult {
	startedAt := time.Now()
	jobName := fmt.Sprintf("ariadne-%s", sanitizeName(task.TaskId))

	timeout := time.Duration(task.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds * time.Second
	}
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	job := buildJob(ns, jobName, task, int64(timeout.Seconds()))
	if _, err := s.client.BatchV1().Jobs(ns).Create(taskCtx, job, metav1.CreateOptions{}); err != nil {
		return errored(runID, task, startedAt, fmt.Errorf("creating job: %w", err))
	}
	defer s.cleanupJob(ns, jobName)

	phase, podName, waitErr := s.waitForJob(taskCtx, ns, jobName)
	finishedAt := time.Now()
	if waitErr != nil {
		return &ariadnev1.ValidatorResult{
			RunId: runID, TaskId: task.TaskId, Kind: task.Kind,
			Phase: ariadnev1.ValidatorPhase_VALIDATOR_PHASE_TIMED_OUT, Message: waitErr.Error(),
			StartedAt: timestamppb.New(startedAt), FinishedAt: timestamppb.New(finishedAt),
		}
	}

	logs, logErr := s.podLogs(ctx, ns, podName)
	payload, truncation := extractSentinel(logs)
	if logErr != nil && truncation == nil {
		truncation = &ariadnev1.Truncation{Truncated: true, Reason: fmt.Sprintf("could not read pod logs: %v", logErr)}
	}

	return &ariadnev1.ValidatorResult{
		RunId: runID, TaskId: task.TaskId, Kind: task.Kind, Phase: phase,
		StartedAt: timestamppb.New(startedAt), FinishedAt: timestamppb.New(finishedAt),
		ResultPayloadJson: payload, PayloadTruncation: truncation,
		ExecutionWindow: &ariadnev1.TimeWindow{Start: timestamppb.New(startedAt), End: timestamppb.New(finishedAt)},
	}
}

func (s *Server) cleanupJob(ns, name string) {
	propagate := metav1.DeletePropagationBackground
	_ = s.client.BatchV1().Jobs(ns).Delete(context.Background(), name, metav1.DeleteOptions{PropagationPolicy: &propagate})
}

func (s *Server) waitForJob(ctx context.Context, ns, name string) (ariadnev1.ValidatorPhase, string, error) {
	for {
		job, err := s.client.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return ariadnev1.ValidatorPhase_VALIDATOR_PHASE_ERRORED, "", err
		}
		if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			podName, err := s.findPodForJob(ctx, ns, name)
			if err != nil {
				return ariadnev1.ValidatorPhase_VALIDATOR_PHASE_ERRORED, "", err
			}
			if job.Status.Succeeded > 0 {
				return ariadnev1.ValidatorPhase_VALIDATOR_PHASE_SUCCEEDED, podName, nil
			}
			return ariadnev1.ValidatorPhase_VALIDATOR_PHASE_FAILED, podName, nil
		}
		select {
		case <-ctx.Done():
			return ariadnev1.ValidatorPhase_VALIDATOR_PHASE_TIMED_OUT, "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Server) findPodForJob(ctx context.Context, ns, jobName string) (string, error) {
	pods, err := s.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pod found for job %s", jobName)
	}
	return pods.Items[0].Name, nil
}

func (s *Server) podLogs(ctx context.Context, ns, podName string) (string, error) {
	stream, err := s.client.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	b, err := io.ReadAll(stream)
	return string(b), err
}

func extractSentinel(logs string) (string, *ariadnev1.Truncation) {
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(line, resultSentinel) {
			return strings.TrimSpace(strings.TrimPrefix(line, resultSentinel)), nil
		}
	}
	return "{}", &ariadnev1.Truncation{Truncated: true, Reason: "no result sentinel found in pod output"}
}

// buildJob mounts spec_json as an env var rather than the mounted file the
// proto comment envisions -- a pragmatic simplification for how small our
// validators' configs are today. Revisit if a spec ever approaches the
// ~32KB env var practical limit.
func buildJob(ns, name string, task *ariadnev1.ValidatorTask, deadlineSeconds int64) *batchv1.Job {
	backoff := int32(0)

	env := make([]corev1.EnvVar, 0, len(task.Env)+1)
	for k, v := range task.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	if task.SpecJson != "" {
		env = append(env, corev1.EnvVar{Name: "ARIADNE_SPEC_JSON", Value: task.SpecJson})
	}

	podLabels := map[string]string{"ariadne.dev/managed-by": "orchestrator", "ariadne.dev/task-id": sanitizeName(task.TaskId)}
	for k, v := range task.PodLabels {
		podLabels[k] = v
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"ariadne.dev/managed-by": "orchestrator"}},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: task.ServiceAccount,
					HostNetwork:        task.HostNetwork,
					Containers: []corev1.Container{{
						Name: "runner", Image: task.Image, Args: task.Args, Env: env,
					}},
				},
			},
		},
	}
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		out = "task"
	}
	return out
}

func errored(runID string, task *ariadnev1.ValidatorTask, startedAt time.Time, err error) *ariadnev1.ValidatorResult {
	return &ariadnev1.ValidatorResult{
		RunId: runID, TaskId: task.TaskId, Kind: task.Kind,
		Phase: ariadnev1.ValidatorPhase_VALIDATOR_PHASE_ERRORED, Message: err.Error(),
		StartedAt: timestamppb.New(startedAt), FinishedAt: timestamppb.New(time.Now()),
	}
}
