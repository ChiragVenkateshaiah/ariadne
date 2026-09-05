// Package logs implements the LogCollector service: the component that
// makes explainability possible. When a test fails, the World Model already
// knows which pods served it and over what window (see ValidatorResult in
// proto/ariadne/v1/validation.proto), so this package fans out across those
// pods, K8s Events, and the audit log to produce one EvidenceBundle an LLM
// can reason over -- the difference between "expected 200, got 500" and a
// real root-cause narrative.
package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// collectPodLogs fetches one container's logs within `window`, parsing each
// line as JSON when possible (our own SUT emits structured slog JSON -- see
// sut/shared/logging.go) and falling back to a raw LogLine otherwise, since a
// real target's logs won't all be structured.
func collectPodLogs(ctx context.Context, client kubernetes.Interface, pod *ariadnev1.ObjectRef,
	container string, window *ariadnev1.TimeWindow, maxLines int32, levelFilter []string,
	includePrevious bool) (*ariadnev1.PodLogs, error) {

	opts := &corev1.PodLogOptions{
		Container:  container,
		Timestamps: true,
		Previous:   includePrevious,
	}
	if window.GetStart() != nil {
		t := metav1.NewTime(window.Start.AsTime())
		opts.SinceTime = &t
	}
	if maxLines > 0 {
		tail := int64(maxLines)
		opts.TailLines = &tail
	}

	stream, err := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("stream logs for %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	defer stream.Close()

	podInfo, err := client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	restartCount := int32(0)
	nodeName := ""
	if err == nil {
		nodeName = podInfo.Spec.NodeName
		for _, cs := range podInfo.Status.ContainerStatuses {
			if cs.Name == container {
				restartCount = cs.RestartCount
			}
		}
	}

	levelSet := make(map[string]bool, len(levelFilter))
	for _, l := range levelFilter {
		levelSet[strings.ToUpper(l)] = true
	}

	var lines []*ariadnev1.LogLine
	var truncated bool
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // long structured log lines are common
	for scanner.Scan() {
		line := parseLogLine(scanner.Text())
		if len(levelSet) > 0 && line.Level != "" && !levelSet[strings.ToUpper(line.Level)] {
			continue
		}
		if window.GetEnd() != nil && line.Timestamp != nil && line.Timestamp.AsTime().After(window.End.AsTime()) {
			continue
		}
		lines = append(lines, line)
		if maxLines > 0 && int32(len(lines)) >= maxLines {
			truncated = true
			break
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("scan logs for %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	return &ariadnev1.PodLogs{
		Pod:                   pod,
		Container:             container,
		NodeName:              nodeName,
		Lines:                 lines,
		FromPreviousContainer: includePrevious,
		RestartCount:          restartCount,
		Truncation:            &ariadnev1.Truncation{Truncated: truncated},
	}, nil
}

// parseLogLine handles our SUT's structured JSON logs (see
// sut/shared/logging.go's slog.NewJSONHandler) and degrades gracefully for
// anything else -- a real target's log format is not ours to assume.
func parseLogLine(raw string) *ariadnev1.LogLine {
	line := &ariadnev1.LogLine{Raw: raw}

	// kubectl-style logs with --timestamps prefix each line with an RFC3339
	// timestamp followed by a space; strip it before attempting JSON parse.
	rest := raw
	if sp := strings.IndexByte(raw, ' '); sp > 0 {
		if ts, err := time.Parse(time.RFC3339Nano, raw[:sp]); err == nil {
			line.Timestamp = timestamppb.New(ts)
			rest = raw[sp+1:]
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(rest), &parsed); err != nil {
		line.Message = rest
		return line
	}

	fields := make(map[string]string, len(parsed))
	for k, v := range parsed {
		fields[k] = fmt.Sprintf("%v", v)
	}
	line.Fields = fields

	if v, ok := parsed["level"].(string); ok {
		line.Level = v
	}
	if v, ok := parsed["msg"].(string); ok {
		line.Message = v
	} else if v, ok := parsed["message"].(string); ok {
		line.Message = v
	}
	if v, ok := parsed["trace_id"].(string); ok {
		line.TraceId = v
	}
	if v, ok := parsed["time"].(string); ok {
		if ts, err := time.Parse(time.RFC3339Nano, v); err == nil {
			line.Timestamp = timestamppb.New(ts)
		}
	}
	return line
}
