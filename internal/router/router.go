package router

import (
	"fmt"

	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/keypool"
	"github.com/clawdbot/keystone/internal/provider"
	"github.com/clawdbot/keystone/internal/registry"
)

type Decision struct {
	Tier     string
	Provider *provider.Provider
	Key      *keypool.Key
	Model    string
	Sticky   bool
	Reason   string
}

func (d *Decision) String() string {
	provName := "?"
	if d.Provider != nil {
		provName = d.Provider.Name
	}
	keyID := "?"
	if d.Key != nil {
		keyID = d.Key.ID
	}
	return fmt.Sprintf("tier=%s provider=%s key=%s model=%s sticky=%v reason=%s",
		d.Tier, provName, keyID, d.Model, d.Sticky, d.Reason)
}

type Router struct {
	Registry   *provider.Registry
	Config     *config.Config
	ModelReg   *registry.ModelRegistry
}

func New(reg *provider.Registry, cfg *config.Config, modelReg *registry.ModelRegistry) *Router {
	return &Router{Registry: reg, Config: cfg, ModelReg: modelReg}
}

func (r *Router) SelectProviderAndKey(tier, model string, contextTokens int) (*Decision, error) {
	chain, ok := r.Config.Fallback.Chains[tier]
	if !ok || len(chain) == 0 {
		providers := r.Registry.FindProvidersForModel(model)
		if len(providers) == 0 {
			return nil, fmt.Errorf("no providers for model %s in tier %s", model, tier)
		}
		chain = make([]string, 0, len(providers))
		for _, p := range providers {
			chain = append(chain, p.Name)
		}
	}

	// Check which models are valid for this tier
	tierModels := map[string]bool{}
	if tierCfg, ok := r.Config.Tiers[tier]; ok {
		for _, m := range tierCfg.Models {
			tierModels[m] = true
		}
	}

	// Build list of models to try: requested model first (if in tier), then tier defaults
	var modelsToTry []string
	if len(tierModels) > 0 {
		for _, m := range []string{model} {
			if tierModels[m] {
				modelsToTry = append(modelsToTry, m)
			}
		}
		for m := range tierModels {
			if m != model {
				modelsToTry = append(modelsToTry, m)
			}
		}
	} else {
		modelsToTry = []string{model}
	}

	for _, tryModel := range modelsToTry {
		// Check context window if model registry is available
		if r.ModelReg != nil && contextTokens > 0 {
			if modelConfig := r.ModelReg.GetModel(tryModel); modelConfig != nil {
				if contextTokens > modelConfig.ContextWindow {
					continue // Skip this model, context too large
				}
			}
		}

		dec, err := r.tryChain(chain, tier, tryModel)
		if err == nil {
			return dec, nil
		}
	}

	// Cross-tier fallback
	if r.Config.Fallback.CrossTier {
		lower := NextLowerTier(tier)
		if lower != "" {
			return r.SelectProviderAndKey(lower, model, contextTokens)
		}
	}

	return nil, fmt.Errorf("all providers exhausted for tier %s model %s", tier, model)
}

func (r *Router) tryChain(chain []string, tier, model string) (*Decision, error) {
	for _, provName := range chain {
		p, ok := r.Registry.Get(provName)
		if !ok {
			continue
		}
		if !p.HasModel(model) {
			continue
		}
		if !p.Pool.IsHealthy() {
			continue
		}
		key, err := p.Pool.Select()
		if err != nil {
			continue
		}
		modelName := provider.ResolveModelName(model, p.Name, r.Config.ModelMap)
		return &Decision{
			Tier:     tier,
			Provider: p,
			Key:      key,
			Model:    modelName,
			Sticky:   false,
			Reason:   "selected_" + p.Name,
		}, nil
	}
	return nil, fmt.Errorf("no available provider in chain for model %s", model)
}

func (r *Router) SelectForProvider(tier, providerName string) (*Decision, error) {
	p, ok := r.Registry.Get(providerName)
	if !ok {
		return nil, fmt.Errorf("provider %s not found", providerName)
	}
	if !p.Pool.IsHealthy() {
		return nil, fmt.Errorf("provider %s has no healthy keys", providerName)
	}

	tierCfg, ok := r.Config.Tiers[tier]
	if !ok || len(tierCfg.Models) == 0 {
		return nil, fmt.Errorf("tier %s has no models", tier)
	}

	for _, m := range tierCfg.Models {
		if p.HasModel(m) {
			key, err := p.Pool.Select()
			if err != nil {
				continue
			}
			modelName := provider.ResolveModelName(m, p.Name, r.Config.ModelMap)
			return &Decision{
				Tier:     tier,
				Provider: p,
				Key:      key,
				Model:    modelName,
				Sticky:   false,
				Reason:   "selected_" + p.Name,
			}, nil
		}
	}

	return nil, fmt.Errorf("provider %s has no models in tier %s", providerName, tier)
}

func (r *Router) SelectForTier(tier string) (*Decision, error) {
	tierCfg, ok := r.Config.Tiers[tier]
	if !ok || len(tierCfg.Models) == 0 {
		return r.SelectProviderAndKey(tier, "", 0)
	}
	return r.SelectProviderAndKey(tier, tierCfg.Models[0], 0)
}

func NextLowerTier(tier string) string {
	switch tier {
	case "premium":
		return "coder"
	case "coder":
		return "mid"
	case "mid":
		return "free"
	default:
		return ""
	}
}

func NextHigherTier(tier string) string {
	switch tier {
	case "free":
		return "mid"
	case "mid":
		return "coder"
	case "coder":
		return "premium"
	default:
		return ""
	}
}

func TierRank(tier string) int {
	switch tier {
	case "free":
		return 0
	case "mid":
		return 1
	case "coder":
		return 2
	case "premium":
		return 3
	default:
		return 1
	}
}

var ErrNoProviders = fmt.Errorf("no providers available")
