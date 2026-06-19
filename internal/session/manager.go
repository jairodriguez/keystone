package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/clawdbot/keystone/internal/keypool"
)

type Session struct {
	ID            string
	Agent         string
	Key           *keypool.Key
	Provider      string
	Model         string
	Tier          string
	TurnCount     int
	ContextEst    int
	CreatedAt     time.Time
	LastUsed      time.Time
	CacheHits     int
	LastClass     string
}

func (s *Session) IsStickyEligible() bool {
	return s.Key != nil && s.Key.State == keypool.StateHealthy
}

type Store interface {
	Get(sessionID string) *Session
	GetOrCreate(sessionID string, agent string) *Session
	Bind(sessionID string, key *keypool.Key, provider, model, tier string)
	Unbind(sessionID string)
	IncrementTurn(sessionID string, contextEst int)
	EvictExpired(ttl time.Duration)
	ActiveCount() int
	All() []*Session
}

type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: make(map[string]*Session)}
}

func (m *MemoryStore) Get(sessionID string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[sessionID]
}

func (m *MemoryStore) GetOrCreate(sessionID string, agent string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		s = &Session{
			ID:        sessionID,
			Agent:     agent,
			CreatedAt: time.Now(),
		}
		m.sessions[sessionID] = s
	}
	s.LastUsed = time.Now()
	return s
}

func (m *MemoryStore) Bind(sessionID string, key *keypool.Key, provider, model, tier string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		s = &Session{
			ID:        sessionID,
			CreatedAt: time.Now(),
		}
		m.sessions[sessionID] = s
	}
	s.Key = key
	s.Provider = provider
	s.Model = model
	s.Tier = tier
	s.LastUsed = time.Now()
}

func (m *MemoryStore) Unbind(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sessionID]; ok {
		s.Key = nil
		s.Provider = ""
		s.Model = ""
		s.Tier = ""
	}
}

func (m *MemoryStore) IncrementTurn(sessionID string, contextEst int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sessionID]; ok {
		s.TurnCount++
		s.ContextEst = contextEst
		s.LastUsed = time.Now()
		if s.TurnCount > 1 {
			s.CacheHits++
		}
	}
}

func (m *MemoryStore) EvictExpired(ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, s := range m.sessions {
		if now.Sub(s.LastUsed) > ttl {
			delete(m.sessions, id)
		}
	}
}

func (m *MemoryStore) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *MemoryStore) All() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

type Manager struct {
	Store        Store
	TTL          time.Duration
	HeaderName   string
	AutoDerive   bool
	AgentHeader  string
}

func NewManager(store Store, ttl time.Duration, headerName string, autoDerive bool, agentHeader string) *Manager {
	return &Manager{
		Store:       store,
		TTL:         ttl,
		HeaderName:  headerName,
		AutoDerive:  autoDerive,
		AgentHeader: agentHeader,
	}
}

func (m *Manager) ResolveSessionID(headerValue, fallback string) string {
	if headerValue != "" {
		return headerValue
	}
	if m.AutoDerive && fallback != "" {
		h := sha256.Sum256([]byte(fallback))
		return hex.EncodeToString(h[:8])
	}
	return "default"
}

func (m *Manager) StartEvictionLoop() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			m.Store.EvictExpired(m.TTL)
		}
	}()
}

func (m *Manager) Get(sessionID string) *Session {
	return m.Store.Get(sessionID)
}

func (m *Manager) GetOrCreate(sessionID, agent string) *Session {
	return m.Store.GetOrCreate(sessionID, agent)
}

func (m *Manager) Bind(sessionID string, key *keypool.Key, provider, model, tier string) {
	m.Store.Bind(sessionID, key, provider, model, tier)
}

func (m *Manager) IncrementTurn(sessionID string, contextEst int) {
	m.Store.IncrementTurn(sessionID, contextEst)
}

func (m *Manager) FormatSummary() string {
	sessions := m.Store.All()
	if len(sessions) == 0 {
		return "no active sessions"
	}
	out := fmt.Sprintf("%d active sessions:\n", len(sessions))
	for _, s := range sessions {
		keyID := "none"
		if s.Key != nil {
			keyID = s.Key.ID
		}
		out += fmt.Sprintf("  %s | agent=%s tier=%s provider=%s key=%s turns=%d ctx=%d\n",
			s.ID[:8], s.Agent, s.Tier, s.Provider, keyID, s.TurnCount, s.ContextEst)
	}
	return out
}
