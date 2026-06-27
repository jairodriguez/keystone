package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/clawdbot/keystone/internal/keypool"
)

type Provider struct {
	Name    string
	BaseURL string
	Pool    *keypool.Pool
	Models  []string
}

func New(name, baseURL string, pool *keypool.Pool, models []string) *Provider {
	return &Provider{Name: name, BaseURL: baseURL, Pool: pool, Models: models}
}

type Resolution struct {
	Key      *keypool.Key
	Provider *Provider
	Model    string
}

func (p *Provider) SelectKey() (*keypool.Key, error) {
	return p.Pool.Select()
}

func (p *Provider) HasModel(model string) bool {
	for _, m := range p.Models {
		if m == model {
			return true
		}
	}
	return false
}

type Registry struct {
	providers map[string]*Provider
}

func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]*Provider)}
}

func (r *Registry) Register(p *Provider) {
	r.providers[p.Name] = p
}

func (r *Registry) Get(name string) (*Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

func (r *Registry) All() []*Provider {
	out := make([]*Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p)
	}
	return out
}

func (r *Registry) FindProvidersForModel(model string) []*Provider {
	var matches []*Provider
	for _, p := range r.providers {
		if p.HasModel(model) {
			matches = append(matches, p)
		}
	}
	return matches
}

func (r *Registry) AnyHealthy() []*Provider {
	var healthy []*Provider
	for _, p := range r.providers {
		if p.Pool.IsHealthy() {
			healthy = append(healthy, p)
		}
	}
	return healthy
}

func ParseBaseURL(rawURL string) (scheme, host, basePath string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", err
	}
	scheme = u.Scheme
	host = u.Host
	basePath = strings.TrimSuffix(u.Path, "/")
	return
}

func RewriteRequest(req *http.Request, provider *Provider, key *keypool.Key, modelName string) error {
	scheme, host, basePath, err := ParseBaseURL(provider.BaseURL)
	if err != nil {
		return err
	}

	req.URL.Scheme = scheme
	req.URL.Host = host
	req.URL.Path = basePath + "/chat/completions"
	req.Host = host

	req.Header.Set("Authorization", "Bearer "+key.Value)

	if modelName != "" {
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		req.Body.Close()

		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.ContentLength = int64(len(bodyBytes))
		} else {
			if currentModel, ok := body["model"].(string); ok && currentModel != modelName {
				body["model"] = modelName
			}
			// Strip parameters that the target model/provider may not support.
			// These are model-specific extensions that cause 400s when sent to models that don't understand them.
			delete(body, "thinking")
			delete(body, "reasoning_effort")
			delete(body, "reasoning")
			newBytes, _ := json.Marshal(body)
			req.Body = io.NopCloser(bytes.NewReader(newBytes))
			req.ContentLength = int64(len(newBytes))
		}
	}

	return nil
}

func ResolveModelName(requested, providerName string, modelMap map[string]map[string]string) string {
	if mapping, ok := modelMap[requested]; ok {
		if name, ok := mapping[providerName]; ok && name != "" {
			return name
		}
	}
	return requested
}
