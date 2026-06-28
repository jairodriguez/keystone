package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clawdbot/keystone/internal/classify"
	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/economics"
	"github.com/clawdbot/keystone/internal/keypool"
	"github.com/clawdbot/keystone/internal/metrics"
	"github.com/clawdbot/keystone/internal/provider"
	"github.com/clawdbot/keystone/internal/registry"
	"github.com/clawdbot/keystone/internal/router"
	"github.com/clawdbot/keystone/internal/session"
	"github.com/rs/zerolog/log"
)


type Server struct {
	Config      *config.Config
	Registry    *provider.Registry
	Router      *router.Router
	ModelReg    *registry.ModelRegistry
	SessionMgr  *session.Manager
	Econ        *economics.Engine
	Classifier  classify.Classifier
	API         *apiAdapter
	client      *http.Client
	inFlight    map[string]int    // provider name → active request count
	maxConcurrent map[string]int  // provider name → max concurrent
	mu          sync.Mutex
}

type apiAdapter struct {
	GetMode func() string
}

func New(cfg *config.Config, reg *provider.Registry, rt *router.Router, modelReg *registry.ModelRegistry, sm *session.Manager, econ *economics.Engine, cls classify.Classifier, modeFn func() string) *Server {
	s := &Server{
		Config:     cfg,
		Registry:   reg,
		Router:     rt,
		ModelReg:   modelReg,
		SessionMgr: sm,
		Econ:       econ,
		Classifier: cls,
		API:        &apiAdapter{GetMode: modeFn},
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				ResponseHeaderTimeout: cfg.Server.ProxyTimeout,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		inFlight:      make(map[string]int),
		maxConcurrent: make(map[string]int),
	}
	// Load per-provider concurrency limits from config
	for _, pc := range cfg.Providers {
		if pc.MaxConcurrent > 0 {
			s.maxConcurrent[pc.Name] = pc.MaxConcurrent
		}
	}
	return s
}

// canSend checks if a provider has capacity for another concurrent request
func (s *Server) canSend(providerName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	max, ok := s.maxConcurrent[providerName]
	if !ok || max <= 0 {
		return true // no limit configured
	}
	return s.inFlight[providerName] < max
}

// acquire increments the in-flight counter for a provider
func (s *Server) acquire(providerName string) {
	s.mu.Lock()
	s.inFlight[providerName]++
	s.mu.Unlock()
}

// release decrements the in-flight counter for a provider
func (s *Server) release(providerName string) {
	s.mu.Lock()
	if s.inFlight[providerName] > 0 {
		s.inFlight[providerName]--
	}
	s.mu.Unlock()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var reqBody map[string]any
	if err := json.Unmarshal(bodyBytes, &reqBody); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	requestedModel, _ := reqBody["model"].(string)
	if requestedModel == "" {
		http.Error(w, "missing model field", http.StatusBadRequest)
		return
	}

	isAuto := requestedModel == "auto"
	if isAuto {
		requestedModel = ""
	}

	sessionID := s.SessionMgr.ResolveSessionID(
		r.Header.Get(s.Config.Sessions.Header),
		string(bodyBytes),
	)
	agent := r.Header.Get(s.Config.Sessions.AgentHeader)

	sess := s.SessionMgr.GetOrCreate(sessionID, agent)

	// Heal poisoned conversation history: remove assistant messages with empty content AND no tool_calls.
	// These get created when a model returns empty, and they cause 400 errors on all subsequent requests.
	if msgs, ok := reqBody["messages"].([]any); ok {
		healed := false
		cleaned := make([]any, 0, len(msgs))
		for _, m := range msgs {
			if msg, ok := m.(map[string]any); ok {
				if role, _ := msg["role"].(string); role == "assistant" {
					content, _ := msg["content"].(string)
					toolCalls := msg["tool_calls"]
					if content == "" && toolCalls == nil {
						healed = true
						continue
					}
				}
			}
			cleaned = append(cleaned, m)
		}
		if healed {
			reqBody["messages"] = cleaned
			bodyBytes, _ = json.Marshal(reqBody)
			log.Warn().Str("session", sessionID).Int("removed", len(msgs)-len(cleaned)).Msg("healed poisoned assistant messages from conversation")
		}
	}

	clsResult := s.Classifier.Classify(
		extractPrompt(reqBody),
		sess.ContextEst,
		sess.TurnCount,
	)

	econDecision := s.Econ.Decide(clsResult, sess, agent, requestedModel)

	var decision *router.Decision
	sticky := false

	// Helper to check if a model is valid for a tier
	modelValidForTier := func(model, tier string) bool {
		if tierCfg, ok := s.Config.Tiers[tier]; ok {
			for _, m := range tierCfg.Models {
				if m == model {
					return true
				}
			}
		}
		return false
	}

	if econDecision.Sticky && sess.IsStickyEligible() && sess.Provider != "" {
		// Only use sticky if the session's model is valid for the new tier
		if modelValidForTier(sess.Model, econDecision.Tier) {
			prov, ok := s.Registry.Get(sess.Provider)
			if ok {
				key, err := prov.Pool.SelectSpecific(sess.Key.ID)
				if err == nil && key.State == keypool.StateHealthy {
					resolved := provider.ResolveModelName(sess.Model, sess.Provider, s.Config.ModelMap)
					prov, _ := s.Registry.Get(sess.Provider)
					decision = &router.Decision{
						Tier:     sess.Tier,
						Provider: prov,
						Key:      key,
						Model:    resolved,
						Sticky:   true,
						Reason:   "session_sticky",
					}
					sticky = true
					metrics.StickyDecisions.WithLabelValues("sticky").Inc()
				}
			}
		}
	}

	if decision == nil {
		decision, err = s.Router.SelectProviderAndKey(econDecision.Tier, requestedModel, sess.ContextEst)
		if err != nil {
			// If the specific model wasn't found, try auto-routing (pick any model from the tier)
			if requestedModel != "" && requestedModel != "auto" {
				log.Info().Str("model", requestedModel).Str("tier", econDecision.Tier).Msg("model not found, auto-routing")
				decision, err = s.Router.SelectProviderAndKey(econDecision.Tier, "", sess.ContextEst)
			}
		}
		if err != nil {
			// Tier escalation: try higher tiers before giving up
			for _, higherTier := range []string{"mid", "coder", "premium"} {
				if router.TierRank(higherTier) <= router.TierRank(econDecision.Tier) {
					continue
				}
				decision, err = s.Router.SelectProviderAndKey(higherTier, "", sess.ContextEst)
				if err == nil {
					log.Info().Str("from_tier", econDecision.Tier).Str("to_tier", higherTier).Msg("tier escalated due to exhaustion")
					break
				}
			}
		}
		if decision == nil {
			// Last resort: any healthy provider with any model
			for _, p := range s.Registry.All() {
				if p.Pool.IsHealthy() && len(p.Models) > 0 {
					key, kErr := p.Pool.Select()
					if kErr == nil {
						log.Warn().Str("provider", p.Name).Str("model", p.Models[0]).Msg("last resort fallback to any healthy provider")
						decision = &router.Decision{
							Tier:     "free",
							Provider: p,
							Key:      key,
							Model:    p.Models[0],
							Sticky:   false,
							Reason:   "last_resort_" + p.Name,
						}
						break
					}
				}
			}
		}
		if decision == nil {
			log.Error().Str("tier", econDecision.Tier).Str("model", requestedModel).Msg("all providers exhausted across all tiers")
			metrics.RequestsTotal.WithLabelValues("none", econDecision.Tier, requestedModel, "503").Inc()
			http.Error(w, "all providers exhausted", http.StatusServiceUnavailable)
			return
		}
		metrics.StickyDecisions.WithLabelValues("new_binding").Inc()
	}

	maxRetries := 6
	acquiredProvider := ""
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Release previous provider's concurrency slot
		if acquiredProvider != "" {
			s.release(acquiredProvider)
			acquiredProvider = ""
		}
		proxyReq, err := http.NewRequest(http.MethodPost, "http://placeholder/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		for k, vv := range r.Header {
			if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "content-length") {
				continue
			}
			proxyReq.Header[k] = vv
		}

		if err := provider.RewriteRequest(proxyReq, decision.Provider, decision.Key, decision.Model); err != nil {
			log.Error().Err(err).Msg("rewrite request failed")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Concurrency check: skip provider if at max concurrent requests
		if !s.canSend(decision.Provider.Name) {
			log.Warn().Str("provider", decision.Provider.Name).Msg("provider at max concurrency, trying fallback")
			originalProvider := decision.Provider.Name
			decision = s.tryFallback(decision, econDecision.Tier, requestedModel, sess.ContextEst)
			if decision != nil {
				metrics.FallbackTotal.WithLabelValues(originalProvider, decision.Provider.Name, econDecision.Tier).Inc()
				continue
			}
		}

		s.acquire(decision.Provider.Name)
		acquiredProvider = decision.Provider.Name
		resp, err := s.client.Do(proxyReq)
		if err != nil {
			s.release(acquiredProvider)
			acquiredProvider = ""
			log.Error().Err(err).Str("provider", decision.Provider.Name).Msg("upstream request failed")
			s.triggerCooldown(decision, 502)
			decision = s.tryFallback(decision, econDecision.Tier, requestedModel, sess.ContextEst)
			if decision == nil {
				metrics.RequestsTotal.WithLabelValues("none", econDecision.Tier, requestedModel, "502").Inc()
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
				return
			}
			continue
		}

		if resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 || resp.StatusCode == 402 {
			cooldownDur := s.getCooldownDuration(decision.Provider.Name, resp.StatusCode)
			s.triggerCooldownWithCode(decision, resp.StatusCode, cooldownDur)

			originalProvider := decision.Provider.Name
			decision = s.tryFallback(decision, econDecision.Tier, requestedModel, sess.ContextEst)
			if decision == nil {
				resp.Body.Close()
				metrics.RequestsTotal.WithLabelValues(originalProvider, econDecision.Tier, requestedModel, strconv.Itoa(resp.StatusCode)).Inc()
				for k, vv := range resp.Header {
					if strings.HasPrefix(strings.ToLower(k), "x-") || strings.EqualFold(k, "content-type") {
						w.Header()[k] = vv
					}
				}
				w.WriteHeader(resp.StatusCode)
				io.Copy(w, resp.Body)
				resp.Body.Close()
				return
			}
			metrics.FallbackTotal.WithLabelValues(originalProvider, decision.Provider.Name, econDecision.Tier).Inc()
			resp.Body.Close()
			continue
		}

		if resp.StatusCode == 400 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bodyStr := string(body)

			isCtxLen := isContextLengthError(bodyStr)
			isUnknownModel := isUnknownModelError(bodyStr)
			isRateLimit := isResourceExhaustedError(bodyStr)

			if isCtxLen || isUnknownModel || isRateLimit {
				reason := "context length exceeded"
				if isUnknownModel {
					reason = "unknown model"
				} else if isRateLimit {
					reason = "resource exhausted"
				}
				log.Warn().
					Str("session", sessionID).
					Str("provider", decision.Provider.Name).
					Str("model", decision.Model).
					Str("tier", econDecision.Tier).
					Str("body_preview", bodyStr[:min(200, len(bodyStr))]).
					Msg(reason + ", attempting fallback")

				s.triggerCooldownWithCode(decision, 400, 30*time.Second)

				originalProvider := decision.Provider.Name
				// Context-length errors need tryContextFallback (finds models with larger context windows)
				// Rate-limit and unknown-model errors use tryFallback (standard provider/key rotation)
				if isCtxLen {
					decision = s.tryContextFallback(decision, econDecision.Tier, sess.ContextEst)
				} else {
					decision = s.tryFallback(decision, econDecision.Tier, requestedModel, sess.ContextEst)
				}
				if decision != nil {
					metrics.FallbackTotal.WithLabelValues(originalProvider, decision.Provider.Name, econDecision.Tier).Inc()
					continue
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			w.Write(body)
			return
		}

		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bodyStr := string(body)
			if isResourceExhaustedError(bodyStr) || isUnknownModelError(bodyStr) {
				log.Warn().
					Str("provider", decision.Provider.Name).
					Int("status", resp.StatusCode).
					Str("body_preview", bodyStr[:min(200, len(bodyStr))]).
					Msg("upstream error, attempting fallback")
				s.triggerCooldownWithCode(decision, resp.StatusCode, 60*time.Second)
				originalProvider := decision.Provider.Name
				decision = s.tryFallback(decision, econDecision.Tier, requestedModel, sess.ContextEst)
				if decision != nil {
					metrics.FallbackTotal.WithLabelValues(originalProvider, decision.Provider.Name, econDecision.Tier).Inc()
					continue
				}
			}
			metrics.RequestsTotal.WithLabelValues(decision.Provider.Name, econDecision.Tier, requestedModel, strconv.Itoa(resp.StatusCode)).Inc()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}

		for k, vv := range resp.Header {
			w.Header()[k] = vv
		}
		w.Header().Set("x-keystone-provider", decision.Provider.Name)
		w.Header().Set("x-keystone-tier", decision.Tier)
		w.Header().Set("x-keystone-model", decision.Model)
		w.Header().Set("x-keystone-key", decision.Key.ID)
		w.Header().Set("x-keystone-sticky", strconv.FormatBool(sticky))
		w.Header().Set("x-keystone-reason", decision.Reason)
		w.Header().Set("x-keystone-session", sessionID)
		w.Header().Set("x-keystone-attempt", strconv.Itoa(attempt+1))

		isStream := false
		if s, ok := reqBody["stream"].(bool); ok && s {
			isStream = true
		}

		// For non-streaming: validate response has content before forwarding.
		// Empty content with no tool_calls poisons conversation history.
		if !isStream && attempt < maxRetries-1 {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if isEmptyResponse(respBody) {
				log.Warn().
					Str("session", sessionID).
					Str("provider", decision.Provider.Name).
					Str("model", decision.Model).
					Msg("empty response detected, retrying with fallback")
				s.triggerCooldownWithCode(decision, 200, 15*time.Second)
				originalProvider := decision.Provider.Name
				decision = s.tryFallback(decision, econDecision.Tier, requestedModel, sess.ContextEst)
				if decision != nil {
					metrics.FallbackTotal.WithLabelValues(originalProvider, decision.Provider.Name, econDecision.Tier).Inc()
					continue
				}
				// No fallback available — send the empty response as-is
				for k, vv := range resp.Header {
					w.Header()[k] = vv
				}
				w.Header().Set("x-keystone-provider", decision.Provider.Name)
				w.Header().Set("x-keystone-tier", econDecision.Tier)
				w.Header().Set("x-keystone-model", "empty_fallback_failed")
				w.WriteHeader(resp.StatusCode)
				w.Write(respBody)
				return
			}

			// Response is valid — forward it
			for k, vv := range resp.Header {
				w.Header()[k] = vv
			}
			w.Header().Set("x-keystone-provider", decision.Provider.Name)
			w.Header().Set("x-keystone-tier", decision.Tier)
			w.Header().Set("x-keystone-model", decision.Model)
			w.Header().Set("x-keystone-key", decision.Key.ID)
			w.Header().Set("x-keystone-sticky", strconv.FormatBool(sticky))
			w.Header().Set("x-keystone-reason", decision.Reason)
			w.Header().Set("x-keystone-session", sessionID)
			w.Header().Set("x-keystone-attempt", strconv.Itoa(attempt+1))
			w.WriteHeader(resp.StatusCode)
			w.Write(respBody)
		} else {
			// Streaming or last attempt
			if isStream && attempt < maxRetries-1 {
				// Buffer stream, check for empty content before forwarding
				streamData, hasContent := bufferAndCheckSSE(resp.Body)
				resp.Body.Close()

				if !hasContent {
					log.Warn().
						Str("session", sessionID).
						Str("provider", decision.Provider.Name).
						Str("model", decision.Model).
						Msg("empty streaming response detected, retrying with fallback")
					s.triggerCooldownWithCode(decision, 200, 15*time.Second)
					originalProvider := decision.Provider.Name
					decision = s.tryFallback(decision, econDecision.Tier, requestedModel, sess.ContextEst)
					if decision != nil {
						metrics.FallbackTotal.WithLabelValues(originalProvider, decision.Provider.Name, econDecision.Tier).Inc()
						continue
					}
					// No fallback — inject minimal placeholder so conversation isn't poisoned
					streamData = []byte("data: {\"choices\":[{\"delta\":{\"content\":\"I apologize, but I couldn't generate a response. Please try again.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}

				for k, vv := range resp.Header {
					w.Header()[k] = vv
				}
				w.Header().Set("x-keystone-provider", decision.Provider.Name)
				w.Header().Set("x-keystone-tier", decision.Tier)
				w.Header().Set("x-keystone-model", decision.Model)
				w.Header().Set("x-keystone-key", decision.Key.ID)
				w.Header().Set("x-keystone-sticky", strconv.FormatBool(sticky))
				w.Header().Set("x-keystone-reason", decision.Reason)
				w.Header().Set("x-keystone-session", sessionID)
				w.Header().Set("x-keystone-attempt", strconv.Itoa(attempt+1))
				w.WriteHeader(resp.StatusCode)
				w.Write(streamData)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			} else {
				// Non-streaming last attempt or non-streaming — forward directly
				for k, vv := range resp.Header {
					w.Header()[k] = vv
				}
				w.Header().Set("x-keystone-provider", decision.Provider.Name)
				w.Header().Set("x-keystone-tier", decision.Tier)
				w.Header().Set("x-keystone-model", decision.Model)
				w.Header().Set("x-keystone-key", decision.Key.ID)
				w.Header().Set("x-keystone-sticky", strconv.FormatBool(sticky))
				w.Header().Set("x-keystone-reason", decision.Reason)
				w.Header().Set("x-keystone-session", sessionID)
				w.Header().Set("x-keystone-attempt", strconv.Itoa(attempt+1))

				w.WriteHeader(resp.StatusCode)

				if isStream {
					s.streamSSE(w, resp.Body)
				} else {
					io.Copy(w, resp.Body)
				}
				resp.Body.Close()
			}
		}

		if !sticky {
			s.SessionMgr.Bind(sessionID, decision.Key, decision.Provider.Name, decision.Model, decision.Tier)
		}
		s.SessionMgr.IncrementTurn(sessionID, estimateContextTokens(reqBody))
		sess.LastClass = clsResult.TaskType

		duration := time.Since(start).Seconds()
		metrics.RequestDuration.WithLabelValues(decision.Provider.Name, decision.Tier).Observe(duration)
		metrics.RequestsTotal.WithLabelValues(decision.Provider.Name, decision.Tier, decision.Model, strconv.Itoa(resp.StatusCode)).Inc()

		log.Info().
			Str("session", sessionID).
			Str("provider", decision.Provider.Name).
			Str("tier", decision.Tier).
			Str("model", decision.Model).
			Bool("sticky", sticky).
			Int("status", resp.StatusCode).
			Str("reason", decision.Reason).
			Msg("request completed")

		return
	}

	if acquiredProvider != "" {
		s.release(acquiredProvider)
	}
	http.Error(w, "all retries exhausted", http.StatusBadGateway)
}

func (s *Server) streamSSE(w http.ResponseWriter, body io.ReadCloser) {
	defer body.Close()

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)

	for {
		n, err := body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Error().Err(err).Msg("SSE stream error")
			}
			return
		}
	}
}

// bufferAndCheckSSE reads an SSE stream into memory and checks whether any
// content was actually generated. Returns the raw bytes and whether content was found.
func bufferAndCheckSSE(body io.ReadCloser) ([]byte, bool) {
	data, err := io.ReadAll(body)
	if err != nil {
		return data, true // on error, assume non-empty to avoid false positives
	}

	hasContent := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		chunk := strings.TrimPrefix(line, "data: ")
		if chunk == "[DONE]" {
			continue
		}
		var event struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(chunk), &event) == nil {
			for _, c := range event.Choices {
				if c.Delta.Content != "" {
					hasContent = true
					break
				}
			}
		}
		if hasContent {
			break
		}
	}
	return data, hasContent
}

func (s *Server) triggerCooldown(decision *router.Decision, statusCode int) {
	pcfg := s.Config.FindProvider(decision.Provider.Name)
	var dur time.Duration
	if pcfg != nil {
		switch {
		case statusCode == 429:
			dur = s.Config.ParseCooldown(decision.Provider.Name, "r429")
		case statusCode == 401 || statusCode == 403:
			dur = s.Config.ParseCooldown(decision.Provider.Name, "r401")
		default:
			dur = s.Config.ParseCooldown(decision.Provider.Name, "r500")
		}
	}
	if dur == 0 {
		switch {
		case statusCode == 429:
			dur = 60 * time.Second
		case statusCode == 401 || statusCode == 403:
			dur = 0
		default:
			dur = 10 * time.Second
		}
	}
	decision.Provider.Pool.TriggerCooldown(decision.Key.ID, statusCode, dur)
}

func (s *Server) triggerCooldownWithCode(decision *router.Decision, statusCode int, dur time.Duration) {
	decision.Provider.Pool.TriggerCooldown(decision.Key.ID, statusCode, dur)
}

func (s *Server) getCooldownDuration(providerName string, statusCode int) time.Duration {
	code := "r500"
	switch {
	case statusCode == 429:
		code = "r429"
	case statusCode == 401 || statusCode == 403:
		code = "r401"
	}
	dur := s.Config.ParseCooldown(providerName, code)
	if dur == 0 {
		switch {
		case statusCode == 429:
			return 60 * time.Second
		case statusCode == 401 || statusCode == 403:
			return 0
		default:
			return 10 * time.Second
		}
	}
	return dur
}

func (s *Server) tryFallback(current *router.Decision, tier, model string, contextTokens int) *router.Decision {
	// Determine the actual model we were using (for auto mode, use the resolved model from the failed decision)
	actualModel := model
	if actualModel == "" && current.Model != "" {
		actualModel = current.Model
	}

	// Step 1: Same model + same provider + different key
	if actualModel != "" {
		p := current.Provider
		if p.Pool.IsHealthy() {
			key, err := p.Pool.Select()
			if err == nil && key.ID != current.Key.ID {
				return &router.Decision{
					Tier:     tier,
					Provider: p,
					Key:      key,
					Model:    actualModel,
					Sticky:   false,
					Reason:   "key_rotation_" + p.Name,
				}
			}
		}
	}

	// Step 2: Same model + different provider
	if actualModel != "" && s.Registry != nil {
		providers := s.Registry.FindProvidersForModel(actualModel)
		for _, p := range providers {
			if p.Name == current.Provider.Name {
				continue
			}
			if !p.Pool.IsHealthy() {
				continue
			}
			key, err := p.Pool.Select()
			if err != nil {
				continue
			}
			resolved := provider.ResolveModelName(actualModel, p.Name, s.Config.ModelMap)
			if resolved == "" {
				resolved = actualModel
			}
			return &router.Decision{
				Tier:     tier,
				Provider: p,
				Key:      key,
				Model:    resolved,
				Sticky:   false,
				Reason:   "same_model_" + p.Name,
			}
		}
	}

	// Step 3: Same tier + different model (try each provider in the chain)
	chain, ok := s.Config.Fallback.Chains[tier]
	if ok {
		for _, provName := range chain {
			if provName == current.Provider.Name {
				continue
			}
			dec, err := s.Router.SelectForProvider(tier, provName)
			if err == nil {
				return dec
			}
		}
	}

	// Step 4: Cross-tier — try higher tier first (better quality), then lower
	if s.Config.Fallback.CrossTier {
		for _, higherTier := range []string{"premium", "coder", "mid"} {
			if router.TierRank(higherTier) <= router.TierRank(tier) {
				continue
			}
			dec, err := s.Router.SelectProviderAndKey(higherTier, "", contextTokens)
			if err == nil {
				log.Info().Str("from_tier", tier).Str("to_tier", higherTier).Msg("fallback tier escalated")
				return dec
			}
		}
		for _, lowerTier := range []string{"mid", "coder", "free"} {
			if router.TierRank(lowerTier) >= router.TierRank(tier) {
				continue
			}
			dec, err := s.Router.SelectProviderAndKey(lowerTier, "", contextTokens)
			if err == nil {
				log.Info().Str("from_tier", tier).Str("to_tier", lowerTier).Msg("fallback tier downgraded")
				return dec
			}
		}
	}

	return nil
}

func isResourceExhaustedError(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "resourceexhausted") ||
		strings.Contains(lower, "worker local total request limit") ||
		strings.Contains(lower, "request limit reached")
}

// isEmptyResponse checks if a non-streaming chat completion response has
// empty content AND no tool_calls. Such responses poison conversation history.
func isEmptyResponse(body []byte) bool {
	var resp struct {
		Choices []struct {
			Message struct {
				Content      *string `json:"content"`
				ToolCalls    *any    `json:"tool_calls"`
				Reasoning    *string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false
	}
	if len(resp.Choices) == 0 {
		return true
	}
	msg := resp.Choices[0].Message
	// Content is empty if it's null or empty string
	contentEmpty := msg.Content == nil || *msg.Content == ""
	toolCallsEmpty := msg.ToolCalls == nil
	return contentEmpty && toolCallsEmpty
}

func isContextLengthError(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "context_length") ||
		strings.Contains(lower, "context length") ||
		strings.Contains(lower, "maximum context") ||
		strings.Contains(lower, "max context") ||
		strings.Contains(lower, "token limit") ||
		strings.Contains(lower, "too many tokens") ||
		strings.Contains(lower, "reduce the length")
}

func isUnknownModelError(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "unknown model") ||
		strings.Contains(lower, "model not found") ||
		strings.Contains(lower, "model does not exist") ||
		strings.Contains(lower, "invalid model") ||
		strings.Contains(lower, "not a valid model") ||
		strings.Contains(lower, "model_code") ||
		strings.Contains(lower, "please check the model code") ||
		strings.Contains(lower, "single tool-calls") ||
		strings.Contains(lower, "tool calls not supported") ||
		strings.Contains(lower, "reasoning_content")
}

func (s *Server) tryContextFallback(current *router.Decision, tier string, contextTokens int) *router.Decision {
	chain, ok := s.Config.Fallback.Chains[tier]
	if ok {
		for _, provName := range chain {
			if provName == current.Provider.Name {
				continue
			}
			p, ok := s.Registry.Get(provName)
			if !ok || !p.Pool.IsHealthy() {
				continue
			}
			key, err := p.Pool.Select()
			if err != nil {
				continue
			}
			tierCfg, hasTier := s.Config.Tiers[tier]
			if hasTier {
				for _, modelID := range tierCfg.Models {
					if s.ModelReg != nil {
						mc := s.ModelReg.GetModel(modelID)
						if mc != nil && mc.ContextWindow > 0 && mc.ContextWindow >= contextTokens {
							if p.HasModel(modelID) {
								resolved := provider.ResolveModelName(modelID, p.Name, s.Config.ModelMap)
								return &router.Decision{
									Tier:     tier,
									Provider: p,
									Key:      key,
									Model:    resolved,
									Sticky:   false,
									Reason:   "context_fallback_" + p.Name,
								}
							}
						}
					}
				}
			}
			// Use the first tier model this provider supports
			if hasTier {
				for _, modelID := range tierCfg.Models {
					if p.HasModel(modelID) {
						resolved := provider.ResolveModelName(modelID, p.Name, s.Config.ModelMap)
						return &router.Decision{
							Tier:     tier,
							Provider: p,
							Key:      key,
							Model:    resolved,
							Sticky:   false,
							Reason:   "context_fallback_" + p.Name,
						}
					}
				}
			}
			// If no tier models matched, still try with any model the provider supports
			dec, err := s.Router.SelectProviderAndKey(tier, "", contextTokens)
			if err == nil && dec.Provider.Name == provName {
				return dec
			}
		}
	}

	higher := router.NextHigherTier(tier)
	if higher != "" {
		dec, err := s.Router.SelectProviderAndKey(higher, "", contextTokens)
		if err == nil {
			log.Info().Str("from_tier", tier).Str("to_tier", higher).Msg("context fallback upgraded tier")
			return dec
		}
	}

	lower := router.NextLowerTier(tier)
	if lower != "" {
		dec, err := s.Router.SelectProviderAndKey(lower, "", contextTokens)
		if err == nil {
			return dec
		}
	}

	return nil
}

func extractPrompt(body map[string]any) string {
	if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
		if last, ok := msgs[len(msgs)-1].(map[string]any); ok {
			if content, ok := last["content"].(string); ok {
				return content
			}
		}
	}
	if prompt, ok := body["prompt"].(string); ok {
		return prompt
	}
	return ""
}

func estimateContextTokens(body map[string]any) int {
	total := 0
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			if msg, ok := m.(map[string]any); ok {
				if content, ok := msg["content"].(string); ok {
					total += len(content) / 4
				}
			}
		}
	}
	return total
}


