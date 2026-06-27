package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/keypool"
	"github.com/clawdbot/keystone/internal/session"
	"github.com/rs/zerolog/log"
)

type ZenStickyProxy struct {
	targetURL  string
	pool       *keypool.Pool
	sessionMgr *session.Manager
	cfg        *config.Config
	client     *http.Client
}

func NewZenStickyProxy(targetURL string, pool *keypool.Pool, sm *session.Manager, cfg *config.Config) *ZenStickyProxy {
	return &ZenStickyProxy{
		targetURL:  targetURL,
		pool:       pool,
		sessionMgr: sm,
		cfg:        cfg,
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: cfg.Server.ProxyTimeout,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (z *ZenStickyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	sessionID := z.sessionMgr.ResolveSessionID(
		r.Header.Get(z.cfg.Sessions.Header),
		string(bodyBytes),
	)

	sess := z.sessionMgr.GetOrCreate(sessionID, "")

	maxRetries := 3

	for attempt := 0; attempt < maxRetries; attempt++ {
		key, err := z.selectKey(sess)
		if err != nil {
			log.Error().Str("session", sessionID).Msg("all zen keys exhausted")
			http.Error(w, "all keys exhausted", http.StatusServiceUnavailable)
			return
		}
			proxyReq, err := http.NewRequest(http.MethodPost, z.targetURL+"/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		proxyReq.Header.Set("Authorization", "Bearer "+key.Value)
		proxyReq.Header.Set("Content-Type", "application/json")

		resp, err := z.client.Do(proxyReq)
		if err != nil {
			log.Warn().Err(err).Str("key", key.ID).Int("attempt", attempt+1).Msg("zen proxy upstream failed")
			if attempt < maxRetries-1 {
				z.pool.TriggerCooldown(key.ID, http.StatusBadGateway, 10*time.Second)
				sess.Key = nil
				continue
			}
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}

		if resp.StatusCode == 429 || resp.StatusCode == 502 || resp.StatusCode == 503 {
			resp.Body.Close()
			z.pool.TriggerCooldown(key.ID, resp.StatusCode, 60*time.Second)
			sess.Key = nil
			log.Warn().Str("key", key.ID).Int("status", resp.StatusCode).Int("attempt", attempt+1).Msg("zen proxy retryable status")
			if attempt < maxRetries-1 {
				continue
			}
			http.Error(w, "all keys rate limited", http.StatusServiceUnavailable)
			return
		}

		if resp.StatusCode == 400 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bodyStr := string(body)
			if isContextLengthError(bodyStr) || isResourceExhaustedError(bodyStr) {
				log.Warn().Str("key", key.ID).Int("attempt", attempt+1).Msg("zen proxy retryable 400")
				z.pool.TriggerCooldown(key.ID, 400, 30*time.Second)
				sess.Key = nil
				if attempt < maxRetries-1 {
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			z.pool.TriggerCooldown(key.ID, resp.StatusCode, 60*time.Second)
			sess.Key = nil
			log.Warn().Str("key", key.ID).Int("status", resp.StatusCode).Int("attempt", attempt+1).Msg("zen proxy non-2xx")
			return
		}

		sess.Key = key
		sess.Model = requestedModel
		sess.Provider = "zen"
		sess.Tier = "zen_proxy"

		for k, vv := range resp.Header {
			w.Header()[k] = vv
		}
		w.Header().Set("x-keystone-provider", "zen")
		w.Header().Set("x-keystone-key", key.ID)
		w.Header().Set("x-keystone-session", sessionID)
		w.Header().Set("x-keystone-attempt", strconv.Itoa(attempt+1))

		isStream := false
		if s, ok := reqBody["stream"].(bool); ok && s {
			isStream = true
		}

		w.WriteHeader(resp.StatusCode)

		if isStream {
			z.streamSSE(w, resp.Body)
		} else {
			io.Copy(w, resp.Body)
		}
		resp.Body.Close()

		z.sessionMgr.IncrementTurn(sessionID, estimateContextTokens(reqBody))

		log.Info().
			Str("session", sessionID).
			Str("key", key.ID).
			Str("model", requestedModel).
			Int("status", resp.StatusCode).
			Int("attempt", attempt+1).
			Msg("zen proxy request completed")

		return
	}

	http.Error(w, "all retries exhausted", http.StatusBadGateway)
}

func (z *ZenStickyProxy) selectKey(sess *session.Session) (*keypool.Key, error) {
	if sess.Key != nil && sess.Key.State == keypool.StateHealthy {
		key, err := z.pool.SelectSpecific(sess.Key.ID)
		if err == nil && key.State == keypool.StateHealthy {
			return key, nil
		}
	}
	key, err := z.pool.Select()
	if err != nil {
		return nil, err
	}
	return key, nil
}

func (z *ZenStickyProxy) streamSSE(w http.ResponseWriter, body io.ReadCloser) {
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
				log.Error().Err(err).Msg("zen proxy SSE stream error")
			}
			return
		}
	}
}
