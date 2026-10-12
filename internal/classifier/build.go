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
