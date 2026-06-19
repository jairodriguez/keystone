package registry

import (
	_ "embed"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed data/models.yaml
var modelsYAML []byte

type ModelRegistry struct {
	Models []ModelConfig `yaml:"models"`
}

type ModelConfig struct {
	ID              string          `yaml:"id"`
	DisplayName     string          `yaml:"display_name"`
	ContextWindow   int             `yaml:"context_window"`
	Status          string          `yaml:"status"`
	Tiers           []string        `yaml:"tiers"`
	Strengths       []string        `yaml:"strengths"`
	Providers       []ModelProvider `yaml:"providers"`
}

type ModelProvider struct {
	Name       string           `yaml:"name"`
	ModelName  string           `yaml:"model_name"`
	FreeTier   *FreeTierConfig  `yaml:"free_tier"`
}

type FreeTierConfig struct {
	RateLimitRPM  int    `yaml:"rate_limit_rpm"`
	RateLimitDaily int   `yaml:"rate_limit_daily"`
	SignupURL     string `yaml:"signup_url"`
	KeyLimit      string `yaml:"key_limit"`
	Credits       string `yaml:"credits"`
}

var DefaultRegistry *ModelRegistry

func Load() (*ModelRegistry, error) {
	var reg ModelRegistry
	if err := yaml.Unmarshal(modelsYAML, &reg); err != nil {
		return nil, err
	}
	DefaultRegistry = &reg
	return &reg, nil
}

func (r *ModelRegistry) GetModel(id string) *ModelConfig {
	for _, m := range r.Models {
		if m.ID == id {
			return &m
		}
	}
	return nil
}

func (r *ModelRegistry) GetModelsByTier(tier string) []ModelConfig {
	var out []ModelConfig
	for _, m := range r.Models {
		for _, t := range m.Tiers {
			if t == tier {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

func (r *ModelRegistry) GetProvidersForModel(modelID string) []ModelProvider {
	m := r.GetModel(modelID)
	if m == nil {
		return nil
	}
	return m.Providers
}

func (r *ModelRegistry) GetFreeTierInfo(provider, modelID string) *FreeTierConfig {
	for _, m := range r.Models {
		if m.ID != modelID {
			continue
		}
		for _, p := range m.Providers {
			if strings.EqualFold(p.Name, provider) {
				return p.FreeTier
			}
		}
	}
	return nil
}

func (r *ModelRegistry) AllModels() []ModelConfig {
	return r.Models
}

func (r *ModelRegistry) GetSignupURL(provider, modelID string) string {
	ft := r.GetFreeTierInfo(provider, modelID)
	if ft != nil && ft.SignupURL != "" {
		return ft.SignupURL
	}
	return ""
}
