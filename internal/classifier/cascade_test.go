package classifier

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeDecider struct {
	fn    func(decisionRequest) (decisionResponse, error)
	calls int
}

func (f *fakeDecider) Model() string { return "fake-jev" }

func (f *fakeDecider) Decide(_ context.Context, r decisionRequest) (decisionResponse, error) {
	f.calls++
	return f.fn(r)
}

type fakeLLM struct {
	res   Result
	err   error
	calls int
	got   []Request
}

func (f *fakeLLM) Classify(_ context.Context, r Request) (Result, error) {
	f.calls++
	f.got = append(f.got, r)
	return f.res, f.err
}

// respond builds a fake Jev answer from human labels. destLabel "" omits the
// destination answer.
func respond(t *testing.T, catLabel string, catProbs map[string]float64, destLabel string, destProbs map[string]float64) func(decisionRequest) (decisionResponse, error) {
	return func(r decisionRequest) (decisionResponse, error) {
		answers := map[string]choiceAnswer{}
		toKeys := func(q decisionQuestion, probs map[string]float64) map[string]float64 {
			out := map[string]float64{}
			for label, p := range probs {
				out[keyFor(t, q, label)] = p
			}
			return out
		}
		cq := r.Questions["category"]
		answers["category"] = choiceAnswer{Type: "choice", Choice: keyFor(t, cq, catLabel), Probabilities: toKeys(cq, catProbs)}
		if destLabel != "" {
			dq := r.Questions["destination"]
			answers["destination"] = choiceAnswer{Type: "choice", Choice: keyFor(t, dq, destLabel), Probabilities: toKeys(dq, destProbs)}
		}
		return decisionResponse{Answers: answers, Raw: `{"answers":"…"}`}, nil
	}
}

func destReq() Request {
	r := baseRequest()
	r.DestinationMatching = true
	return r
}

func TestCascadeHighConfidenceSkipsLLM(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.92, "Dining": 0.05}, "", nil)}
	llm := &fakeLLM{}
	res, err := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 0 {
		t.Errorf("LLM calls = %d, want 0", llm.calls)
	}
	if res.Outcome != Classified || res.Category != "Groceries" || res.Source != SourceJev {
		t.Errorf("result = %+v", res)
	}
	if res.Reason != "Jev: Groceries (0.92) · next: Dining (0.05)" {
		t.Errorf("Reason = %q", res.Reason)
	}
	if !strings.Contains(res.RawPrompt, `"questions"`) || res.RawResponse == "" {
		t.Errorf("raw prompt/response not recorded: %q / %q", res.RawPrompt, res.RawResponse)
	}
}

func TestCascadeThresholdBoundaries(t *testing.T) {
	llm := &fakeLLM{res: Result{Outcome: Assumed, Category: "Dining"}}
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.85}, "", nil)}
	res, _ := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), baseRequest())
	if res.Outcome != Classified || llm.calls != 0 {
		t.Errorf("p == High must be CLASSIFIED without LLM: %+v calls=%d", res, llm.calls)
	}

	jev = &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.50}, "", nil)}
	res, _ = newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), baseRequest())
	if llm.calls != 1 || res.Source != SourceJevLLM {
		t.Errorf("p == Low must escalate: %+v calls=%d", res, llm.calls)
	}
}

func TestCascadeHighWithConfidentMatch(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.92}, "Mercadona", map[string]float64{"Mercadona": 0.97})}
	llm := &fakeLLM{}
	res, _ := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), destReq())
	if llm.calls != 0 {
		t.Errorf("LLM calls = %d, want 0", llm.calls)
	}
	if res.Destination == nil || res.Destination.Name != "Mercadona" || res.Destination.Action != "MATCH" || res.Destination.Confidence != "CLASSIFIED" {
		t.Errorf("Destination = %+v", res.Destination)
	}
	if !strings.HasSuffix(res.Reason, " · payee: Mercadona (0.97)") {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestCascadeHighWithUnsureMatch(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.92}, "Mercadona", map[string]float64{"Mercadona": 0.6})}
	llm := &fakeLLM{}
	res, _ := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), destReq())
	if llm.calls != 0 || res.Destination != nil || res.Outcome != Classified {
		t.Errorf("result = %+v, calls = %d", res, llm.calls)
	}
}

func TestCascadeHighWithNewAccount(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.92}, newAccountKey, map[string]float64{newAccountKey: 0.9})}
	llm := &fakeLLM{res: Result{
		Outcome: Classified, Category: "Dining", RawPrompt: "llm prompt", RawResponse: "llm response",
		Destination: &DestinationResult{Name: "Lidl", Action: "CREATE", Confidence: "CLASSIFIED"},
	}}
	req := destReq()
	req.CategoryOnly = false
	res, err := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 || !llm.got[0].DestinationMatching {
		t.Errorf("LLM calls = %d, got = %+v", llm.calls, llm.got)
	}
	if res.Category != "Groceries" || res.Outcome != Classified {
		t.Errorf("Jev category must be kept: %+v", res)
	}
	if res.Destination == nil || res.Destination.Name != "Lidl" || res.Destination.Action != "CREATE" {
		t.Errorf("Destination = %+v", res.Destination)
	}
	if res.Source != SourceJevLLM || !strings.Contains(res.RawPrompt, "--- LLM ---") || !strings.Contains(res.RawResponse, "llm response") {
		t.Errorf("source/raw = %q %q %q", res.Source, res.RawPrompt, res.RawResponse)
	}
}

func TestCascadeNewAccountRejectsUnsureLLM(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.92}, newAccountKey, map[string]float64{newAccountKey: 0.9})}
	for name, dr := range map[string]*DestinationResult{
		"assumed": {Name: "Lidl", Action: "CREATE", Confidence: "ASSUMED"},
		"match":   {Name: "Amazon", Action: "MATCH", Confidence: "CLASSIFIED"},
		"nil":     nil,
	} {
		llm := &fakeLLM{res: Result{Outcome: Classified, Destination: dr}}
		res, _ := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), destReq())
		if res.Destination != nil {
			t.Errorf("%s: Destination = %+v, want nil", name, res.Destination)
		}
	}
}

func TestCascadeNewAccountLLMErrorKeepsCategory(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.92}, newAccountKey, map[string]float64{newAccountKey: 0.9})}
	llm := &fakeLLM{err: errors.New("boom")}
	res, err := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), destReq())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if res.Category != "Groceries" || res.Destination != nil || res.Source != SourceJev {
		t.Errorf("result = %+v", res)
	}
}

func TestCascadeMiddleBandEscalates(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.6, "Dining": 0.4}, "", nil)}
	llm := &fakeLLM{res: Result{Outcome: Assumed, Category: "Dining", Reason: "llm reason", Assumption: "x", RawPrompt: "p", RawResponse: "r"}}
	res, err := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 {
		t.Errorf("LLM calls = %d, want 1", llm.calls)
	}
	if res.Outcome != Assumed || res.Category != "Dining" || res.Reason != "llm reason" || res.Assumption != "x" || res.Source != SourceJevLLM {
		t.Errorf("result = %+v", res)
	}
	if !strings.HasSuffix(res.RawPrompt, "--- LLM ---\n\np") || !strings.HasSuffix(res.RawResponse, "--- LLM ---\n\nr") {
		t.Errorf("raw = %q / %q", res.RawPrompt, res.RawResponse)
	}
}

func TestCascadeMiddleBandUsesJevDestinationWhenLLMHasNone(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.6}, "Mercadona", map[string]float64{"Mercadona": 0.97})}
	llm := &fakeLLM{res: Result{Outcome: Classified, Category: "Groceries"}}
	res, _ := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), destReq())
	if res.Destination == nil || res.Destination.Name != "Mercadona" || res.Destination.Action != "MATCH" {
		t.Errorf("Destination = %+v", res.Destination)
	}
}

func TestCascadeMiddleBandLLMErrorFails(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.6}, "", nil)}
	llm := &fakeLLM{err: errors.New("boom")}
	if _, err := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), baseRequest()); err == nil {
		t.Fatal("expected error")
	}
}

func TestCascadeLowBand(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.31}, "Mercadona", map[string]float64{"Mercadona": 0.9})}
	llm := &fakeLLM{}
	res, _ := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), destReq())
	if llm.calls != 0 {
		t.Errorf("LLM calls = %d, want 0", llm.calls)
	}
	if res.Outcome != NeedsReview || res.Category != "" || res.Source != SourceJev {
		t.Errorf("result = %+v", res)
	}
	if res.Reason != "Jev: low confidence — best guess Groceries (0.31) · payee: Mercadona (0.90)" {
		t.Errorf("Reason = %q", res.Reason)
	}
	if res.Destination == nil || res.Destination.Name != "Mercadona" {
		t.Errorf("Destination = %+v", res.Destination)
	}
}

func TestCascadeLowBandNewAccount(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.31}, newAccountKey, map[string]float64{newAccountKey: 0.95})}
	llm := &fakeLLM{res: Result{Destination: &DestinationResult{Name: "Lidl", Action: "CREATE", Confidence: "CLASSIFIED"}}}
	res, _ := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), destReq())
	if llm.calls != 1 || res.Outcome != NeedsReview || res.Destination == nil || res.Destination.Name != "Lidl" {
		t.Errorf("result = %+v calls = %d", res, llm.calls)
	}
}

func TestCascadeFallsBackOnJevFailure(t *testing.T) {
	cases := map[string]func(decisionRequest) (decisionResponse, error){
		"503": func(decisionRequest) (decisionResponse, error) {
			return decisionResponse{}, &JevError{Status: 503, Message: "down"}
		},
		"402": func(decisionRequest) (decisionResponse, error) {
			return decisionResponse{}, &JevError{Status: 402, Message: "credits"}
		},
		"network": func(decisionRequest) (decisionResponse, error) {
			return decisionResponse{}, errors.New("dial tcp: timeout")
		},
		"no category": func(decisionRequest) (decisionResponse, error) {
			return decisionResponse{Answers: map[string]choiceAnswer{}}, nil
		},
		"unknown key": func(decisionRequest) (decisionResponse, error) {
			return decisionResponse{Answers: map[string]choiceAnswer{"category": {Choice: "zzz", Probabilities: map[string]float64{"zzz": 1}}}}, nil
		},
		"no score": func(decisionRequest) (decisionResponse, error) {
			return decisionResponse{Answers: map[string]choiceAnswer{"category": {Choice: "c0"}}}, nil
		},
	}
	for name, fn := range cases {
		llm := &fakeLLM{res: Result{Outcome: Classified, Category: "Dining", Reason: "llm reason"}}
		res, err := newCascade(llm, &fakeDecider{fn: fn}, DefaultThresholds, "").Classify(context.Background(), baseRequest())
		if err != nil {
			t.Errorf("%s: err = %v", name, err)
			continue
		}
		if llm.calls != 1 || res.Source != SourceLLM || res.Category != "Dining" {
			t.Errorf("%s: result = %+v calls = %d", name, res, llm.calls)
		}
		if res.Reason != "Jev unavailable — llm reason" {
			t.Errorf("%s: Reason = %q", name, res.Reason)
		}
	}
}

func TestCascadeBypassesJev(t *testing.T) {
	for name, req := range map[string]Request{
		"override":      {SystemPromptOverride: "pick an account", Categories: baseRequest().Categories},
		"no categories": {DestinationName: "X", Description: "X"},
	} {
		jev := &fakeDecider{fn: func(decisionRequest) (decisionResponse, error) {
			t.Fatal("Jev must not be called")
			return decisionResponse{}, nil
		}}
		llm := &fakeLLM{res: Result{RawResponse: `{"account":"Savings"}`, Reason: "r"}}
		res, err := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), req)
		if err != nil || jev.calls != 0 || llm.calls != 1 || res.Source != SourceLLM || res.Reason != "r" {
			t.Errorf("%s: res = %+v err = %v jev = %d llm = %d", name, res, err, jev.calls, llm.calls)
		}
	}
}

func TestCascadeDestinationOnlyEscalatesWhenPayeeUnsure(t *testing.T) {
	llmDest := &DestinationResult{Name: "Amazon", Action: "MATCH", Confidence: "CLASSIFIED"}
	for _, catP := range []float64{0.95, 0.2} {
		jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": catP}, "Mercadona", map[string]float64{"Mercadona": 0.6})}
		llm := &fakeLLM{res: Result{Outcome: Classified, Category: "Groceries", Destination: llmDest}}
		req := destReq()
		req.DestinationOnly = true
		res, err := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if llm.calls != 1 || res.Destination != llmDest || res.Source != SourceJevLLM {
			t.Errorf("catP=%v: result = %+v calls = %d", catP, res, llm.calls)
		}
	}
}

func TestCascadeDestinationOnlyConfidentMatchSkipsLLM(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.95}, "Mercadona", map[string]float64{"Mercadona": 0.97})}
	llm := &fakeLLM{}
	req := destReq()
	req.DestinationOnly = true
	res, _ := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), req)
	if llm.calls != 0 || res.Destination == nil || res.Destination.Name != "Mercadona" {
		t.Errorf("result = %+v calls = %d", res, llm.calls)
	}
}

func TestCascadeMatchCarriesAccountID(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.92}, "Mercadona", map[string]float64{"Mercadona": 0.97})}
	res, _ := newCascade(&fakeLLM{}, jev, DefaultThresholds, "").Classify(context.Background(), destReq())
	if res.Destination == nil || res.Destination.ID != "10" {
		t.Errorf("Destination = %+v, want ID 10", res.Destination)
	}
}

func TestCascadeRawPromptShowsModel(t *testing.T) {
	jev := &fakeDecider{fn: respond(t, "Groceries", map[string]float64{"Groceries": 0.92}, "", nil)}
	res, _ := newCascade(&fakeLLM{}, jev, DefaultThresholds, "").Classify(context.Background(), baseRequest())
	if !strings.Contains(res.RawPrompt, `"model": "fake-jev"`) {
		t.Errorf("RawPrompt = %s", res.RawPrompt)
	}
}

func TestCascadeSkipsJevDuringCooldown(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	jev := &fakeDecider{fn: func(decisionRequest) (decisionResponse, error) {
		return decisionResponse{}, &JevError{Status: 503, Message: "down"}
	}}
	llm := &fakeLLM{res: Result{Outcome: Classified, Category: "Dining", Reason: "r"}}
	c := newCascade(llm, jev, DefaultThresholds, "")
	c.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		res, err := c.Classify(context.Background(), baseRequest())
		if err != nil || res.Source != SourceLLM || res.Reason != "Jev unavailable — r" {
			t.Fatalf("call %d: %+v %v", i, res, err)
		}
	}
	if jev.calls != 1 {
		t.Errorf("Jev calls during cooldown = %d, want 1", jev.calls)
	}

	now = now.Add(jevCooldown)
	jev.fn = respond(t, "Groceries", map[string]float64{"Groceries": 0.92}, "", nil)
	res, _ := c.Classify(context.Background(), baseRequest())
	if jev.calls != 2 || res.Source != SourceJev {
		t.Errorf("after cooldown: jev calls = %d, res = %+v", jev.calls, res)
	}
}

func TestCascadeInvalidAnswerDoesNotTripCooldown(t *testing.T) {
	jev := &fakeDecider{fn: func(decisionRequest) (decisionResponse, error) {
		return decisionResponse{Answers: map[string]choiceAnswer{}}, nil
	}}
	c := newCascade(&fakeLLM{}, jev, DefaultThresholds, "")
	c.Classify(context.Background(), baseRequest())
	c.Classify(context.Background(), baseRequest())
	if jev.calls != 2 {
		t.Errorf("Jev calls = %d, want 2 (a bad answer for one transaction must not disable Jev)", jev.calls)
	}
}
