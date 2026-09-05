package orchestrate

import "testing"

func TestExtractSentinel_FindsLineAmongOtherOutput(t *testing.T) {
	logs := "starting up\nprobing target\n###ARIADNE-RESULT###{\"reachable\":true}\ndone\n"
	payload, truncation := extractSentinel(logs)
	if payload != `{"reachable":true}` {
		t.Fatalf("expected extracted payload, got %q", payload)
	}
	if truncation != nil {
		t.Fatalf("expected no truncation when sentinel is found, got %+v", truncation)
	}
}

func TestExtractSentinel_MissingSentinelIsTruncated(t *testing.T) {
	payload, truncation := extractSentinel("just some logs\nwith no sentinel\n")
	if payload != "{}" {
		t.Fatalf("expected empty-object fallback payload, got %q", payload)
	}
	if truncation == nil || !truncation.Truncated {
		t.Fatal("expected a truncation flag when no sentinel line is present")
	}
}

func TestSanitizeName_LowercasesAndReplacesInvalidChars(t *testing.T) {
	got := sanitizeName("Web-UI_to_Postgres.Test")
	want := "web-ui-to-postgres-test"
	if got != want {
		t.Fatalf("sanitizeName() = %q, want %q", got, want)
	}
}

func TestSanitizeName_TruncatesLongNames(t *testing.T) {
	long := "this-is-a-very-long-task-id-that-exceeds-the-kubernetes-name-length-budget"
	got := sanitizeName(long)
	if len(got) > 40 {
		t.Fatalf("expected sanitized name capped at 40 chars, got %d: %q", len(got), got)
	}
}

func TestSanitizeName_EmptyFallsBackToDefault(t *testing.T) {
	if got := sanitizeName("!!!"); got != "task" {
		t.Fatalf("expected an all-invalid-chars name to fall back to \"task\", got %q", got)
	}
}
