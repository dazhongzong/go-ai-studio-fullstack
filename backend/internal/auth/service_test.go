package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"example.com/go-workerpool-http/internal/store"
)

type memoryStore struct {
	users    map[string]store.User
	sessions map[string]store.AuthSession
}

func newMemoryStore() *memoryStore {
	return &memoryStore{users: make(map[string]store.User), sessions: make(map[string]store.AuthSession)}
}

func (m *memoryStore) CreateUser(_ context.Context, user store.User) error {
	for _, existing := range m.users {
		if existing.Username == user.Username || existing.Email == user.Email {
			return store.ErrUserExists
		}
	}
	m.users[user.ID] = user
	return nil
}

func (m *memoryStore) FindUserByLogin(_ context.Context, login string) (store.User, error) {
	for _, user := range m.users {
		if user.Username == login || user.Email == login {
			return user, nil
		}
	}
	return store.User{}, store.ErrUserNotFound
}

func (m *memoryStore) FindUserByID(_ context.Context, id string) (store.User, error) {
	user, ok := m.users[id]
	if !ok {
		return store.User{}, store.ErrUserNotFound
	}
	return user, nil
}

func (m *memoryStore) CreateAuthSession(_ context.Context, session store.AuthSession) error {
	m.sessions[session.ID] = session
	return nil
}

func (m *memoryStore) FindAuthSession(_ context.Context, tokenHash string) (store.AuthSession, error) {
	for _, session := range m.sessions {
		if session.RefreshTokenHash == tokenHash {
			return session, nil
		}
	}
	return store.AuthSession{}, store.ErrAuthSessionNotFound
}

func (m *memoryStore) RevokeAuthSession(_ context.Context, id string) error {
	session, ok := m.sessions[id]
	if !ok {
		return store.ErrAuthSessionNotFound
	}
	session.RevokedAt.Valid = true
	session.RevokedAt.Time = time.Now()
	m.sessions[id] = session
	return nil
}

func TestRegisterLoginRefreshAndLogout(t *testing.T) {
	service, err := New(newMemoryStore(), "01234567890123456789012345678901", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := service.Register(context.Background(), "Alice", "ALICE@example.com", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.User.Username != "alice" || tokens.RefreshToken == "" || tokens.AccessToken == "" {
		t.Fatalf("unexpected registration tokens: %+v", tokens)
	}

	principal, err := service.AuthenticateAccessToken(tokens.AccessToken)
	if err != nil || principal.UserID != tokens.User.ID {
		t.Fatalf("authenticate access token: principal=%+v err=%v", principal, err)
	}

	refreshed, err := service.Refresh(context.Background(), tokens.RefreshToken, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RefreshToken == tokens.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}
	if _, err := service.Refresh(context.Background(), tokens.RefreshToken, "", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("old refresh token should be revoked, got %v", err)
	}
	if err := service.Logout(context.Background(), refreshed.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(context.Background(), refreshed.RefreshToken, "", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("logged out refresh token should be invalid, got %v", err)
	}
}
