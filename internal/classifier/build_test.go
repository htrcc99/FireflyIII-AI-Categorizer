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
