package watch

import (
	"context"
	"time"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
)

// Server implements ariadnev1.ChangeStreamServiceServer on top of a Broker.
// It is intentionally thin: all the actual work (watching, diffing,
// classifying) happens in Sensor: this type exists only to expose that data
// over gRPC the way proto/ariadne/v1/change.proto specifies.
type Server struct {
	ariadnev1.UnimplementedChangeStreamServiceServer
	broker *Broker
}

func NewServer(broker *Broker) *Server {
	return &Server{broker: broker}
}

// Subscribe replays retained history since the requested timestamp (so a
// brain restart never loses the window right before it reconnected), then
// switches to live tailing. There is a small window between computing the
// replay slice and registering the live subscription where an event could
// theoretically be missed; given the Broker's retention (minutes) versus a
// gRPC subscribe call's latency (milliseconds), this is an acceptable
// trade-off for a demo system, not a production exactly-once pipeline.
func (s *Server) Subscribe(req *ariadnev1.SubscribeRequest, stream ariadnev1.ChangeStreamService_SubscribeServer) error {
	since := time.Unix(0, 0)
	if req.Since != nil {
		since = req.Since.AsTime()
	}

	classFilter := make(map[ariadnev1.ChangeClass]bool, len(req.Classes))
	for _, c := range req.Classes {
		classFilter[c] = true
	}
	nsFilter := make(map[string]bool, len(req.Namespaces))
	for _, n := range req.Namespaces {
		nsFilter[n] = true
	}
	matches := func(ev *ariadnev1.ChangeEvent) bool {
		if len(classFilter) > 0 && !classFilter[ev.ChangeClass] {
			return false
		}
		if len(nsFilter) > 0 && !nsFilter[ev.Object.Namespace] {
			return false
		}
		return true
	}

	for _, ev := range s.broker.Since(since) {
		if !matches(ev) {
			continue
		}
		if err := stream.Send(ev); err != nil {
			return err
		}
	}

	live, cancel := s.broker.Subscribe()
	defer cancel()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-live:
			if !ok {
				return nil
			}
			if !matches(ev) {
				continue
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (s *Server) GetRecentChanges(_ context.Context, req *ariadnev1.GetRecentChangesRequest) (*ariadnev1.GetRecentChangesResponse, error) {
	since := time.Unix(0, 0)
	if req.Window != nil && req.Window.Start != nil {
		since = req.Window.Start.AsTime()
	}
	until := time.Now()
	if req.Window != nil && req.Window.End != nil {
		until = req.Window.End.AsTime()
	}

	var relatedTo map[string]bool
	if len(req.RelatedTo) > 0 {
		relatedTo = make(map[string]bool, len(req.RelatedTo))
		for _, ref := range req.RelatedTo {
			relatedTo[ref.Namespace+"/"+ref.Kind+"/"+ref.Name] = true
		}
	}
	var nsFilter map[string]bool
	if len(req.Namespaces) > 0 {
		nsFilter = make(map[string]bool, len(req.Namespaces))
		for _, n := range req.Namespaces {
			nsFilter[n] = true
		}
	}

	var out []*ariadnev1.ChangeEvent
	for _, ev := range s.broker.Since(since) {
		if ev.ObservedAt.AsTime().After(until) {
			continue
		}
		if nsFilter != nil && !nsFilter[ev.Object.Namespace] {
			continue
		}
		if relatedTo != nil && !relatedTo[ev.Object.Namespace+"/"+ev.Object.Kind+"/"+ev.Object.Name] {
			continue
		}
		out = append(out, ev)
	}
	return &ariadnev1.GetRecentChangesResponse{Events: out}, nil
}

// Replay is intentionally left to the embedded
// UnimplementedChangeStreamServiceServer (returns a proper gRPC Unimplemented
// status). Demo-safety replay of a recorded fixture is planned -- see
// docs/ARCHITECTURE.md's LLM record/replay guidance -- but a real status code
// beats silently streaming nothing in the meantime.
