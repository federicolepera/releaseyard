package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultSessionKeyPrefix = "releaseyard:dashboard:"

// RedisSessionStore persists sessions and login transactions in Redis so that
// dashboard replicas share them and a rollout does not invalidate sessions.
type RedisSessionStore struct {
	client *redis.Client
	prefix string
}

func NewRedisSessionStore(client *redis.Client, keyPrefix string) *RedisSessionStore {
	if keyPrefix == "" {
		keyPrefix = defaultSessionKeyPrefix
	}
	return &RedisSessionStore{client: client, prefix: keyPrefix}
}

func (s *RedisSessionStore) PutSession(ctx context.Context, id string, session dashboardSession, ttl time.Duration) error {
	key := s.key("session", id)
	if ttl <= 0 {
		return s.client.Del(ctx, key).Err()
	}
	data, err := json.Marshal(newStoredSession(session))
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	return s.client.Set(ctx, key, data, ttl).Err()
}

func (s *RedisSessionStore) GetSession(ctx context.Context, id string) (dashboardSession, bool, error) {
	data, err := s.client.Get(ctx, s.key("session", id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return dashboardSession{}, false, nil
	}
	if err != nil {
		return dashboardSession{}, false, err
	}
	var stored storedSession
	if err := json.Unmarshal(data, &stored); err != nil {
		return dashboardSession{}, false, fmt.Errorf("decode session: %w", err)
	}
	return stored.session(), true, nil
}

func (s *RedisSessionStore) DeleteSession(ctx context.Context, id string) error {
	return s.client.Del(ctx, s.key("session", id)).Err()
}

func (s *RedisSessionStore) PutTransaction(ctx context.Context, state string, transaction loginTransaction, ttl time.Duration) error {
	key := s.key("transaction", state)
	if ttl <= 0 {
		return s.client.Del(ctx, key).Err()
	}
	data, err := json.Marshal(newStoredTransaction(transaction))
	if err != nil {
		return fmt.Errorf("encode login transaction: %w", err)
	}
	return s.client.Set(ctx, key, data, ttl).Err()
}

func (s *RedisSessionStore) TakeTransaction(ctx context.Context, state string) (loginTransaction, bool, error) {
	const take = `local value = redis.call("GET", KEYS[1])
if value then
  redis.call("DEL", KEYS[1])
end
return value`
	data, err := s.client.Eval(ctx, take, []string{s.key("transaction", state)}).Text()
	if errors.Is(err, redis.Nil) {
		return loginTransaction{}, false, nil
	}
	if err != nil {
		return loginTransaction{}, false, err
	}
	var stored storedTransaction
	if err := json.Unmarshal([]byte(data), &stored); err != nil {
		return loginTransaction{}, false, fmt.Errorf("decode login transaction: %w", err)
	}
	return stored.transaction(), true, nil
}

func (s *RedisSessionStore) key(kind, id string) string {
	return s.prefix + kind + ":" + id
}

type storedSession struct {
	Issuer       string    `json:"issuer,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	Username     string    `json:"username,omitempty"`
	Groups       []string  `json:"groups,omitempty"`
	CSRFToken    string    `json:"csrfToken,omitempty"`
	IDToken      string    `json:"idToken,omitempty"`
	LastActivity time.Time `json:"lastActivity"`
	Expires      time.Time `json:"expires"`
	AuthMethod   string    `json:"authMethod,omitempty"`
}

func newStoredSession(session dashboardSession) storedSession {
	return storedSession{
		Issuer:       session.issuer,
		Subject:      session.subject,
		Username:     session.username,
		Groups:       append([]string(nil), session.groups...),
		CSRFToken:    session.csrfToken,
		IDToken:      session.idToken,
		LastActivity: session.lastActivity,
		Expires:      session.expires,
		AuthMethod:   string(session.authMethod),
	}
}

func (s storedSession) session() dashboardSession {
	return dashboardSession{
		issuer:       s.Issuer,
		subject:      s.Subject,
		username:     s.Username,
		groups:       append([]string(nil), s.Groups...),
		csrfToken:    s.CSRFToken,
		idToken:      s.IDToken,
		lastActivity: s.LastActivity,
		expires:      s.Expires,
		authMethod:   authMethod(s.AuthMethod),
	}
}

type storedTransaction struct {
	State    string    `json:"state"`
	Binding  string    `json:"binding"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	Expires  time.Time `json:"expires"`
}

func newStoredTransaction(transaction loginTransaction) storedTransaction {
	return storedTransaction{
		State:    transaction.state,
		Binding:  transaction.binding,
		Nonce:    transaction.nonce,
		Verifier: transaction.verifier,
		Expires:  transaction.expires,
	}
}

func (s storedTransaction) transaction() loginTransaction {
	return loginTransaction{
		state:    s.State,
		binding:  s.Binding,
		nonce:    s.Nonce,
		verifier: s.Verifier,
		expires:  s.Expires,
	}
}
