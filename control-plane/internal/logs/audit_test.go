package logs

import (
	"testing"
	"time"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestParseServiceAccount_RecognizesTheFixedKubernetesFormat(t *testing.T) {
	isSA, ns, name := parseServiceAccount("system:serviceaccount:travel:booking-api")
	if !isSA || ns != "travel" || name != "booking-api" {
		t.Fatalf("got isSA=%v ns=%q name=%q, want true/travel/booking-api", isSA, ns, name)
	}
}

func TestParseServiceAccount_RejectsHumanUsers(t *testing.T) {
	isSA, _, _ := parseServiceAccount("kubernetes-admin")
	if isSA {
		t.Fatal("expected a human username to not be classified as a ServiceAccount")
	}
}

func TestToAuditEvent_ExtractsServiceAccountIdentity(t *testing.T) {
	raw := &rawAuditEvent{
		Verb:                     "get",
		RequestReceivedTimestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	raw.User.Username = "system:serviceaccount:travel:pricing-svc"
	raw.ObjectRef = &struct {
		Resource    string `json:"resource"`
		Subresource string `json:"subresource"`
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
		APIGroup    string `json:"apiGroup"`
	}{Resource: "configmaps", Namespace: "travel", Name: "pricing-flags"}

	ev := toAuditEvent(raw)
	if !ev.IsServiceAccount || ev.SaNamespace != "travel" || ev.SaName != "pricing-svc" {
		t.Fatalf("expected SA identity extracted, got %+v", ev)
	}
	if ev.Resource != "configmaps" || ev.Namespace != "travel" {
		t.Fatalf("expected objectRef fields carried through, got %+v", ev)
	}
}

func TestMatchesFilter_ResponseCodeFilterFindsAuthzDrift(t *testing.T) {
	ev := &ariadnev1.AuditEvent{Timestamp: timestamppb.Now(), ResponseCode: 403}
	f := auditFilter{responseCodes: toIntSet([]int32{401, 403})}
	if !matchesFilter(ev, f) {
		t.Fatal("expected a 403 to match a [401,403] response-code filter")
	}

	ev200 := &ariadnev1.AuditEvent{Timestamp: timestamppb.Now(), ResponseCode: 200}
	if matchesFilter(ev200, f) {
		t.Fatal("expected a 200 to NOT match a [401,403] response-code filter")
	}
}

func TestMatchesFilter_ExcludeReadOnly(t *testing.T) {
	f := auditFilter{excludeReadOnly: true}
	getEv := &ariadnev1.AuditEvent{Timestamp: timestamppb.Now(), Verb: "get"}
	patchEv := &ariadnev1.AuditEvent{Timestamp: timestamppb.Now(), Verb: "patch"}
	if matchesFilter(getEv, f) {
		t.Fatal("expected a read-only verb to be excluded when excludeReadOnly is set")
	}
	if !matchesFilter(patchEv, f) {
		t.Fatal("expected a write verb to pass through when excludeReadOnly is set")
	}
}

func TestAggregateSubjectActivity_TheK03Story(t *testing.T) {
	// The exact claim the K03 finding is built on: a ServiceAccount granted
	// many permissions but observed exercising only a few, plus a 403 that
	// should surface as a forbidden-request count.
	events := []*ariadnev1.AuditEvent{
		{UserName: "system:serviceaccount:travel:booking-api", IsServiceAccount: true, Verb: "get", Resource: "configmaps", Namespace: "travel", ResponseCode: 200, Timestamp: timestamppb.New(time.Unix(100, 0))},
		{UserName: "system:serviceaccount:travel:booking-api", IsServiceAccount: true, Verb: "get", Resource: "configmaps", Namespace: "travel", ResponseCode: 200, Timestamp: timestamppb.New(time.Unix(200, 0))},
		{UserName: "system:serviceaccount:travel:booking-api", IsServiceAccount: true, Verb: "list", Resource: "secrets", Namespace: "travel", ResponseCode: 403, Timestamp: timestamppb.New(time.Unix(300, 0))},
	}

	activity := aggregateSubjectActivity(events)
	if len(activity) != 1 {
		t.Fatalf("expected one subject, got %d", len(activity))
	}
	a := activity[0]
	if a.TotalRequests != 3 || a.ForbiddenRequests != 1 {
		t.Fatalf("expected total=3 forbidden=1, got total=%d forbidden=%d", a.TotalRequests, a.ForbiddenRequests)
	}
	if len(a.VerbsUsed) != 2 || len(a.ResourcesTouched) != 2 {
		t.Fatalf("expected 2 distinct verbs and 2 distinct resources, got verbs=%v resources=%v", a.VerbsUsed, a.ResourcesTouched)
	}
	if !a.FirstSeen.AsTime().Equal(time.Unix(100, 0)) || !a.LastSeen.AsTime().Equal(time.Unix(300, 0)) {
		t.Fatalf("expected first/last seen to span the full event range, got first=%v last=%v",
			a.FirstSeen.AsTime(), a.LastSeen.AsTime())
	}
}
