package pipeline

import (
	"strings"
	"testing"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/classifier"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/firefly"
)

func TestResolveMatchedAccountPrefersID(t *testing.T) {
	accounts := []firefly.Account{{ID: "1", Name: "AMAZON"}, {ID: "2", Name: "Amazon"}}

	got := resolveMatchedAccount(accounts, &classifier.DestinationResult{ID: "2", Name: "Amazon", Action: "MATCH"})
	if got.ID != "2" {
		t.Errorf("by ID: got %+v, want account 2", got)
	}

	got = resolveMatchedAccount(accounts, &classifier.DestinationResult{Name: "amazon", Action: "MATCH"})
	if got.ID != "1" {
		t.Errorf("by name fallback: got %+v, want first case-insensitive match", got)
	}

	got = resolveMatchedAccount(accounts, &classifier.DestinationResult{ID: "99", Name: "Amazon", Action: "MATCH"})
	if got.ID != "1" {
		t.Errorf("unknown ID must fall back to name: got %+v", got)
	}

	if got := resolveMatchedAccount(accounts, &classifier.DestinationResult{Name: "Lidl"}); got.ID != "" {
		t.Errorf("no match: got %+v", got)
	}
}

func TestHistoryOverrideReason(t *testing.T) {
	if r := historyOverrideReason(3, classifier.SourceJev); !strings.Contains(r, "Jev was consulted for destination") || strings.Contains(r, "AI was") {
		t.Errorf("jev source: %q", r)
	}
	for _, src := range []string{classifier.SourceJevLLM, classifier.SourceLLM, ""} {
		if r := historyOverrideReason(3, src); !strings.Contains(r, "AI was consulted for destination") {
			t.Errorf("source %q: %q", src, r)
		}
	}
	if r := historyOverrideReason(3, ""); !strings.HasPrefix(r, "Auto-matched from 3 previous transactions.") {
		t.Errorf("prefix: %q", r)
	}
}
