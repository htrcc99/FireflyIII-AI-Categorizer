package classifier

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func transferAccounts() []AccountCandidate {
	return []AccountCandidate{{ID: "c1", Name: "Checking"}, {ID: "s1", Name: "Savings"}}
}

// respondTransfer answers the single transfer question with label at probability p.
func respondTransfer(t *testing.T, label string, p float64) func(decisionRequest) (decisionResponse, error) {
	return func(r decisionRequest) (decisionResponse, error) {
		q, ok := r.Questions["account"]
		if !ok || len(r.Questions) != 1 {
			t.Fatalf("questions = %+v, want only \"account\"", r.Questions)
		}
		k := keyFor(t, q, label)
		return decisionResponse{Answers: map[string]choiceAnswer{
			"account": {Type: "choice", Choice: k, Probabilities: map[string]float64{k: p}},
		}, Raw: `{"answers":"…"}`}, nil
	}
}

func TestSuggestTransferJevConfident(t *testing.T) {
	var sent decisionRequest
	inner := respondTransfer(t, "Savings", 0.9)
	jev := &fakeDecider{fn: func(r decisionRequest) (decisionResponse, error) { sent = r; return inner(r) }}
	llm := &fakeLLM{}
	s, err := SuggestTransfer(context.Background(), newCascade(llm, jev, DefaultThresholds, ""), "MOVE TO SAVINGS", transferAccounts())
	if err != nil {
		t.Fatal(err)
	}
	if s.AccountID != "s1" || s.AccountName != "Savings" || s.Source != SourceJev || llm.calls != 0 {
		t.Errorf("suggestion = %+v, llm calls = %d", s, llm.calls)
	}
	if sent.Model != "fake-jev" || !strings.Contains(prettyJSON(sent.State), "MOVE TO SAVINGS") {
		t.Errorf("request = %+v", sent)
	}
}

func TestSuggestTransferJevMiddleBandUsesLLM(t *testing.T) {
	jev := &fakeDecider{fn: respondTransfer(t, "Savings", 0.6)}
	llm := &fakeLLM{res: Result{RawResponse: `{"account": "checking"}`}}
	s, err := SuggestTransfer(context.Background(), newCascade(llm, jev, DefaultThresholds, ""), "MOVE", transferAccounts())
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 || llm.got[0].SystemPromptOverride == "" {
		t.Fatalf("LLM calls = %d, got = %+v", llm.calls, llm.got)
	}
	if s.AccountID != "c1" || s.AccountName != "Checking" || s.Source != SourceLLM {
		t.Errorf("suggestion = %+v", s)
	}
}

func TestSuggestTransferJevLowBandGivesNoSuggestion(t *testing.T) {
	jev := &fakeDecider{fn: respondTransfer(t, "Savings", 0.3)}
	llm := &fakeLLM{}
	s, err := SuggestTransfer(context.Background(), newCascade(llm, jev, DefaultThresholds, ""), "MOVE", transferAccounts())
	if err != nil || s.AccountID != "" || llm.calls != 0 {
		t.Errorf("suggestion = %+v, err = %v, llm calls = %d", s, err, llm.calls)
	}
}

func TestSuggestTransferJevFailureFallsBackAndCoolsDown(t *testing.T) {
	jev := &fakeDecider{fn: func(decisionRequest) (decisionResponse, error) {
		return decisionResponse{}, &JevError{Status: 503, Message: "down"}
	}}
	llm := &fakeLLM{res: Result{RawResponse: `{"account":"Savings"}`}}
	c := newCascade(llm, jev, DefaultThresholds, "")
	for i := 0; i < 2; i++ {
		s, err := SuggestTransfer(context.Background(), c, "MOVE", transferAccounts())
		if err != nil || s.AccountID != "s1" || s.Source != SourceLLM {
			t.Fatalf("call %d: %+v %v", i, s, err)
		}
	}
	if jev.calls != 1 {
		t.Errorf("Jev calls = %d, want 1 (second call inside cooldown)", jev.calls)
	}
}

func TestSuggestTransferJevInvalidAnswerFallsBack(t *testing.T) {
	jev := &fakeDecider{fn: func(decisionRequest) (decisionResponse, error) {
		return decisionResponse{Answers: map[string]choiceAnswer{"account": {Choice: "zzz", Probabilities: map[string]float64{"zzz": 1}}}}, nil
	}}
	llm := &fakeLLM{res: Result{RawResponse: `{"account":"Savings"}`}}
	s, _ := SuggestTransfer(context.Background(), newCascade(llm, jev, DefaultThresholds, ""), "MOVE", transferAccounts())
	if llm.calls != 1 || s.AccountID != "s1" {
		t.Errorf("suggestion = %+v, llm calls = %d", s, llm.calls)
	}
}

func TestSuggestTransferLLMOnly(t *testing.T) {
	cases := []struct {
		raw     string
		wantID  string
		wantErr bool
	}{
		{"```json\n{\"account\": \"Savings\"}\n```", "s1", false},
		{`{"account": "Holiday fund"}`, "", false},
		{`not json`, "", true},
	}
	for _, tc := range cases {
		llm := &fakeLLM{res: Result{RawResponse: tc.raw}}
		s, err := SuggestTransfer(context.Background(), llm, "MOVE", transferAccounts())
		if (err != nil) != tc.wantErr || s.AccountID != tc.wantID {
			t.Errorf("raw %q: suggestion = %+v, err = %v", tc.raw, s, err)
		}
		if !strings.Contains(llm.got[0].SystemPromptOverride, "Savings") {
			t.Errorf("prompt must list accounts: %q", llm.got[0].SystemPromptOverride)
		}
	}
}

func TestSuggestTransferLLMError(t *testing.T) {
	llm := &fakeLLM{err: errors.New("boom")}
	if _, err := SuggestTransfer(context.Background(), llm, "MOVE", transferAccounts()); err == nil {
		t.Fatal("expected error")
	}
}

func TestSuggestTransferNoAccounts(t *testing.T) {
	jev := &fakeDecider{fn: func(decisionRequest) (decisionResponse, error) {
		t.Fatal("Jev must not be called")
		return decisionResponse{}, nil
	}}
	llm := &fakeLLM{}
	s, err := SuggestTransfer(context.Background(), newCascade(llm, jev, DefaultThresholds, ""), "MOVE", nil)
	if err != nil || s.AccountID != "" || llm.calls != 0 {
		t.Errorf("suggestion = %+v, err = %v, llm calls = %d", s, err, llm.calls)
	}
}
