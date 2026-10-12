# Jev Cascade Pre-Classifier Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add TypeSafe's Jev decision model (via OpenRouter) as a cheap first classification stage that only escalates uncertain transactions to the configured LLM.

**Architecture:** A new `CascadeClassifier` in `internal/classifier` implements the existing `Classifier` interface and wraps the configured LLM classifier. It sends one Jev request per transaction (category + optional destination as `choice` questions), applies two probability thresholds, and calls the LLM only for the middle band or to name a new payee account. The pipeline flow is unchanged; a new `Source` field travels from `Result` to `job.Job` so the UI can show who decided.

**Tech Stack:** Go 1.22 (stdlib `net/http`, `encoding/json`, `log/slog`; no new dependencies), chi router, jQuery/AdminLTE frontend.

**Spec:** `docs/superpowers/specs/2026-10-12-jev-cascade-design.md`

## Global Constraints

- No new Go module dependencies; Jev is called with `net/http`.
- Jev endpoint: `POST https://openrouter.ai/api/alpha/decisions`, header `Authorization: Bearer <OpenRouter key>`. Default model `typesafe/jev-1.13`. HTTP timeout 15 s.
- Default thresholds: `High = 0.85`, `Low = 0.50`. Bands: high `p ≥ High`, middle `Low ≤ p < High`, low `p < Low`.
- Score of a choice answer: `probabilities[choice]`, else `confidence`, else the answer is invalid.
- Destination candidates are capped at 50; accounts seen in history for the payee are always included.
- Synthetic option keys: categories `c<i>`, accounts `a<i>`, new-account option `new`.
- With Jev disabled (the default) behaviour must be byte-for-byte the current one.
- Any Jev failure degrades to LLM-only; it must never fail a job that would succeed today.
- `Source` values: `history`, `jev`, `jev+llm`, `llm`.
- Env vars: `JEV_ENABLED` (default `false`), `OPENROUTER_API_KEY`, `JEV_MODEL` (default `typesafe/jev-1.13`), `JEV_HIGH_THRESHOLD` (default `0.85`), `JEV_LOW_THRESHOLD` (default `0.50`).
- Thresholds must satisfy `0 ≤ low ≤ high ≤ 1`; `PUT /api/config` answers 400 otherwise and does not persist.
- `SuggestTransfer` is out of scope: requests with `SystemPromptOverride` bypass Jev.
- Run `gofmt -w` on every new or modified `.go` file before committing (the code blocks below are not column-aligned).
- Every commit message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

- **Firefly has zero categories** → Jev cannot be asked an empty choice; the cascade must go straight to the LLM (test in Task 5).
- **Probability exactly on a threshold** (`p == High`, `p == Low`) → `p == High` is CLASSIFIED, `p == Low` escalates to the LLM (test in Task 5).
- **Category names with spaces, accents or symbols** (e.g. `Food & Drink`, `Café`) → synthetic keys round-trip to the exact name (test in Task 4).
- **Settings saved with low > high or out of [0,1]** → 400 and nothing persisted (test in Task 6).
- **Hundreds of expense accounts, with the historical payee account dissimilar by name** → at most 50 candidates and the historical account still present (test in Task 3).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/classifier/classifier.go` (modify) | Add `Result.Source` and `Source*` constants. |
| `internal/job/job.go`, `internal/job/registry.go` (modify) | Store `Source` on jobs. |
| `internal/pipeline/pipeline.go` (modify) | Pass source to `SetFinished`. |
| `internal/classifier/jev.go` (create) | Jev HTTP client, wire types, `JevError`, `decider` interface. |
| `internal/classifier/candidates.go` (create) | Destination candidate preselection. |
| `internal/classifier/cascade_query.go` (create) | Build the Jev request and interpret the response (key mapping, scores). |
| `internal/classifier/cascade.go` (create) | `CascadeClassifier`: thresholds, LLM escalation, fallback, `Result` composition. |
| `internal/classifier/build.go` (modify) | Wrap the LLM with the cascade when enabled. |
| `internal/config/config.go`, `internal/config/store.go` (modify) | New Jev settings, env vars, threshold validation. |
| `internal/api/handler.go` (modify) | Expose/accept Jev settings, validate, pass to `Build`. |
| `cmd/server/main.go` (modify) | Pass Jev settings to `Build` in `--batch` mode. |
| `public/index.html`, `public/app.js` (modify) | Settings panel and source label in Jobs. |
| `README.md`, `public/docs.html` (modify) | User documentation. |

---

### Task 1: Decision source plumbing

**Files:**
- Modify: `internal/classifier/classifier.go`
- Modify: `internal/job/job.go`
- Modify: `internal/job/registry.go:52-64`
- Modify: `internal/pipeline/pipeline.go:189` and `internal/pipeline/pipeline.go:342-352`
- Test: `internal/job/registry_test.go` (create)

**Interfaces:**
- Produces: `classifier.Result.Source string`; constants `classifier.SourceHistory = "history"`, `SourceJev = "jev"`, `SourceJevLLM = "jev+llm"`, `SourceLLM = "llm"`; `job.Job.Source string` (JSON `source`); `(*job.Registry).SetFinished(id, outcome, category, reason, assumption, rawPrompt, rawResponse, destAccount, destAction, source string)`.

- [ ] **Step 1: Write the failing test**

Create `internal/job/registry_test.go`:

```go
package job

import "testing"

func TestSetFinishedStoresSource(t *testing.T) {
	r := NewRegistry()
	j := r.Create("42", "", "Mercadona", "MERCADONA 123", nil)

	r.SetFinished(j.ID, "CLASSIFIED", "Groceries", "reason", "", "p", "r", "Mercadona", "MATCH", "jev")

	got, ok := r.Get(j.ID)
	if !ok {
		t.Fatal("job not found")
	}
	if got.Source != "jev" {
		t.Errorf("Source = %q, want %q", got.Source, "jev")
	}
	if got.Status != StatusFinished {
		t.Errorf("Status = %q, want %q", got.Status, StatusFinished)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/job/ -run TestSetFinishedStoresSource -v`
Expected: FAIL to compile — `too many arguments in call to r.SetFinished` / `got.Source undefined`.

- [ ] **Step 3: Implement**

In `internal/job/job.go`, add after the `DestinationAction` field:

```go
	// Source records which stage decided the outcome: "history", "jev", "jev+llm" or "llm".
	Source string `json:"source,omitempty"`
```

In `internal/job/registry.go`, replace `SetFinished` with:

```go
func (r *Registry) SetFinished(id, outcome, category, reason, assumption, rawPrompt, rawResponse, destAccount, destAction, source string) {
	r.update(id, func(j *Job) {
		j.Status = StatusFinished
		j.Outcome = outcome
		j.Category = category
		j.Reason = reason
		j.Assumption = assumption
		j.RawPrompt = rawPrompt
		j.RawResponse = rawResponse
		j.DestinationAccount = destAccount
		j.DestinationAction = destAction
		j.Source = source
	})
}
```

In `internal/classifier/classifier.go`, add after the `Outcome` constants:

```go
// Decision sources, recorded on Result.Source and shown in the Jobs UI.
const (
	SourceHistory = "history"
	SourceJev     = "jev"
	SourceJevLLM  = "jev+llm"
	SourceLLM     = "llm"
)
```

and add to `Result` after `Destination`:

```go
	// Source identifies which stage produced this result. Empty means "llm".
	Source string
```

In `internal/pipeline/pipeline.go`, change the history-match call (line 189) to:

```go
		p.registry.SetFinished(j.ID, string(classifier.Classified), historyMatchCat, reason, "", "", "", destName, destAction, classifier.SourceHistory)
```

and the final call (lines 342-352) to:

```go
	source := result.Source
	if source == "" {
		source = classifier.SourceLLM
	}
	p.registry.SetFinished(
		j.ID,
		finishedOutcome,
		finishedCategory,
		finishedReason,
		result.Assumption,
		result.RawPrompt,
		result.RawResponse,
		destAccount,
		destAction,
		source,
	)
```

- [ ] **Step 4: Run tests and build**

Run: `go test ./internal/job/ -v && go build ./... && go vet ./...`
Expected: PASS, build and vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/job internal/classifier/classifier.go internal/pipeline/pipeline.go
git commit -m "feat: record decision source on classification results and jobs

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Jev HTTP client

**Files:**
- Create: `internal/classifier/jev.go`
- Test: `internal/classifier/jev_test.go`

**Interfaces:**
- Produces:
  - `const DefaultJevModel = "typesafe/jev-1.13"`
  - `type decisionQuestion struct { Type, Instructions string; Criteria map[string]string }` (JSON `type`, `instructions`, `criteria`)
  - `type decisionRequest struct { Model string; State any; Questions map[string]decisionQuestion }`
  - `type choiceAnswer struct { Type, Choice string; Confidence *float64; Probabilities map[string]float64 }`
  - `type decisionResponse struct { ID, Model string; Answers map[string]choiceAnswer; Raw string }` (`Raw` = response body, not JSON-decoded)
  - `type decider interface { Decide(ctx context.Context, req decisionRequest) (decisionResponse, error) }`
  - `type JevError struct { Status int; Message string }` with `Error() string` and `ConfigProblem() bool` (true for 400, 401, 402, 403, 404, 413)
  - `func NewJevClient(apiKey, model string) *JevClient` (empty model → `DefaultJevModel`); `(*JevClient).Decide` sets `req.Model` itself.

- [ ] **Step 1: Write the failing tests**

Create `internal/classifier/jev_test.go`:

```go
package classifier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestJevClient(t *testing.T, handler http.HandlerFunc) *JevClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewJevClient("test-key", "")
	c.endpoint = srv.URL
	return c
}

func TestJevClientSendsRequest(t *testing.T) {
	var gotAuth, gotMethod, gotCT string
	var gotBody map[string]any
	c := newTestJevClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"model":"typesafe/jev-1.13-20260917","answers":{"category":{"type":"choice","choice":"c0","probabilities":{"c0":1}}},"usage":{"input_tokens":1,"output_tokens":1}}`))
	})

	_, err := c.Decide(context.Background(), decisionRequest{
		State: map[string]string{"k": "v"},
		Questions: map[string]decisionQuestion{
			"category": {Type: "choice", Instructions: "Which?", Criteria: map[string]string{"c0": "Groceries"}},
		},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody["model"] != DefaultJevModel {
		t.Errorf("model = %v, want %s", gotBody["model"], DefaultJevModel)
	}
	q := gotBody["questions"].(map[string]any)["category"].(map[string]any)
	if q["type"] != "choice" || q["criteria"].(map[string]any)["c0"] != "Groceries" {
		t.Errorf("question = %v", q)
	}
}

func TestJevClientUsesConfiguredModel(t *testing.T) {
	var gotBody map[string]any
	c := newTestJevClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"model":"x","answers":{}}`))
	})
	c.model = "typesafe/jev-2"
	if _, err := c.Decide(context.Background(), decisionRequest{}); err != nil {
		t.Fatal(err)
	}
	if gotBody["model"] != "typesafe/jev-2" {
		t.Errorf("model = %v", gotBody["model"])
	}
}

func TestJevClientParsesResponse(t *testing.T) {
	body := `{"id":"gen-1","model":"typesafe/jev-1.13-20260917","answers":{
		"category":{"type":"choice","choice":"c1","confidence":0.67,"probabilities":{"c0":0.22,"c1":0.78}},
		"destination":{"type":"choice","choice":"new"}},
		"usage":{"input_tokens":476,"output_tokens":70,"cost":0.00002}}`
	c := newTestJevClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })

	resp, err := c.Decide(context.Background(), decisionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cat := resp.Answers["category"]
	if cat.Choice != "c1" || cat.Probabilities["c1"] != 0.78 || cat.Confidence == nil || *cat.Confidence != 0.67 {
		t.Errorf("category answer = %+v", cat)
	}
	dest := resp.Answers["destination"]
	if dest.Choice != "new" || dest.Confidence != nil || dest.Probabilities != nil {
		t.Errorf("destination answer with optional fields absent = %+v", dest)
	}
	if resp.Raw != body {
		t.Errorf("Raw not preserved")
	}
}

func TestJevClientErrors(t *testing.T) {
	cases := []struct {
		status     int
		body       string
		wantMsg    string
		wantConfig bool
	}{
		{402, `{"error":{"code":402,"message":"Insufficient credits"}}`, "Insufficient credits", true},
		{401, `{"error":{"code":401,"message":"No auth"}}`, "No auth", true},
		{413, `{"error":{"code":413,"message":"Too large"}}`, "Too large", true},
		{429, `{"error":{"code":429,"message":"Rate limited"}}`, "Rate limited", false},
		{503, `upstream down`, "upstream down", false},
		{529, `{"error":{"code":529,"message":"Overloaded"}}`, "Overloaded", false},
	}
	for _, tc := range cases {
		c := newTestJevClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := c.Decide(context.Background(), decisionRequest{})
		var je *JevError
		if !errors.As(err, &je) {
			t.Fatalf("status %d: err = %v, want *JevError", tc.status, err)
		}
		if je.Status != tc.status || je.Message != tc.wantMsg {
			t.Errorf("status %d: got %+v", tc.status, je)
		}
		if je.ConfigProblem() != tc.wantConfig {
			t.Errorf("status %d: ConfigProblem = %v, want %v", tc.status, je.ConfigProblem(), tc.wantConfig)
		}
	}
}

func TestJevClientInvalidJSON(t *testing.T) {
	c := newTestJevClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`not json`)) })
	if _, err := c.Decide(context.Background(), decisionRequest{}); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestJevClientTimeout(t *testing.T) {
	c := newTestJevClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	})
	c.http.Timeout = 20 * time.Millisecond
	if _, err := c.Decide(context.Background(), decisionRequest{}); err == nil {
		t.Fatal("expected timeout error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/classifier/ -run TestJevClient -v`
Expected: FAIL to compile — `undefined: JevClient`, `undefined: decisionRequest`.

- [ ] **Step 3: Implement**

Create `internal/classifier/jev.go`:

```go
package classifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultJevModel is the TypeSafe decision model served by OpenRouter.
const DefaultJevModel = "typesafe/jev-1.13"

const (
	jevEndpoint = "https://openrouter.ai/api/alpha/decisions"
	jevTimeout  = 15 * time.Second
)

type decisionQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type decisionRequest struct {
	Model     string                      `json:"model"`
	State     any                         `json:"state"`
	Questions map[string]decisionQuestion `json:"questions"`
}

// choiceAnswer is a Jev "choice" answer. Confidence and Probabilities are
// optional in the API schema and may be absent.
type choiceAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type decisionResponse struct {
	ID      string                  `json:"id,omitempty"`
	Model   string                  `json:"model"`
	Answers map[string]choiceAnswer `json:"answers"`

	// Raw is the undecoded response body, kept for the job detail view.
	Raw string `json:"-"`
}

// decider is the seam between the cascade and the Jev API, replaced by a fake in tests.
type decider interface {
	Decide(ctx context.Context, req decisionRequest) (decisionResponse, error)
}

// JevError is a non-2xx answer from the Decisions API.
type JevError struct {
	Status  int
	Message string
}

func (e *JevError) Error() string {
	return fmt.Sprintf("jev: HTTP %d: %s", e.Status, e.Message)
}

// ConfigProblem reports errors caused by credentials, credit or a malformed
// request rather than a transient outage; they are logged at error level.
func (e *JevError) ConfigProblem() bool {
	switch e.Status {
	case 400, 401, 402, 403, 404, 413:
		return true
	}
	return false
}

// JevClient calls OpenRouter's Decisions API.
type JevClient struct {
	http     *http.Client
	endpoint string
	apiKey   string
	model    string
}

func NewJevClient(apiKey, model string) *JevClient {
	if model == "" {
		model = DefaultJevModel
	}
	return &JevClient{
		http:     &http.Client{Timeout: jevTimeout},
		endpoint: jevEndpoint,
		apiKey:   apiKey,
		model:    model,
	}
}

func (c *JevClient) Decide(ctx context.Context, req decisionRequest) (decisionResponse, error) {
	req.Model = c.model
	body, err := json.Marshal(req)
	if err != nil {
		return decisionResponse{}, fmt.Errorf("jev: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return decisionResponse{}, fmt.Errorf("jev: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return decisionResponse{}, fmt.Errorf("jev: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return decisionResponse{}, fmt.Errorf("jev: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return decisionResponse{}, &JevError{Status: resp.StatusCode, Message: msg}
	}

	var out decisionResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return decisionResponse{}, fmt.Errorf("jev: decode response: %w", err)
	}
	out.Raw = string(raw)
	return out, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/classifier/ -run TestJevClient -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/jev.go internal/classifier/jev_test.go
git commit -m "feat: add OpenRouter Decisions API client for Jev

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Destination candidate preselection

**Files:**
- Create: `internal/classifier/candidates.go`
- Test: `internal/classifier/candidates_test.go`

**Interfaces:**
- Consumes: `AccountCandidate`, `HistoricalEntry`, `isBlankName` (existing, `classifier.go`).
- Produces: `const maxDestinationCandidates = 50`; `func selectCandidates(accounts []AccountCandidate, groupKey string, history []HistoricalEntry, limit int) []AccountCandidate`.

- [ ] **Step 1: Write the failing tests**

Create `internal/classifier/candidates_test.go`:

```go
package classifier

import (
	"fmt"
	"testing"
)

func names(accts []AccountCandidate) []string {
	out := make([]string, len(accts))
	for i, a := range accts {
		out[i] = a.Name
	}
	return out
}

func TestSelectCandidatesRanksBySimilarity(t *testing.T) {
	accts := []AccountCandidate{
		{ID: "1", Name: "Amazon"},
		{ID: "2", Name: "Lidl"},
		{ID: "3", Name: "Mercadona Online"},
		{ID: "4", Name: "Mercadona"},
	}
	got := names(selectCandidates(accts, "MERCADONA 4432 VALENCIA", nil, 2))
	if len(got) != 2 || got[0] != "Mercadona" || got[1] != "Mercadona Online" {
		t.Errorf("got %v, want [Mercadona Mercadona Online]", got)
	}
}

func TestSelectCandidatesCapsAndPinsHistory(t *testing.T) {
	var accts []AccountCandidate
	for i := 0; i < 500; i++ {
		accts = append(accts, AccountCandidate{ID: fmt.Sprint(i), Name: fmt.Sprintf("Shop %d", i)})
	}
	accts = append(accts, AccountCandidate{ID: "pinned", Name: "Corner Grocer"})
	history := []HistoricalEntry{{DestinationAccountID: "pinned"}}

	got := selectCandidates(accts, "SHOP PAYMENT", history, maxDestinationCandidates)
	if len(got) != maxDestinationCandidates {
		t.Fatalf("len = %d, want %d", len(got), maxDestinationCandidates)
	}
	if got[0].ID != "pinned" {
		t.Errorf("history account not first: %v", got[0])
	}
}

func TestSelectCandidatesDropsBlankNames(t *testing.T) {
	accts := []AccountCandidate{{ID: "1", Name: "(no name)"}, {ID: "2", Name: ""}, {ID: "3", Name: "Netflix"}}
	got := names(selectCandidates(accts, "NETFLIX.COM", nil, 50))
	if len(got) != 1 || got[0] != "Netflix" {
		t.Errorf("got %v, want [Netflix]", got)
	}
}

func TestSelectCandidatesKeepsOrderWithoutSignal(t *testing.T) {
	accts := []AccountCandidate{{ID: "1", Name: "B"}, {ID: "2", Name: "A"}}
	got := names(selectCandidates(accts, "", nil, 50))
	if len(got) != 2 || got[0] != "B" || got[1] != "A" {
		t.Errorf("got %v, want original order", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/classifier/ -run TestSelectCandidates -v`
Expected: FAIL to compile — `undefined: selectCandidates`.

- [ ] **Step 3: Implement**

Create `internal/classifier/candidates.go`:

```go
package classifier

import (
	"sort"
	"strings"
	"unicode"
)

// maxDestinationCandidates bounds how many expense accounts are offered to Jev
// so the request stays well inside its 32k-token context.
const maxDestinationCandidates = 50

// selectCandidates returns up to limit expense accounts to offer as destination
// options. Accounts already used for this payee in history come first; the rest
// are ranked by word overlap with groupKey, keeping the original order on ties.
func selectCandidates(accounts []AccountCandidate, groupKey string, history []HistoricalEntry, limit int) []AccountCandidate {
	pinned := make(map[string]bool)
	for _, h := range history {
		if h.DestinationAccountID != "" {
			pinned[h.DestinationAccountID] = true
		}
	}

	keyTokens := tokenize(groupKey)
	keyLower := strings.ToLower(strings.TrimSpace(groupKey))

	type scored struct {
		acct  AccountCandidate
		score float64
	}
	var first, rest []scored
	for _, a := range accounts {
		if isBlankName(a.Name) {
			continue
		}
		s := scored{acct: a, score: similarity(keyTokens, keyLower, a.Name)}
		if pinned[a.ID] {
			first = append(first, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].score > rest[j].score })

	out := make([]AccountCandidate, 0, limit)
	for _, s := range append(first, rest...) {
		if len(out) == limit {
			break
		}
		out = append(out, s.acct)
	}
	return out
}

// similarity is the share of the account name's words found in the key, plus a
// bonus when one string contains the other.
func similarity(keyTokens map[string]bool, keyLower, name string) float64 {
	nameTokens := tokenize(name)
	if len(nameTokens) == 0 || len(keyTokens) == 0 {
		return 0
	}
	shared := 0
	for t := range nameTokens {
		if keyTokens[t] {
			shared++
		}
	}
	score := float64(shared) / float64(len(nameTokens))
	nameLower := strings.ToLower(strings.TrimSpace(name))
	if strings.Contains(keyLower, nameLower) || strings.Contains(nameLower, keyLower) {
		score += 0.5
	}
	return score
}

func tokenize(s string) map[string]bool {
	out := make(map[string]bool)
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		out[f] = true
	}
	return out
}
```

Ranking check for the first test: key tokens `{mercadona, 4432, valencia}`. "Mercadona" → 1/1 + 0.5 (contained) = 1.5; "Mercadona Online" → 1/2 = 0.5; others 0.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/classifier/ -run TestSelectCandidates -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/candidates.go internal/classifier/candidates_test.go
git commit -m "feat: preselect destination account candidates for Jev

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Jev query building and answer interpretation

**Files:**
- Create: `internal/classifier/cascade_query.go`
- Test: `internal/classifier/cascade_query_test.go`

**Interfaces:**
- Consumes: `decisionRequest`, `decisionQuestion`, `choiceAnswer`, `decisionResponse` (Task 2); `selectCandidates`, `maxDestinationCandidates` (Task 3); `Request`, `Category`, `AccountCandidate`, `GroupKey`, `isBlankName` (existing).
- Produces:
  - `type Thresholds struct { High, Low float64 }`; `var DefaultThresholds = Thresholds{High: 0.85, Low: 0.50}`
  - `const newAccountKey = "new"`
  - `type jevQuery struct { req decisionRequest; categories map[string]Category; accounts map[string]AccountCandidate }`
  - `func buildJevQuery(req Request, customContext string) jevQuery`
  - `type jevDecision struct { category Category; categoryP float64; runnerUp string; runnerUpP float64; dest *AccountCandidate; destP float64; destNew bool }`
  - `func (q jevQuery) interpret(resp decisionResponse, th Thresholds) (jevDecision, error)`
  - `func answerScore(a choiceAnswer) (float64, bool)`

- [ ] **Step 1: Write the failing tests**

Create `internal/classifier/cascade_query_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/classifier/ -run 'TestBuildJevQuery|TestAnswerScore|TestInterpret' -v`
Expected: FAIL to compile — `undefined: buildJevQuery`.

- [ ] **Step 3: Implement**

Create `internal/classifier/cascade_query.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/classifier/ -run 'TestBuildJevQuery|TestAnswerScore|TestInterpret' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/cascade_query.go internal/classifier/cascade_query_test.go
git commit -m "feat: build Jev decision requests and interpret answers

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: CascadeClassifier decision logic

**Files:**
- Create: `internal/classifier/cascade.go`
- Test: `internal/classifier/cascade_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–4; `Classifier`, `Result`, `DestinationResult`, `Classified`, `NeedsReview` (existing).
- Produces: `type CascadeClassifier struct` implementing `Classifier`; `func newCascade(llm Classifier, jev decider, th Thresholds, customContext string) *CascadeClassifier`.

- [ ] **Step 1: Write the failing tests**

Create `internal/classifier/cascade_test.go`:

```go
package classifier

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeDecider struct {
	fn    func(decisionRequest) (decisionResponse, error)
	calls int
}

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
		"503":           func(decisionRequest) (decisionResponse, error) { return decisionResponse{}, &JevError{Status: 503, Message: "down"} },
		"402":           func(decisionRequest) (decisionResponse, error) { return decisionResponse{}, &JevError{Status: 402, Message: "credits"} },
		"network":       func(decisionRequest) (decisionResponse, error) { return decisionResponse{}, errors.New("dial tcp: timeout") },
		"no category":   func(decisionRequest) (decisionResponse, error) { return decisionResponse{Answers: map[string]choiceAnswer{}}, nil },
		"unknown key":   func(decisionRequest) (decisionResponse, error) { return decisionResponse{Answers: map[string]choiceAnswer{"category": {Choice: "zzz", Probabilities: map[string]float64{"zzz": 1}}}}, nil },
		"no score":      func(decisionRequest) (decisionResponse, error) { return decisionResponse{Answers: map[string]choiceAnswer{"category": {Choice: "c0"}}}, nil },
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
		jev := &fakeDecider{fn: func(decisionRequest) (decisionResponse, error) { t.Fatal("Jev must not be called"); return decisionResponse{}, nil }}
		llm := &fakeLLM{res: Result{RawResponse: `{"account":"Savings"}`, Reason: "r"}}
		res, err := newCascade(llm, jev, DefaultThresholds, "").Classify(context.Background(), req)
		if err != nil || jev.calls != 0 || llm.calls != 1 || res.Source != SourceLLM || res.Reason != "r" {
			t.Errorf("%s: res = %+v err = %v jev = %d llm = %d", name, res, err, jev.calls, llm.calls)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/classifier/ -run TestCascade -v`
Expected: FAIL to compile — `undefined: newCascade`.

- [ ] **Step 3: Implement**

Create `internal/classifier/cascade.go`:

```go
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
```

- [ ] **Step 4: Run all classifier tests**

Run: `go test ./internal/classifier/ -v`
Expected: PASS (all tests from Tasks 2–5).

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/cascade.go internal/classifier/cascade_test.go
git commit -m "feat: add Jev-first cascade classifier with LLM escalation

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Configuration, wiring and settings API

**Files:**
- Modify: `internal/config/config.go` (struct, `Load`, `ApplyStored`, new helpers)
- Modify: `internal/config/store.go` (`StoredConfig`)
- Modify: `internal/classifier/build.go`
- Modify: `internal/api/handler.go` (`configResponse`, `getConfig`, `configUpdateRequest`, `putConfig`, `reloadClients`, `mergeConfigUpdate`)
- Modify: `cmd/server/main.go` (`runBatch`)
- Test: `internal/config/config_test.go` (create), `internal/classifier/build_test.go` (create), `internal/api/handler_test.go` (create)

**Interfaces:**
- Consumes: `newCascade`, `NewJevClient`, `Thresholds`, `DefaultThresholds`, `DefaultJevModel`, `CascadeClassifier` (Tasks 2, 4, 5).
- Produces:
  - `config.Config` fields `JevEnabled bool`, `OpenRouterKey string`, `JevModel string`, `JevHighThreshold float64`, `JevLowThreshold float64`
  - `config.StoredConfig` fields `JevEnabled *bool` (`jev_enabled`), `OpenRouterKey string` (`openrouter_api_key`), `JevModel string` (`jev_model`), `JevHighThreshold *float64` (`jev_high_threshold`), `JevLowThreshold *float64` (`jev_low_threshold`)
  - `func (c *Config) ValidateJevThresholds() error`
  - `classifier.BuildParams` fields `JevEnabled bool`, `OpenRouterKey string`, `JevModel string`, `JevThresholds Thresholds`
  - JSON in `GET /api/config`: `jev_enabled`, `openrouter_key_set`, `jev_model`, `jev_high_threshold`, `jev_low_threshold`; accepted by `PUT /api/config`: `jev_enabled`, `openrouter_api_key`, `jev_model`, `jev_high_threshold`, `jev_low_threshold`.

- [ ] **Step 1: Write the failing tests**

Create `internal/config/config_test.go`:

```go
package config

import "testing"

func TestLoadJevDefaultsAndEnv(t *testing.T) {
	t.Setenv("CONFIG_FILE", t.TempDir()+"/config.json")
	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JevEnabled || cfg.JevModel != "typesafe/jev-1.13" || cfg.JevHighThreshold != 0.85 || cfg.JevLowThreshold != 0.50 {
		t.Errorf("defaults = %+v", cfg)
	}

	t.Setenv("JEV_ENABLED", "true")
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	t.Setenv("JEV_HIGH_THRESHOLD", "0.9")
	t.Setenv("JEV_LOW_THRESHOLD", "not-a-number")
	cfg, _, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.JevEnabled || cfg.OpenRouterKey != "or-key" || cfg.JevHighThreshold != 0.9 || cfg.JevLowThreshold != 0.50 {
		t.Errorf("env = %+v", cfg)
	}
}

func TestApplyStoredJev(t *testing.T) {
	on, high, low := true, 0.95, 0.4
	cfg := &Config{JevModel: "typesafe/jev-1.13", JevHighThreshold: 0.85, JevLowThreshold: 0.5}
	ApplyStored(cfg, StoredConfig{
		JevEnabled: &on, OpenRouterKey: "k", JevModel: "typesafe/jev-2",
		JevHighThreshold: &high, JevLowThreshold: &low,
	})
	if !cfg.JevEnabled || cfg.OpenRouterKey != "k" || cfg.JevModel != "typesafe/jev-2" || cfg.JevHighThreshold != 0.95 || cfg.JevLowThreshold != 0.4 {
		t.Errorf("cfg = %+v", cfg)
	}

	cfg = &Config{JevModel: "typesafe/jev-1.13", OpenRouterKey: "env"}
	ApplyStored(cfg, StoredConfig{})
	if cfg.JevModel != "typesafe/jev-1.13" || cfg.OpenRouterKey != "env" {
		t.Errorf("empty stored values must not override: %+v", cfg)
	}
}

func TestValidateJevThresholds(t *testing.T) {
	cases := []struct {
		high, low float64
		ok        bool
	}{
		{0.85, 0.5, true},
		{0.5, 0.5, true},
		{1, 0, true},
		{0.4, 0.5, false},
		{1.1, 0.5, false},
		{0.8, -0.1, false},
	}
	for _, tc := range cases {
		err := (&Config{JevHighThreshold: tc.high, JevLowThreshold: tc.low}).ValidateJevThresholds()
		if (err == nil) != tc.ok {
			t.Errorf("high=%v low=%v: err = %v", tc.high, tc.low, err)
		}
	}
}
```

Create `internal/classifier/build_test.go`:

```go
package classifier

import "testing"

func TestBuildJev(t *testing.T) {
	base := BuildParams{Provider: "openai", OpenAIKey: "sk", OpenAIModel: "gpt-4o-mini", JevThresholds: DefaultThresholds}

	cl, err := Build(base)
	if _, ok := cl.(*OpenAIClassifier); err != nil || !ok {
		t.Errorf("disabled: got %T, %v", cl, err)
	}

	p := base
	p.JevEnabled = true
	cl, err = Build(p)
	if _, ok := cl.(*OpenAIClassifier); err != nil || !ok {
		t.Errorf("enabled without key: got %T, %v", cl, err)
	}

	p.OpenRouterKey = "or-key"
	cl, err = Build(p)
	cc, ok := cl.(*CascadeClassifier)
	if err != nil || !ok {
		t.Fatalf("enabled with key: got %T, %v", cl, err)
	}
	if _, ok := cc.llm.(*OpenAIClassifier); !ok {
		t.Errorf("cascade must wrap the configured LLM, got %T", cc.llm)
	}
	if jc, ok := cc.jev.(*JevClient); !ok || jc.model != DefaultJevModel {
		t.Errorf("jev client = %#v", cc.jev)
	}
}
```

Create `internal/api/handler_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/config"
)

func TestMergeConfigUpdateJev(t *testing.T) {
	on, high, low := true, 0.9, 0.3
	key, model := "or-key", "typesafe/jev-2"
	got := mergeConfigUpdate(config.StoredConfig{OpenRouterKey: "old"}, configUpdateRequest{
		JevEnabled: &on, OpenRouterKey: &key, JevModel: &model, JevHighThreshold: &high, JevLowThreshold: &low,
	})
	if got.JevEnabled == nil || !*got.JevEnabled || got.OpenRouterKey != "or-key" || got.JevModel != model ||
		got.JevHighThreshold == nil || *got.JevHighThreshold != 0.9 || got.JevLowThreshold == nil || *got.JevLowThreshold != 0.3 {
		t.Errorf("merged = %+v", got)
	}

	empty := ""
	got = mergeConfigUpdate(config.StoredConfig{OpenRouterKey: "old"}, configUpdateRequest{OpenRouterKey: &empty})
	if got.OpenRouterKey != "old" {
		t.Errorf("empty key must keep existing, got %q", got.OpenRouterKey)
	}
}

func TestPutConfigRejectsInvalidThresholds(t *testing.T) {
	store, err := config.NewStore(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		baseCfg: &config.Config{JevHighThreshold: 0.85, JevLowThreshold: 0.5},
		store:   store,
	}

	for _, body := range []string{
		`{"jev_high_threshold":0.4,"jev_low_threshold":0.6}`,
		`{"jev_high_threshold":1.5}`,
		`{"jev_low_threshold":-0.1}`,
	} {
		rec := httptest.NewRecorder()
		h.putConfig(rec, httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
		if got := store.Get(); got.JevHighThreshold != nil || got.JevLowThreshold != nil {
			t.Errorf("%s: invalid thresholds were persisted: %+v", body, got)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ ./internal/classifier/ ./internal/api/ -run 'Jev|Threshold' -v`
Expected: FAIL to compile — unknown fields `JevEnabled`, `OpenRouterKey`, etc.

- [ ] **Step 3: Implement config**

In `internal/config/store.go`, add to `StoredConfig` after `BatchConcurrency`:

```go
	JevEnabled       *bool    `json:"jev_enabled,omitempty"`
	OpenRouterKey    string   `json:"openrouter_api_key,omitempty"`
	JevModel         string   `json:"jev_model,omitempty"`
	JevHighThreshold *float64 `json:"jev_high_threshold,omitempty"`
	JevLowThreshold  *float64 `json:"jev_low_threshold,omitempty"`
```

In `internal/config/config.go`, add to `Config` after `BatchConcurrency`:

```go
	// Jev pre-classifier (OpenRouter Decisions API).
	JevEnabled       bool
	OpenRouterKey    string
	JevModel         string
	JevHighThreshold float64
	JevLowThreshold  float64
```

In `Load()`, add to the `cfg := &Config{...}` literal after `BatchConcurrency`:

```go
		JevEnabled:              getEnv("JEV_ENABLED", "false") == "true",
		OpenRouterKey:           getEnv("OPENROUTER_API_KEY", ""),
		JevModel:                getEnv("JEV_MODEL", "typesafe/jev-1.13"),
		JevHighThreshold:        getEnvFloat("JEV_HIGH_THRESHOLD", 0.85),
		JevLowThreshold:         getEnvFloat("JEV_LOW_THRESHOLD", 0.50),
```

At the end of `ApplyStored`, add:

```go
	if sc.JevEnabled != nil {
		cfg.JevEnabled = *sc.JevEnabled
	}
	if sc.OpenRouterKey != "" {
		cfg.OpenRouterKey = sc.OpenRouterKey
	}
	if sc.JevModel != "" {
		cfg.JevModel = sc.JevModel
	}
	if sc.JevHighThreshold != nil {
		cfg.JevHighThreshold = *sc.JevHighThreshold
	}
	if sc.JevLowThreshold != nil {
		cfg.JevLowThreshold = *sc.JevLowThreshold
	}
```

After `IsConfigured`, add:

```go
// ValidateJevThresholds requires 0 <= low <= high <= 1.
func (c *Config) ValidateJevThresholds() error {
	if c.JevLowThreshold < 0 || c.JevHighThreshold > 1 || c.JevLowThreshold > c.JevHighThreshold {
		return fmt.Errorf("jev thresholds must satisfy 0 <= low (%.2f) <= high (%.2f) <= 1", c.JevLowThreshold, c.JevHighThreshold)
	}
	return nil
}
```

After `getEnvInt`, add:

```go
func getEnvFloat(key string, def float64) float64 {
	s := getEnv(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return v
}
```

- [ ] **Step 4: Implement Build**

Replace `internal/classifier/build.go` with:

```go
package classifier

import "log/slog"

// BuildParams holds the fields needed to construct any Classifier implementation.
type BuildParams struct {
	Provider      string
	OpenAIKey     string
	OpenAIModel   string
	OpenAIBaseURL string
	GeminiKey     string
	GeminiModel   string
	DeepseekKey   string
	DeepseekModel string
	CustomContext string // optional free-text context appended to the system prompt

	// Jev pre-classifier. When enabled with a key, the LLM above becomes the
	// fallback for uncertain decisions.
	JevEnabled    bool
	OpenRouterKey string
	JevModel      string
	JevThresholds Thresholds
}

// Build constructs the appropriate Classifier for the given provider.
func Build(p BuildParams) (Classifier, error) {
	llm, err := buildLLM(p)
	if err != nil {
		return nil, err
	}
	if !p.JevEnabled {
		return llm, nil
	}
	if p.OpenRouterKey == "" {
		slog.Warn("jev enabled but no OpenRouter API key is set; using the AI provider only")
		return llm, nil
	}
	return newCascade(llm, NewJevClient(p.OpenRouterKey, p.JevModel), p.JevThresholds, p.CustomContext), nil
}

func buildLLM(p BuildParams) (Classifier, error) {
	switch p.Provider {
	case "gemini":
		return NewGemini(p.GeminiKey, p.GeminiModel, p.CustomContext)
	case "deepseek":
		return NewDeepseek(p.DeepseekKey, p.DeepseekModel, p.CustomContext), nil
	default:
		return NewOpenAI(p.OpenAIKey, p.OpenAIModel, p.OpenAIBaseURL, p.CustomContext), nil
	}
}
```

- [ ] **Step 5: Implement API and CLI wiring**

In `internal/api/handler.go`:

Add to `configResponse` after `BatchConcurrency`:

```go
	JevEnabled       bool    `json:"jev_enabled"`
	OpenRouterKeySet bool    `json:"openrouter_key_set"`
	JevModel         string  `json:"jev_model"`
	JevHighThreshold float64 `json:"jev_high_threshold"`
	JevLowThreshold  float64 `json:"jev_low_threshold"`
```

Add to the `configResponse{...}` literal in `getConfig` after `BatchConcurrency: cfg.BatchConcurrency,`:

```go
		JevEnabled:       cfg.JevEnabled,
		OpenRouterKeySet: cfg.OpenRouterKey != "",
		JevModel:         cfg.JevModel,
		JevHighThreshold: cfg.JevHighThreshold,
		JevLowThreshold:  cfg.JevLowThreshold,
```

Add to `configUpdateRequest` after `BatchConcurrency`:

```go
	JevEnabled       *bool    `json:"jev_enabled"`
	OpenRouterKey    *string  `json:"openrouter_api_key"`
	JevModel         *string  `json:"jev_model"`
	JevHighThreshold *float64 `json:"jev_high_threshold"`
	JevLowThreshold  *float64 `json:"jev_low_threshold"`
```

In `putConfig`, replace

```go
	existing := h.store.Get()
	updated := mergeConfigUpdate(existing, req)

	if err := h.store.Save(updated); err != nil {
```

with

```go
	existing := h.store.Get()
	updated := mergeConfigUpdate(existing, req)

	effective := *h.baseCfg
	config.ApplyStored(&effective, updated)
	if err := effective.ValidateJevThresholds(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.store.Save(updated); err != nil {
```

At the end of `mergeConfigUpdate`, before `return existing`, add:

```go
	if req.JevEnabled != nil {
		existing.JevEnabled = req.JevEnabled
	}
	if req.OpenRouterKey != nil && *req.OpenRouterKey != "" {
		existing.OpenRouterKey = *req.OpenRouterKey
	}
	if req.JevModel != nil {
		existing.JevModel = *req.JevModel
	}
	if req.JevHighThreshold != nil {
		existing.JevHighThreshold = req.JevHighThreshold
	}
	if req.JevLowThreshold != nil {
		existing.JevLowThreshold = req.JevLowThreshold
	}
```

In `reloadClients`, add to the `classifier.BuildParams{...}` literal after `CustomContext: cfg.CustomSystemContext,`:

```go
		JevEnabled:    cfg.JevEnabled,
		OpenRouterKey: cfg.OpenRouterKey,
		JevModel:      cfg.JevModel,
		JevThresholds: classifier.Thresholds{High: cfg.JevHighThreshold, Low: cfg.JevLowThreshold},
```

and change the reload log line to:

```go
	slog.Info("clients reloaded", "provider", cfg.AIProvider, "jev", cfg.JevEnabled, "firefly", cfg.FireflyURL)
```

In `cmd/server/main.go` `runBatch`, add the same four lines to the `classifier.BuildParams{...}` literal after `CustomContext: cfg.CustomSystemContext,`:

```go
		JevEnabled:    cfg.JevEnabled,
		OpenRouterKey: cfg.OpenRouterKey,
		JevModel:      cfg.JevModel,
		JevThresholds: classifier.Thresholds{High: cfg.JevHighThreshold, Low: cfg.JevLowThreshold},
```

- [ ] **Step 6: Run all tests**

Run: `go test ./... && go vet ./...`
Expected: PASS, vet clean.

- [ ] **Step 7: Commit**

```bash
git add internal/config internal/classifier/build.go internal/classifier/build_test.go internal/api cmd/server/main.go
git commit -m "feat: configure and wire the Jev cascade from env vars and settings API

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Settings UI, Jobs source label and documentation

**Files:**
- Modify: `public/index.html` (AI Provider box, after the `section-deepseek` div, before its closing `</form>`)
- Modify: `public/app.js` (`buildJobRow`, `loadSettings`, `saveSettings`, new `sourceLabel`)
- Modify: `README.md` (env var tables, "How it works")
- Modify: `public/docs.html` (Settings subsection and "How it works")

**Interfaces:**
- Consumes: JSON fields from Task 6 and `job.source` from Task 1.

No automated frontend tests exist in this repo; this task is verified manually in Step 6.

- [ ] **Step 1: Settings panel**

In `public/index.html`, immediately after the closing `</div>` of `<div id="section-deepseek" style="display:none">` and before `</form>`, insert:

```html
                                        <hr>
                                        <h4 style="margin-top:0"><i class="fa fa-bolt"></i> Jev pre-classifier (OpenRouter)</h4>
                                        <div class="form-group">
                                            <div class="col-sm-offset-4 col-sm-8">
                                                <div class="checkbox">
                                                    <label>
                                                        <input type="checkbox" id="cfg-jev-enabled">
                                                        <strong>Use Jev before the AI provider</strong>
                                                    </label>
                                                </div>
                                                <p class="help-block" style="margin-top:2px">
                                                    Jev decides the category with calibrated probabilities; only
                                                    uncertain cases are sent to the AI provider above.
                                                </p>
                                            </div>
                                        </div>
                                        <div class="form-group">
                                            <label class="col-sm-4 control-label">OpenRouter key</label>
                                            <div class="col-sm-8">
                                                <input type="password" class="form-control" id="cfg-openrouter-key"
                                                    placeholder="Leave blank to keep existing">
                                                <p class="help-block" id="openrouter-key-hint"
                                                    style="display:none;color:#3c763d">
                                                    <i class="fa fa-check-circle"></i> Key is configured
                                                </p>
                                            </div>
                                        </div>
                                        <div class="form-group">
                                            <label class="col-sm-4 control-label">Model</label>
                                            <div class="col-sm-8">
                                                <input type="text" class="form-control" id="cfg-jev-model"
                                                    placeholder="typesafe/jev-1.13">
                                            </div>
                                        </div>
                                        <div class="form-group">
                                            <label class="col-sm-4 control-label">Thresholds</label>
                                            <div class="col-sm-4">
                                                <input type="number" class="form-control" id="cfg-jev-high"
                                                    min="0" max="1" step="0.05" placeholder="0.85">
                                                <p class="help-block">Accept at or above</p>
                                            </div>
                                            <div class="col-sm-4">
                                                <input type="number" class="form-control" id="cfg-jev-low"
                                                    min="0" max="1" step="0.05" placeholder="0.50">
                                                <p class="help-block">Needs review below</p>
                                            </div>
                                        </div>
```

- [ ] **Step 2: Load and save settings**

In `public/app.js` `loadSettings`, after `$('#cfg-batch-concurrency').val(d.batch_concurrency || '');` add:

```js
        $('#cfg-jev-enabled').prop('checked', !!d.jev_enabled);
        $('#cfg-openrouter-key').val('');
        $('#openrouter-key-hint').toggle(!!d.openrouter_key_set);
        $('#cfg-jev-model').val(d.jev_model || '');
        $('#cfg-jev-high').val(d.jev_high_threshold != null ? d.jev_high_threshold : '');
        $('#cfg-jev-low').val(d.jev_low_threshold != null ? d.jev_low_threshold : '');
```

In `saveSettings`, after the `batch_concurrency` lines (`if (!isNaN(bc) && bc > 0) payload.batch_concurrency = bc;`) add:

```js
    payload.jev_enabled = $('#cfg-jev-enabled').prop('checked');
    var ork = $('#cfg-openrouter-key').val();
    if (ork) payload.openrouter_api_key = ork;
    var jm = $('#cfg-jev-model').val().trim();
    if (jm) payload.jev_model = jm;
    var jh = parseFloat($('#cfg-jev-high').val());
    if (!isNaN(jh)) payload.jev_high_threshold = jh;
    var jl = parseFloat($('#cfg-jev-low').val());
    if (!isNaN(jl)) payload.jev_low_threshold = jl;
```

(Invalid thresholds come back as a 400 whose text is shown by the existing `catch` in `saveSettings`.)

- [ ] **Step 3: Source label in Jobs**

In `public/app.js`, after `jobLabel`, add:

```js
function sourceLabel(j) {
    switch (j.source) {
        case 'history': return 'History';
        case 'jev': return 'Jev';
        case 'jev+llm': return 'Jev → LLM';
        case 'llm': return 'LLM';
    }
    return '';
}
```

In `buildJobRow`, replace the outcome cell

```js
        + '<td><span class="label ' + lbl.cls + '">' + lbl.txt + '</span></td>'
```

with

```js
        + '<td><span class="label ' + lbl.cls + '">' + lbl.txt + '</span>'
        + (sourceLabel(j) ? ' <small class="text-muted" title="Decided by">' + sourceLabel(j) + '</small>' : '')
        + '</td>'
```

- [ ] **Step 4: README**

In `README.md`, after the "### AI provider" env var table, add:

```markdown
### Jev pre-classifier (optional)

[Jev](https://openrouter.ai/docs/guides/community/jev) is TypeSafe's decision model on OpenRouter. When enabled, it picks the category (and payee account) first, with calibrated probabilities, and only uncertain transactions are sent to the AI provider above — cutting LLM calls and cost.

| Variable | Default | Description |
|----------|---------|-------------|
| `JEV_ENABLED` | `false` | Ask Jev before the AI provider |
| `OPENROUTER_API_KEY` | — | OpenRouter API key used for Jev |
| `JEV_MODEL` | `typesafe/jev-1.13` | Jev model ID |
| `JEV_HIGH_THRESHOLD` | `0.85` | Jev probability at or above which the category is accepted (`CLASSIFIED`) |
| `JEV_LOW_THRESHOLD` | `0.50` | Below this the transaction is flagged `NEEDS_REVIEW` without an LLM call; between the two thresholds the AI provider decides |

When Jev picks "none of the listed accounts" with high confidence, the AI provider is called once to name the new expense account. If Jev is unavailable, every transaction falls back to the AI provider. The Jobs page shows who decided each transaction: `History`, `Jev`, `Jev → LLM` or `LLM`.
```

In the "## How it works" diagram, replace the line `  Send to LLM with three-outcome prompt` with:

```
  (Optional) Jev decides category [+ payee] — confident: done · unsure: continue · hopeless: NEEDS_REVIEW
        │
        ▼
  Send to LLM with three-outcome prompt
```

- [ ] **Step 5: docs.html**

In `public/docs.html`, in the `<h2 id="architecture">How it works</h2>` `<pre>` block, replace `  Send to LLM with three-outcome prompt` with the same three lines as in Step 4. Then, at the end of the `<h3>Settings</h3>` subsection (before `<h2 id="outcomes">`), add:

```html
                <h4>Jev pre-classifier</h4>
                <p>
                    Enable <strong>Use Jev before the AI provider</strong> and enter an OpenRouter key to let
                    TypeSafe's Jev decision model classify transactions first. Jev returns a probability for its
                    choice: at or above the <em>accept</em> threshold the category is applied directly, below the
                    <em>review</em> threshold the transaction goes to the Review page, and anything in between is
                    sent to the AI provider. The Jobs table shows which stage decided each transaction.
                </p>
```

- [ ] **Step 6: Manual verification**

Run: `go build -o /tmp/categorizer ./cmd/server && PORT=3999 CONFIG_FILE=$(mktemp -d)/config.json /tmp/categorizer`
Then in a browser at `http://localhost:3999`:
1. Settings shows the Jev panel; toggle on, set thresholds 0.4/0.6, Save → red error mentioning `jev thresholds must satisfy`.
2. Set 0.85/0.50 with an OpenRouter key, Save → key hint "Key is configured" appears after reload.
3. With a configured Firefly instance: run a batch on a handful of transactions; Jobs rows show `Jev` / `Jev → LLM` labels; expanding a row shows the Jev JSON request/response.

Expected: all three behave as described; server log shows `"jev":true` on `clients reloaded`.

- [ ] **Step 7: Commit**

```bash
git add public/index.html public/app.js README.md public/docs.html
git commit -m "feat: add Jev settings panel, decision source label and docs

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
