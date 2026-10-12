package classifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

const (
	jevUnavailablePrefix = "Jev unavailable — "
	rawSeparator         = "\n\n--- LLM ---\n\n"
)

// CascadeClassifier asks Jev first and only calls the wrapped LLM for
// middle-band category decisions or to name a new payee account.
type CascadeClassifier struct {
	jev           decider
	llm           Classifier
	th            Thresholds
	customContext string
}

func newCascade(llm Classifier, jev decider, th Thresholds, customContext string) *CascadeClassifier {
	return &CascadeClassifier{jev: jev, llm: llm, th: th, customContext: customContext}
}

func (c *CascadeClassifier) Classify(ctx context.Context, req Request) (Result, error) {
	// Special-purpose prompts (transfer suggestions) and empty category lists
	// cannot be expressed as a Jev choice.
	if req.SystemPromptOverride != "" || len(req.Categories) == 0 {
		return c.llmOnly(ctx, req, "")
	}

	q := buildJevQuery(req, c.customContext)
	resp, err := c.jev.Decide(ctx, q.req)
	if err == nil {
		var d jevDecision
		if d, err = q.interpret(resp, c.th); err == nil {
			return c.decide(ctx, req, q, resp, d)
		}
	}

	var je *JevError
	if errors.As(err, &je) && je.ConfigProblem() {
		slog.Error("jev failed, falling back to LLM", "error", err)
	} else {
		slog.Warn("jev failed, falling back to LLM", "error", err)
	}
	return c.llmOnly(ctx, req, jevUnavailablePrefix)
}

func (c *CascadeClassifier) decide(ctx context.Context, req Request, q jevQuery, resp decisionResponse, d jevDecision) (Result, error) {
	jevPrompt := prettyJSON(q.req)
	jevResp := prettyRaw(resp.Raw)

	var res Result
	switch {
	case d.categoryP >= c.th.High:
		res = Result{Outcome: Classified, Category: d.category.Name}
	case d.categoryP < c.th.Low:
		res = Result{Outcome: NeedsReview}
	default:
		llmRes, err := c.llm.Classify(ctx, req)
		if err != nil {
			return Result{}, err
		}
		if llmRes.Destination == nil && d.dest != nil {
			llmRes.Destination = matchDestination(d.dest)
		}
		llmRes.Source = SourceJevLLM
		llmRes.RawPrompt = jevPrompt + rawSeparator + llmRes.RawPrompt
		llmRes.RawResponse = jevResp + rawSeparator + llmRes.RawResponse
		return llmRes, nil
	}

	res.Reason = jevReason(d, res.Outcome)
	res.Source = SourceJev
	res.RawPrompt = jevPrompt
	res.RawResponse = jevResp
	if d.dest != nil {
		res.Destination = matchDestination(d.dest)
	}

	if d.destNew {
		llmReq := req
		llmReq.DestinationMatching = true
		llmReq.CategoryOnly = false
		llmRes, err := c.llm.Classify(ctx, llmReq)
		if err != nil {
			slog.Warn("LLM call for new payee name failed; leaving destination unset", "error", err)
			return res, nil
		}
		if dr := llmRes.Destination; dr != nil && dr.Action == "CREATE" && dr.Confidence == "CLASSIFIED" {
			res.Destination = dr
		}
		res.Source = SourceJevLLM
		res.RawPrompt += rawSeparator + llmRes.RawPrompt
		res.RawResponse += rawSeparator + llmRes.RawResponse
	}
	return res, nil
}

func (c *CascadeClassifier) llmOnly(ctx context.Context, req Request, reasonPrefix string) (Result, error) {
	res, err := c.llm.Classify(ctx, req)
	if err != nil {
		return Result{}, err
	}
	res.Source = SourceLLM
	res.Reason = reasonPrefix + res.Reason
	return res, nil
}

func matchDestination(a *AccountCandidate) *DestinationResult {
	return &DestinationResult{Name: a.Name, Action: "MATCH", Confidence: "CLASSIFIED"}
}

func jevReason(d jevDecision, outcome Outcome) string {
	var s string
	if outcome == NeedsReview {
		s = fmt.Sprintf("Jev: low confidence — best guess %s (%.2f)", d.category.Name, d.categoryP)
	} else {
		s = fmt.Sprintf("Jev: %s (%.2f)", d.category.Name, d.categoryP)
		if d.runnerUp != "" {
			s += fmt.Sprintf(" · next: %s (%.2f)", d.runnerUp, d.runnerUpP)
		}
	}
	if d.dest != nil {
		s += fmt.Sprintf(" · payee: %s (%.2f)", d.dest.Name, d.destP)
	}
	return s
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}
	return string(b)
}

func prettyRaw(raw string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return raw
	}
	return buf.String()
}
