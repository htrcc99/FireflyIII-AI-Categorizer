package classifier

import (
	"fmt"
	"strings"
)

// Thresholds split Jev's probability for its chosen option into three bands:
// p >= High is trusted, p < Low is sent to review, anything between goes to the LLM.
type Thresholds struct {
	High float64
	Low  float64
}

var DefaultThresholds = Thresholds{High: 0.85, Low: 0.50}

// newAccountKey is the destination option meaning "none of the listed accounts".
const newAccountKey = "new"

const (
	categoryInstructions = "Which of the account holder's spending categories does this bank withdrawal belong to? " +
		"Use the past classifications for the same payee as strong evidence."
	destinationInstructions = "Which existing payee (expense) account does this withdrawal go to? " +
		"Choose \"new\" only if the payee is clearly a specific business that is not in the list."
	newAccountCriterion = "None of the listed accounts; the payee is a specific business not in the list."
)

type jevTransaction struct {
	Destination string   `json:"destination"`
	Description string   `json:"description"`
	Amount      *float64 `json:"amount,omitempty"`
}

type jevHistoryEntry struct {
	Description string `json:"description"`
	Category    string `json:"category"`
}

type jevState struct {
	Transaction          jevTransaction    `json:"transaction"`
	History              []jevHistoryEntry `json:"history,omitempty"`
	AccountHolderContext string            `json:"account_holder_context,omitempty"`
}

// jevQuery is a Decisions API request plus the maps that translate its
// synthetic option keys back to categories and accounts.
type jevQuery struct {
	req        decisionRequest
	categories map[string]Category
	accounts   map[string]AccountCandidate
}

func buildJevQuery(req Request, customContext string) jevQuery {
	dest := req.DestinationName
	if isBlankName(dest) && req.Description != "" {
		dest = req.Description
	}
	state := jevState{
		Transaction:          jevTransaction{Destination: dest, Description: req.Description, Amount: req.Amount},
		AccountHolderContext: strings.TrimSpace(customContext),
	}
	for _, h := range req.History {
		if h.CategoryName == "" {
			continue
		}
		state.History = append(state.History, jevHistoryEntry{Description: h.Description, Category: h.CategoryName})
	}

	q := jevQuery{
		req:        decisionRequest{State: state, Questions: map[string]decisionQuestion{}},
		categories: make(map[string]Category),
		accounts:   make(map[string]AccountCandidate),
	}

	catCriteria := make(map[string]string, len(req.Categories))
	for i, c := range req.Categories {
		key := fmt.Sprintf("c%d", i)
		q.categories[key] = c
		if c.Notes != "" {
			catCriteria[key] = c.Name + " — " + c.Notes
		} else {
			catCriteria[key] = c.Name
		}
	}
	q.req.Questions["category"] = decisionQuestion{Type: "choice", Instructions: categoryInstructions, Criteria: catCriteria}

	if req.DestinationMatching && !req.CategoryOnly {
		cands := selectCandidates(req.ExpenseAccounts, GroupKey(req.DestinationName, req.Description), req.History, maxDestinationCandidates)
		if len(cands) > 0 {
			destCriteria := make(map[string]string, len(cands)+1)
			for i, a := range cands {
				key := fmt.Sprintf("a%d", i)
				q.accounts[key] = a
				destCriteria[key] = a.Name
			}
			destCriteria[newAccountKey] = newAccountCriterion
			q.req.Questions["destination"] = decisionQuestion{Type: "choice", Instructions: destinationInstructions, Criteria: destCriteria}
		}
	}
	return q
}

// jevDecision is Jev's answer translated back to domain values. dest is set
// only for a confident match to an existing account; destNew only for a
// confident "new account" answer.
type jevDecision struct {
	category  Category
	categoryP float64
	runnerUp  string
	runnerUpP float64
	dest      *AccountCandidate
	destP     float64
	destNew   bool
}

// answerScore returns the probability of the chosen option, falling back to
// the answer's confidence when probabilities are absent.
func answerScore(a choiceAnswer) (float64, bool) {
	if p, ok := a.Probabilities[a.Choice]; ok {
		return p, true
	}
	if a.Confidence != nil {
		return *a.Confidence, true
	}
	return 0, false
}

// interpret validates the category answer (errors mean "fall back to the LLM")
// and keeps the destination answer only when it is valid and confident.
func (q jevQuery) interpret(resp decisionResponse, th Thresholds) (jevDecision, error) {
	ans, ok := resp.Answers["category"]
	if !ok {
		return jevDecision{}, fmt.Errorf("jev: response has no category answer")
	}
	cat, ok := q.categories[ans.Choice]
	if !ok {
		return jevDecision{}, fmt.Errorf("jev: unknown category option %q", ans.Choice)
	}
	p, ok := answerScore(ans)
	if !ok {
		return jevDecision{}, fmt.Errorf("jev: category answer has no probability or confidence")
	}

	d := jevDecision{category: cat, categoryP: p}
	for key, prob := range ans.Probabilities {
		other, known := q.categories[key]
		if key == ans.Choice || !known {
			continue
		}
		if prob > d.runnerUpP || (prob == d.runnerUpP && d.runnerUp != "" && other.Name < d.runnerUp) {
			d.runnerUp, d.runnerUpP = other.Name, prob
		}
	}

	if _, asked := q.req.Questions["destination"]; !asked {
		return d, nil
	}
	dans, ok := resp.Answers["destination"]
	if !ok {
		return d, nil
	}
	dp, ok := answerScore(dans)
	if !ok || dp < th.High {
		return d, nil
	}
	if dans.Choice == newAccountKey {
		d.destNew, d.destP = true, dp
		return d, nil
	}
	if acct, ok := q.accounts[dans.Choice]; ok {
		d.dest, d.destP = &acct, dp
	}
	return d, nil
}
