package classifier

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

const transferInstructions = "This bank withdrawal is actually a transfer between the account holder's own accounts. " +
	"Which of the listed asset accounts received the money?"

// TransferSuggestion is a proposed destination asset account for converting a
// withdrawal into a transfer. A zero AccountID means no suggestion.
type TransferSuggestion struct {
	AccountID   string
	AccountName string
	Source      string // SourceJev or SourceLLM
	RawResponse string
}

// SuggestTransfer picks the most likely destination asset account for a
// transfer. A cascade asks Jev first: a confident answer is used directly, a
// hopeless one yields no suggestion, and anything else (middle band, failure,
// cooldown) falls back to the LLM prompt.
func SuggestTransfer(ctx context.Context, cl Classifier, description string, accounts []AccountCandidate) (TransferSuggestion, error) {
	if len(accounts) == 0 {
		return TransferSuggestion{}, nil
	}
	if c, ok := cl.(*CascadeClassifier); ok {
		if s, done := c.suggestTransfer(ctx, description, accounts); done {
			return s, nil
		}
	}
	return suggestTransferLLM(ctx, cl, description, accounts)
}

// suggestTransfer returns done=false when the LLM should decide instead.
func (c *CascadeClassifier) suggestTransfer(ctx context.Context, description string, accounts []AccountCandidate) (TransferSuggestion, bool) {
	if c.coolingDown() {
		return TransferSuggestion{}, false
	}

	keyed := make(map[string]AccountCandidate, len(accounts))
	criteria := make(map[string]string, len(accounts))
	for i, a := range accounts {
		key := fmt.Sprintf("a%d", i)
		keyed[key] = a
		criteria[key] = a.Name
	}
	req := decisionRequest{
		Model: c.jev.Model(),
		State: map[string]string{"transaction_description": description},
		Questions: map[string]decisionQuestion{
			"account": {Type: "choice", Instructions: transferInstructions, Criteria: criteria},
		},
	}

	resp, err := c.jev.Decide(ctx, req)
	if err != nil {
		c.startCooldown()
		slog.Warn("jev transfer suggestion failed, falling back to LLM", "error", err)
		return TransferSuggestion{}, false
	}
	ans := resp.Answers["account"]
	acct, known := keyed[ans.Choice]
	p, scored := answerScore(ans)
	if !known || !scored {
		slog.Warn("jev returned an invalid transfer answer, falling back to LLM", "choice", ans.Choice)
		return TransferSuggestion{}, false
	}

	switch {
	case p >= c.th.High:
		return TransferSuggestion{AccountID: acct.ID, AccountName: acct.Name, Source: SourceJev, RawResponse: resp.Raw}, true
	case p < c.th.Low:
		return TransferSuggestion{RawResponse: resp.Raw}, true
	}
	return TransferSuggestion{}, false
}

func suggestTransferLLM(ctx context.Context, cl Classifier, description string, accounts []AccountCandidate) (TransferSuggestion, error) {
	var acctList strings.Builder
	for _, a := range accounts {
		acctList.WriteString("  - " + a.Name + "\n")
	}

	result, err := cl.Classify(ctx, Request{
		SystemPromptOverride: fmt.Sprintf(
			"You select the best destination asset account for a transfer.\n"+
				"The transaction description is: %q\n\n"+
				"Available asset accounts:\n%s\n"+
				"Pick the most likely destination account.\n"+
				"Respond with ONLY valid JSON: {\"account\": \"<exact account name from the list>\"}",
			description, acctList.String(),
		),
	})
	if err != nil {
		return TransferSuggestion{}, err
	}

	var parsed struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal([]byte(stripMarkdown(result.RawResponse)), &parsed); err != nil {
		return TransferSuggestion{}, fmt.Errorf("parse transfer suggestion: %w (raw: %s)", err, result.RawResponse)
	}

	for _, a := range accounts {
		if strings.EqualFold(a.Name, parsed.Account) {
			return TransferSuggestion{AccountID: a.ID, AccountName: a.Name, Source: SourceLLM, RawResponse: result.RawResponse}, nil
		}
	}
	return TransferSuggestion{RawResponse: result.RawResponse}, nil
}
