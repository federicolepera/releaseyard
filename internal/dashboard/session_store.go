package dashboard

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	maxLoginTransactions = 4096
	maxSessions          = 4096
)

var errSessionCapacity = errors.New("session capacity reached")

// SessionStore persists browser sessions and in-flight OIDC login
// transactions. A shared implementation lets multiple dashboard replicas serve
// the same browser and keeps sessions across rollouts.
type SessionStore interface {
	PutSession(ctx context.Context, id string, session dashboardSession, ttl time.Duration) error
	GetSession(ctx context.Context, id string) (dashboardSession, bool, error)
	DeleteSession(ctx context.Context, id string) error

	PutTransaction(ctx context.Context, state string, transaction loginTransaction, ttl time.Duration) error
	TakeTransaction(ctx context.Context, state string) (loginTransaction, bool, error)
}

type memorySessionEntry struct {
	session dashboardSession
	expires time.Time
}

type memoryTransactionEntry struct {
	transaction loginTransaction
	expires     time.Time
}

type memorySessionStore struct {
	mu           sync.Mutex
	sessions     map[string]memorySessionEntry
	transactions map[string]memoryTransactionEntry
	now          func() time.Time
}

func newMemorySessionStore() *memorySessionStore {
	return &memorySessionStore{
		sessions:     make(map[string]memorySessionEntry),
		transactions: make(map[string]memoryTransactionEntry),
		now:          time.Now,
	}
}

func (s *memorySessionStore) PutSession(_ context.Context, id string, session dashboardSession, ttl time.Duration) error {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if _, exists := s.sessions[id]; !exists && len(s.sessions) >= maxSessions {
		return errSessionCapacity
	}
	if ttl <= 0 {
		delete(s.sessions, id)
		return nil
	}
	s.sessions[id] = memorySessionEntry{session: cloneSession(session), expires: now.Add(ttl)}
	return nil
}

func (s *memorySessionStore) GetSession(_ context.Context, id string) (dashboardSession, bool, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.sessions[id]
	if !ok || !now.Before(entry.expires) {
		delete(s.sessions, id)
		return dashboardSession{}, false, nil
	}
	return cloneSession(entry.session), true, nil
}

func (s *memorySessionStore) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

func (s *memorySessionStore) PutTransaction(_ context.Context, state string, transaction loginTransaction, ttl time.Duration) error {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if _, exists := s.transactions[state]; !exists && len(s.transactions) >= maxLoginTransactions {
		return errSessionCapacity
	}
	if ttl <= 0 {
		delete(s.transactions, state)
		return nil
	}
	s.transactions[state] = memoryTransactionEntry{transaction: transaction, expires: now.Add(ttl)}
	return nil
}

func (s *memorySessionStore) TakeTransaction(_ context.Context, state string) (loginTransaction, bool, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.transactions[state]
	delete(s.transactions, state)
	if !ok || !now.Before(entry.expires) {
		return loginTransaction{}, false, nil
	}
	return entry.transaction, true, nil
}

func (s *memorySessionStore) pruneLocked(now time.Time) {
	for id, entry := range s.sessions {
		if !now.Before(entry.expires) {
			delete(s.sessions, id)
		}
	}
	for state, entry := range s.transactions {
		if !now.Before(entry.expires) {
			delete(s.transactions, state)
		}
	}
}

func cloneSession(session dashboardSession) dashboardSession {
	session.groups = append([]string(nil), session.groups...)
	return session
}
