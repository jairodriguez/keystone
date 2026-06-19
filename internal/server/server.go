package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/clawdbot/keystone/internal/classify"
	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/economics"
	"github.com/clawdbot/keystone/internal/keypool"
	"github.com/clawdbot/keystone/internal/metrics"
	"github.com/clawdbot/keystone/internal/provider"
	"github.com/clawdbot/keystone/internal/router"
	"github.com/clawdbot/keystone/internal/session"
	"github.com/rs/zerolog/log"
)

type Server struct {
	Config     *config.Config
	Registry   *provider.Registry
	Router     *router.Router
	SessionMgr *session.Manager
	Econ       *economics.Engine
	Classifier classify.Classifier
	API        *apiAdapter
	client     *http.Client
}

type apiAdapter struct {
	GetMode func() string
}

func New(cfg *config.Config, reg *provider.Registry, rt *router.Router, sm *session.Manager, econ *economics.Engine, cls classify.Classifier, modeFn func() string) *Server {
	return &Server{
		Config:     cfg,
		Registry:   reg,
		Router:     rt,
		SessionMgr: sm,
		Econ:       econ,
		Classifier: cls,
		API:        &apiAdapter{GetMode: modeFn},
		client: &http.Client{
			Timeout: 0,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
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

	sessionID := s.SessionMgr.ResolveSessionID(
		r.Header.Get(s.Config.Sessions.Header),
		string(bodyBytes),
	)
	agent := r.Header.Get(s.Config.Sessions.AgentHeader)

	sess := s.SessionMgr.GetOrCreate(sessionID, agent)

	clsResult := s.Classifier.Classify(
		extractPrompt(reqBody),
		sess.ContextEst,
		sess.TurnCount,
	)

	econDecision := s.Econ.Decide(clsResult, sess, agent)

	var decision *router.Decision
	sticky := false

	if econDecision.Sticky && sess.IsStickyEligible() && sess.Provider != "" {
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

	if decision == nil {
		decision, err = s.Router.SelectProviderAndKey(econDecision.Tier, requestedModel)
		if err != nil {
			log.Error().Err(err).Str("tier", econDecision.Tier).Str("model", requestedModel).Msg("all providers exhausted")
			metrics.RequestsTotal.WithLabelValues("none", econDecision.Tier, requestedModel, "503").Inc()
			http.Error(w, "all providers exhausted", http.StatusServiceUnavailable)
			return
		}
		metrics.StickyDecisions.WithLabelValues("new_binding").Inc()
	}

	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
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

		resp, err := s.client.Do(proxyReq)
		if err != nil {
			log.Error().Err(err).Str("provider", decision.Provider.Name).Msg("upstream request failed")
			s.triggerCooldown(decision, 502)
			decision = s.tryFallback(decision, econDecision.Tier, requestedModel)
			if decision == nil {
				metrics.RequestsTotal.WithLabelValues("none", econDecision.Tier, requestedModel, "502").Inc()
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
				return
			}
			continue
		}

		if resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 401 || resp.StatusCode == 403 {
			cooldownDur := s.getCooldownDuration(decision.Provider.Name, resp.StatusCode)
			s.triggerCooldownWithCode(decision, resp.StatusCode, cooldownDur)

			originalProvider := decision.Provider.Name
			decision = s.tryFallback(decision, econDecision.Tier, requestedModel)
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

		w.WriteHeader(resp.StatusCode)

		if isStream {
			s.streamSSE(w, resp.Body)
		} else {
			io.Copy(w, resp.Body)
		}
		resp.Body.Close()

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

func (s *Server) tryFallback(current *router.Decision, tier, model string) *router.Decision {
	lower := router.NextLowerTier(tier)
	if lower != "" && s.Config.Fallback.CrossTier {
		dec, err := s.Router.SelectProviderAndKey(lower, model)
		if err == nil {
			return dec
		}
	}

	chain, ok := s.Config.Fallback.Chains[tier]
	if !ok || len(chain) <= 1 {
		return nil
	}

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
		resolved := provider.ResolveModelName(model, p.Name, s.Config.ModelMap)
		return &router.Decision{
			Tier:     tier,
			Provider: p,
			Key:      key,
			Model:    resolved,
			Sticky:   false,
			Reason:   "fallback_from_" + current.Provider.Name,
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


