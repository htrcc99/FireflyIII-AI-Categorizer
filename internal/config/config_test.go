package config

import (
	"math"
	"testing"
)

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
		{math.NaN(), 0.5, false},
		{0.85, math.Inf(-1), false},
	}
	for _, tc := range cases {
		err := (&Config{JevHighThreshold: tc.high, JevLowThreshold: tc.low}).ValidateJevThresholds()
		if (err == nil) != tc.ok {
			t.Errorf("high=%v low=%v: err = %v", tc.high, tc.low, err)
		}
	}
}

func TestLoadResetsInvalidThresholds(t *testing.T) {
	t.Setenv("CONFIG_FILE", t.TempDir()+"/config.json")
	t.Setenv("JEV_HIGH_THRESHOLD", "85")
	t.Setenv("JEV_LOW_THRESHOLD", "NaN")
	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JevHighThreshold != 0.85 || cfg.JevLowThreshold != 0.50 {
		t.Errorf("invalid env thresholds not reset: high=%v low=%v", cfg.JevHighThreshold, cfg.JevLowThreshold)
	}
}
