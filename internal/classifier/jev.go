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
