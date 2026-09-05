// Audit log reading: the data source behind the behavioural security story
// (docs/ARCHITECTURE.md's K03 example -- "this ServiceAccount is granted 47
// permissions and exercised 3"). The API server writes one JSON object per
// line (audit.k8s.io/v1 Event, see deploy/audit-policy/policy.yaml); this
// file reads that back out, filters it, and aggregates it into
// SubjectActivity so the brain can reason over behaviour without holding
// every raw event.
package logs

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// rawAuditEvent mirrors just the fields of a Kubernetes audit.k8s.io/v1
// Event we actually use. The real object has more; decoding only what we
// need keeps this resilient to fields we don't yet care about.
type rawAuditEvent struct {
	AuditID    string `json:"auditID"`
	Stage      string `json:"stage"`
	Verb       string `json:"verb"`
	RequestURI string `json:"requestURI"`
	User       struct {
		Username string   `json:"username"`
		Groups   []string `json:"groups"`
	} `json:"user"`
	SourceIPs []string `json:"sourceIPs"`
	UserAgent string   `json:"userAgent"`
	ObjectRef *struct {
		Resource    string `json:"resource"`
		Subresource string `json:"subresource"`
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
		APIGroup    string `json:"apiGroup"`
	} `json:"objectRef"`
	ResponseStatus *struct {
		Code int32 `json:"code"`
	} `json:"responseStatus"`
	RequestReceivedTimestamp time.Time         `json:"requestReceivedTimestamp"`
	Annotations              map[string]string `json:"annotations"`
}

type auditFilter struct {
	window              *ariadnev1.TimeWindow
	verbs               map[string]bool
	resources           map[string]bool
	namespaces          map[string]bool
	userNames           map[string]bool
	responseCodes       map[int32]bool
	serviceAccountsOnly bool
	excludeReadOnly     bool
	limit               int
}

var readOnlyVerbs = map[string]bool{"get": true, "list": true, "watch": true}

func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	s := make(map[string]bool, len(items))
	for _, i := range items {
		s[i] = true
	}
	return s
}

func toIntSet(items []int32) map[int32]bool {
	if len(items) == 0 {
		return nil
	}
	s := make(map[int32]bool, len(items))
	for _, i := range items {
		s[i] = true
	}
	return s
}

// readAuditEvents scans the audit log file top to bottom, decoding and
// filtering as it goes. The file is demo-scale (one API server, a handful of
// namespaces); a real deployment would need an index or a log pipeline
// rather than a linear scan, but that is future work, not a correctness gap
// for what this system is built to demonstrate today.
func readAuditEvents(path string, f auditFilter) ([]*ariadnev1.AuditEvent, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()

	var out []*ariadnev1.AuditEvent
	truncated := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		var raw rawAuditEvent
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue // a malformed line shouldn't abort the whole read
		}
		if raw.Stage != "ResponseComplete" {
			continue // Metadata/RequestReceived stages duplicate the same request; keep only the outcome
		}
		ev := toAuditEvent(&raw)
		if !matchesFilter(ev, f) {
			continue
		}
		out = append(out, ev)
		if f.limit > 0 && len(out) >= f.limit {
			truncated = true
			break
		}
	}
	return out, truncated, scanner.Err()
}

func toAuditEvent(raw *rawAuditEvent) *ariadnev1.AuditEvent {
	ev := &ariadnev1.AuditEvent{
		AuditId:   raw.AuditID,
		Timestamp: timestamppb.New(raw.RequestReceivedTimestamp),
		Stage:     raw.Stage,
		Verb:      raw.Verb,
		UserName:  raw.User.Username,
		UserGroups: raw.User.Groups,
		SourceIps: raw.SourceIPs,
		UserAgent: raw.UserAgent,
		RequestUri: raw.RequestURI,
	}
	if isSA, ns, name := parseServiceAccount(raw.User.Username); isSA {
		ev.IsServiceAccount, ev.SaNamespace, ev.SaName = true, ns, name
	}
	if raw.ObjectRef != nil {
		ev.ApiGroup = raw.ObjectRef.APIGroup
		ev.Resource = raw.ObjectRef.Resource
		ev.Subresource = raw.ObjectRef.Subresource
		ev.Namespace = raw.ObjectRef.Namespace
		ev.Name = raw.ObjectRef.Name
	}
	if raw.ResponseStatus != nil {
		ev.ResponseCode = raw.ResponseStatus.Code
	}
	if d, ok := raw.Annotations["authorization.k8s.io/decision"]; ok {
		ev.Decision = d
	}
	if r, ok := raw.Annotations["authorization.k8s.io/reason"]; ok {
		ev.AuthzReason = r
	}
	return ev
}

// parseServiceAccount extracts (namespace, name) from Kubernetes' fixed
// "system:serviceaccount:<namespace>:<name>" username format.
func parseServiceAccount(username string) (isSA bool, namespace, name string) {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, prefix) {
		return false, "", ""
	}
	parts := strings.SplitN(strings.TrimPrefix(username, prefix), ":", 2)
	if len(parts) != 2 {
		return true, "", ""
	}
	return true, parts[0], parts[1]
}

func matchesFilter(ev *ariadnev1.AuditEvent, f auditFilter) bool {
	ts := ev.Timestamp.AsTime()
	if f.window.GetStart() != nil && ts.Before(f.window.Start.AsTime()) {
		return false
	}
	if f.window.GetEnd() != nil && ts.After(f.window.End.AsTime()) {
		return false
	}
	if f.verbs != nil && !f.verbs[ev.Verb] {
		return false
	}
	if f.resources != nil && !f.resources[ev.Resource] {
		return false
	}
	if f.namespaces != nil && !f.namespaces[ev.Namespace] {
		return false
	}
	if f.userNames != nil && !f.userNames[ev.UserName] {
		return false
	}
	if f.responseCodes != nil && !f.responseCodes[ev.ResponseCode] {
		return false
	}
	if f.serviceAccountsOnly && !ev.IsServiceAccount {
		return false
	}
	if f.excludeReadOnly && readOnlyVerbs[ev.Verb] {
		return false
	}
	return true
}

// aggregateSubjectActivity is what turns a pile of audit events into the
// "granted 47 permissions, exercised 3" story: one row per identity,
// summarising what it actually did.
func aggregateSubjectActivity(events []*ariadnev1.AuditEvent) []*ariadnev1.SubjectActivity {
	type accum struct {
		isSA           bool
		verbs          map[string]bool
		resources      map[string]bool
		namespaces     map[string]bool
		total          int64
		forbidden      int64
		first, last    time.Time
	}
	bySubject := make(map[string]*accum)

	for _, ev := range events {
		a, ok := bySubject[ev.UserName]
		if !ok {
			a = &accum{isSA: ev.IsServiceAccount, verbs: map[string]bool{}, resources: map[string]bool{}, namespaces: map[string]bool{}}
			bySubject[ev.UserName] = a
		}
		a.verbs[ev.Verb] = true
		if ev.Resource != "" {
			resourceKey := ev.Resource
			if ev.ApiGroup != "" {
				resourceKey = ev.ApiGroup + "/" + ev.Resource
			}
			a.resources[resourceKey] = true
		}
		if ev.Namespace != "" {
			a.namespaces[ev.Namespace] = true
		}
		a.total++
		if ev.ResponseCode == 401 || ev.ResponseCode == 403 {
			a.forbidden++
		}
		ts := ev.Timestamp.AsTime()
		if a.first.IsZero() || ts.Before(a.first) {
			a.first = ts
		}
		if ts.After(a.last) {
			a.last = ts
		}
	}

	out := make([]*ariadnev1.SubjectActivity, 0, len(bySubject))
	for subject, a := range bySubject {
		out = append(out, &ariadnev1.SubjectActivity{
			Subject: subject, IsServiceAccount: a.isSA,
			VerbsUsed: keysOf(a.verbs), ResourcesTouched: keysOf(a.resources), NamespacesTouched: keysOf(a.namespaces),
			TotalRequests: a.total, ForbiddenRequests: a.forbidden,
			FirstSeen: timestamppb.New(a.first), LastSeen: timestamppb.New(a.last),
		})
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
