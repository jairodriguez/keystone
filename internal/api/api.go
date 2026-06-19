package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/clawdbot/keystone/internal/classify"
	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/keypool"
	"github.com/clawdbot/keystone/internal/provider"
	"github.com/clawdbot/keystone/internal/session"
	"github.com/rs/zerolog/log"
)

type Server struct {
	Config     *config.Config
	Registry   *provider.Registry
	SessionMgr *session.Manager
	Classifier *classify.RuleClassifier
	Mode       string
	mu         sync.RWMutex
}

func New(cfg *config.Config, reg *provider.Registry, sm *session.Manager) *Server {
	return &Server{
		Config:     cfg,
		Registry:   reg,
		SessionMgr: sm,
		Classifier: &classify.RuleClassifier{},
		Mode:       cfg.Classifier.Mode,
	}
}

func (s *Server) Register(mux *http.ServeMux, basePath string) {
	mux.HandleFunc(basePath+"/mode", s.handleMode)
	mux.HandleFunc(basePath+"/health", s.handleHealth)
	mux.HandleFunc(basePath+"/stats", s.handleStats)
	mux.HandleFunc(basePath+"/sessions", s.handleSessions)
	mux.HandleFunc(basePath+"/session/", s.handleSessionAction)
}

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	currentMode := s.Mode
	s.mu.RUnlock()

	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]string{"mode": currentMode})
	case http.MethodPost:
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.Mode != "aggressive" && req.Mode != "normal" && req.Mode != "simple" {
			http.Error(w, "invalid mode: must be aggressive, normal, or simple", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.Mode = req.Mode
		s.mu.Unlock()
		log.Info().Str("mode", req.Mode).Msg("Routing mode changed")
		json.NewEncoder(w).Encode(map[string]string{"mode": s.Mode, "status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"mode":       s.getMode(),
		"providers":  make(map[string]interface{}),
	}

	providers := s.Registry.All()
	for _, p := range providers {
		keys := p.Pool.AllKeys()
		healthy := 0
		cooling := 0
		dead := 0
		for _, k := range keys {
			switch k.State {
			case keypool.StateHealthy:
				healthy++
			case keypool.StateCooling:
				cooling++
			case keypool.StateDead:
				dead++
			}
		}
		response["providers"].(map[string]interface{})[p.Name] = map[string]interface{}{
			"total_keys": len(keys),
			"healthy":    healthy,
			"cooling":    cooling,
			"dead":       dead,
			"models":     p.Models,
		}
	}

	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	sessions := s.SessionMgr.Store.All()
	tierDist := make(map[string]int)
	for _, s := range sessions {
		tierDist[s.Tier]++
	}

	response := map[string]interface{}{
		"active_sessions":    len(sessions),
		"tier_distribution":  tierDist,
		"mode":               s.getMode(),
	}

	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.SessionMgr.Store.All()
	var out []map[string]interface{}
	for _, s := range sessions {
		keyID := "none"
		if s.Key != nil {
			keyID = s.Key.ID
		}
		out = append(out, map[string]interface{}{
			"id":            s.ID,
			"agent":         s.Agent,
			"tier":          s.Tier,
			"provider":      s.Provider,
			"model":         s.Model,
			"key_id":        keyID,
			"turns":         s.TurnCount,
			"context_est":   s.ContextEst,
			"cache_hits":    s.CacheHits,
			"last_class":    s.LastClass,
			"created_at":    s.CreatedAt.Format(time.RFC3339),
			"last_used":     s.LastUsed.Format(time.RFC3339),
		})
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleSessionAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, s.Config.API.BasePath+"/session/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	sessionID := parts[0]
	action := parts[1]

	if action == "unbind" && r.Method == http.MethodPost {
		s.SessionMgr.Store.Unbind(sessionID)
		json.NewEncoder(w).Encode(map[string]string{"status": "unbound"})
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func (s *Server) getMode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Mode
}

func (s *Server) GetMode() string {
	return s.getMode()
}
