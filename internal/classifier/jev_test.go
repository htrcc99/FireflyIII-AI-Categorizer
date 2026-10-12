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
