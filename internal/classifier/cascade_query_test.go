package classifier

import (
	"encoding/json"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

// keyFor returns the synthetic key whose criterion is label (or starts with "label — ").
func keyFor(t *testing.T, q decisionQuestion, label string) string {
	t.Helper()
	if label == newAccountKey {
		return newAccountKey
	}
	for k, v := range q.Criteria {
		if v == label || strings.HasPrefix(v, label+" — ") {
			return k
		}
	}
	t.Fatalf("no criterion for %q in %v", label, q.Criteria)
	return ""
}

func baseRequest() Request {
	amount := 42.10
	return Request{
		Categories: []Category{
			{Name: "Groceries", Notes: "Supermarkets"},
			{Name: "Dining"},
			{Name: "Food & Drink — Café"},
		},
		DestinationName: "MERCADONA",
		Description:     "MERCADONA 4432",
		Amount:          &amount,
		History: []HistoricalEntry{
			{Description: "MERCADONA 1111", CategoryName: "Groceries", DestinationAccountID: "10"},
			{Description: "MERCADONA 2222", CategoryName: ""},
		},
		ExpenseAccounts: []AccountCandidate{{ID: "10", Name: "Mercadona"}, {ID: "11", Name: "Amazon"}},
	}
}

func TestBuildJevQueryCategoryQuestion(t *testing.T) {
	q := buildJevQuery(baseRequest(), "I live in Spain")
	cat, ok := q.req.Questions["category"]
	if !ok || cat.Type != "choice" || cat.Instructions == "" {
		t.Fatalf("category question = %+v", cat)
	}
	if len(cat.Criteria) != 3 {
		t.Fatalf("criteria = %v", cat.Criteria)
	}
	if cat.Criteria[keyFor(t, cat, "Groceries")] != "Groceries — Supermarkets" {
		t.Errorf("notes not included: %v", cat.Criteria)
	}
	k := keyFor(t, cat, "Food & Drink — Café")
	if !strings.HasPrefix(k, "c") || q.categories[k].Name != "Food & Drink — Café" {
		t.Errorf("synthetic key %q does not map back: %+v", k, q.categories[k])
	}
	if _, ok := q.req.Questions["destination"]; ok {
		t.Error("destination question sent without DestinationMatching")
	}
}

func TestBuildJevQueryState(t *testing.T) {
	q := buildJevQuery(baseRequest(), "I live in Spain")
	b, _ := json.Marshal(q.req.State)
	var state map[string]any
	_ = json.Unmarshal(b, &state)

	txn := state["transaction"].(map[string]any)
	if txn["destination"] != "MERCADONA" || txn["description"] != "MERCADONA 4432" || txn["amount"] != 42.1 {
		t.Errorf("transaction = %v", txn)
	}
	hist := state["history"].([]any)
	if len(hist) != 1 || hist[0].(map[string]any)["category"] != "Groceries" {
		t.Errorf("history = %v (entries without category must be skipped)", hist)
	}
	if state["account_holder_context"] != "I live in Spain" {
		t.Errorf("context = %v", state["account_holder_context"])
	}

	q = buildJevQuery(baseRequest(), "  ")
	b, _ = json.Marshal(q.req.State)
	if strings.Contains(string(b), "account_holder_context") {
		t.Errorf("blank context should be omitted: %s", b)
	}
}

func TestBuildJevQueryBlankDestinationUsesDescription(t *testing.T) {
	req := baseRequest()
	req.DestinationName = "(no name)"
	q := buildJevQuery(req, "")
	b, _ := json.Marshal(q.req.State)
	if !strings.Contains(string(b), `"destination":"MERCADONA 4432"`) {
		t.Errorf("state = %s", b)
	}
}

func TestBuildJevQueryDestinationQuestion(t *testing.T) {
	req := baseRequest()
	req.DestinationMatching = true
	q := buildJevQuery(req, "")
	dest, ok := q.req.Questions["destination"]
	if !ok {
		t.Fatal("destination question missing")
	}
	if _, ok := dest.Criteria[newAccountKey]; !ok {
		t.Errorf("missing %q option: %v", newAccountKey, dest.Criteria)
	}
	k := keyFor(t, dest, "Mercadona")
	if q.accounts[k].ID != "10" {
		t.Errorf("account key %q maps to %+v", k, q.accounts[k])
	}

	req.CategoryOnly = true
	if _, ok := buildJevQuery(req, "").req.Questions["destination"]; ok {
		t.Error("destination question sent with CategoryOnly")
	}

	req.CategoryOnly = false
	req.ExpenseAccounts = nil
	if _, ok := buildJevQuery(req, "").req.Questions["destination"]; ok {
		t.Error("destination question sent with no candidate accounts")
	}
}

func TestAnswerScore(t *testing.T) {
	if p, ok := answerScore(choiceAnswer{Choice: "c0", Probabilities: map[string]float64{"c0": 0.7}, Confidence: f64(0.4)}); !ok || p != 0.7 {
		t.Errorf("probability should win: %v %v", p, ok)
	}
	if p, ok := answerScore(choiceAnswer{Choice: "c0", Confidence: f64(0.4)}); !ok || p != 0.4 {
		t.Errorf("confidence fallback: %v %v", p, ok)
	}
	if _, ok := answerScore(choiceAnswer{Choice: "c0"}); ok {
		t.Error("no score should be invalid")
	}
}

func TestInterpret(t *testing.T) {
	req := baseRequest()
	req.DestinationMatching = true
	q := buildJevQuery(req, "")
	cat := q.req.Questions["category"]
	dest := q.req.Questions["destination"]
	groc, dining := keyFor(t, cat, "Groceries"), keyFor(t, cat, "Dining")
	merc := keyFor(t, dest, "Mercadona")

	resp := decisionResponse{Answers: map[string]choiceAnswer{
		"category":    {Choice: groc, Probabilities: map[string]float64{groc: 0.9, dining: 0.08}},
		"destination": {Choice: merc, Probabilities: map[string]float64{merc: 0.97}},
	}}
	d, err := q.interpret(resp, DefaultThresholds)
	if err != nil {
		t.Fatal(err)
	}
	if d.category.Name != "Groceries" || d.categoryP != 0.9 || d.runnerUp != "Dining" || d.runnerUpP != 0.08 {
		t.Errorf("category decision = %+v", d)
	}
	if d.dest == nil || d.dest.ID != "10" || d.destP != 0.97 || d.destNew {
		t.Errorf("destination decision = %+v", d)
	}

	resp.Answers["destination"] = choiceAnswer{Choice: newAccountKey, Probabilities: map[string]float64{newAccountKey: 0.9}}
	d, _ = q.interpret(resp, DefaultThresholds)
	if !d.destNew || d.dest != nil {
		t.Errorf("new account decision = %+v", d)
	}

	resp.Answers["destination"] = choiceAnswer{Choice: merc, Probabilities: map[string]float64{merc: 0.6}}
	d, _ = q.interpret(resp, DefaultThresholds)
	if d.dest != nil || d.destNew {
		t.Errorf("unconfident destination must be dropped: %+v", d)
	}

	resp.Answers["destination"] = choiceAnswer{Choice: "a999", Probabilities: map[string]float64{"a999": 1}}
	d, err = q.interpret(resp, DefaultThresholds)
	if err != nil || d.dest != nil || d.destNew {
		t.Errorf("unknown destination key must be ignored, got %+v %v", d, err)
	}
}

func TestInterpretInvalidCategory(t *testing.T) {
	q := buildJevQuery(baseRequest(), "")
	groc := keyFor(t, q.req.Questions["category"], "Groceries")
	cases := map[string]decisionResponse{
		"missing":     {Answers: map[string]choiceAnswer{}},
		"unknown key": {Answers: map[string]choiceAnswer{"category": {Choice: "a0", Probabilities: map[string]float64{"a0": 1}}}},
		"no score":    {Answers: map[string]choiceAnswer{"category": {Choice: groc}}},
	}
	for name, resp := range cases {
		if _, err := q.interpret(resp, DefaultThresholds); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
