// Package auth contains account authentication and refresh-token sessions.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"example.com/go-workerpool-http/internal/store"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidToken       = errors.New("invalid token")
	ErrUserInactive       = errors.New("user is inactive")
	ErrInvalidInput       = errors.New("invalid authentication input")
)

type Principal struct {
	UserID    string
	Role      string
	SessionID string
}

type Tokens struct {
	AccessToken  string     `json:"access_token"`
	RefreshToken string     `json:"refresh_token"`
	TokenType    string     `json:"token_type"`
	ExpiresIn    int64      `json:"expires_in"`
	User         store.User `json:"user"`
}

type Service struct {
	users      store.UserStore
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func New(users store.UserStore, secret string, accessTTL, refreshTTL time.Duration) (*Service, error) {
	secret = strings.TrimSpace(secret)
	if users == nil || len(secret) < 32 {
		return nil, errors.New("auth: user store and a JWT secret of at least 32 bytes are required")
	}
	if accessTTL <= 0 {
		accessTTL = 15 * time.Minute
	}
	if refreshTTL <= 0 {
		refreshTTL = 30 * 24 * time.Hour
	}
	return &Service{
		users:      users,
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}, nil
}

func (s *Service) Register(ctx context.Context, username, email, password string) (Tokens, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	email = strings.ToLower(strings.TrimSpace(email))
	if len(username) < 3 || len(username) > 64 || strings.ContainsAny(username, " \t\r\n") {
		return Tokens{}, fmt.Errorf("%w: username must be 3..64 characters without spaces", ErrInvalidInput)
	}
	if !strings.Contains(email, "@") || len(email) > 255 {
		return Tokens{}, fmt.Errorf("%w: invalid email", ErrInvalidInput)
	}
	if len(password) < 8 || len(password) > 128 {
		return Tokens{}, fmt.Errorf("%w: password must be 8..128 characters", ErrInvalidInput)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return Tokens{}, fmt.Errorf("hash password: %w", err)
	}
	user := store.User{
		ID:           newID(),
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		Role:         "user",
		Status:       "active",
		CreatedAt:    time.Now(),
	}
	if err := s.users.CreateUser(ctx, user); err != nil {
		return Tokens{}, err
	}
	return s.issueTokens(ctx, user, "", "")
}

func (s *Service) Login(ctx context.Context, login, password, userAgent, ipAddress string) (Tokens, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	user, err := s.users.FindUserByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			return Tokens{}, ErrInvalidCredentials
		}
		return Tokens{}, err
	}
	if user.Status != "active" || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		return Tokens{}, ErrInvalidCredentials
	}
	return s.issueTokens(ctx, user, userAgent, ipAddress)
}

func (s *Service) Refresh(ctx context.Context, refreshToken, userAgent, ipAddress string) (Tokens, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return Tokens{}, ErrInvalidToken
	}
	session, err := s.users.FindAuthSession(ctx, hashToken(refreshToken))
	if err != nil {
		return Tokens{}, ErrInvalidToken
	}
	if session.RevokedAt.Valid || !session.ExpiresAt.After(time.Now()) {
		return Tokens{}, ErrInvalidToken
	}
	user, err := s.users.FindUserByID(ctx, session.UserID)
	if err != nil {
		return Tokens{}, err
	}
	if user.Status != "active" {
		return Tokens{}, ErrUserInactive
	}
	// Rotate refresh tokens. Reusing an old token after rotation fails because
	// the old database session is revoked.
	if err := s.users.RevokeAuthSession(ctx, session.ID); err != nil {
		return Tokens{}, err
	}
	return s.issueTokens(ctx, user, userAgent, ipAddress)
}

func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil
	}
	session, err := s.users.FindAuthSession(ctx, hashToken(refreshToken))
	if errors.Is(err, store.ErrAuthSessionNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.users.RevokeAuthSession(ctx, session.ID)
}

func (s *Service) AuthenticateAccessToken(token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, ErrInvalidToken
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	var claims struct {
		Subject   string `json:"sub"`
		Role      string `json:"role"`
		SessionID string `json:"sid"`
		ExpiresAt int64  `json:"exp"`
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "HS256" || header.Type != "JWT" {
		return Principal{}, ErrInvalidToken
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(claimsBytes, &claims) != nil {
		return Principal{}, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	expected, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(expected, mac.Sum(nil)) || claims.Subject == "" || claims.ExpiresAt <= time.Now().Unix() {
		return Principal{}, ErrInvalidToken
	}
	return Principal{UserID: claims.Subject, Role: claims.Role, SessionID: claims.SessionID}, nil
}

func (s *Service) User(ctx context.Context, principal Principal) (store.User, error) {
	return s.users.FindUserByID(ctx, principal.UserID)
}

func (s *Service) issueTokens(ctx context.Context, user store.User, userAgent, ipAddress string) (Tokens, error) {
	refreshToken := newSecret()
	session := store.AuthSession{
		ID:               newID(),
		UserID:           user.ID,
		RefreshTokenHash: hashToken(refreshToken),
		UserAgent:        truncate(userAgent, 512),
		IPAddress:        truncate(ipAddress, 64),
		ExpiresAt:        time.Now().Add(s.refreshTTL),
	}
	if err := s.users.CreateAuthSession(ctx, session); err != nil {
		return Tokens{}, err
	}
	accessToken, err := s.signAccessToken(user.ID, user.Role, session.ID)
	if err != nil {
		return Tokens{}, err
	}
	user.PasswordHash = ""
	return Tokens{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int64(s.accessTTL / time.Second),
		User:         user,
	}, nil
}

func (s *Service) signAccessToken(userID, role, sessionID string) (string, error) {
	encode := func(value any) (string, error) {
		body, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(body), nil
	}
	header, err := encode(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims, err := encode(map[string]any{
		"sub":  userID,
		"role": role,
		"sid":  sessionID,
		"iat":  now.Unix(),
		"exp":  now.Add(s.accessTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(header + "." + claims))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + claims + "." + signature, nil
}

func newID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return hex.EncodeToString(value[:4]) + "-" + hex.EncodeToString(value[4:6]) + "-" +
		hex.EncodeToString(value[6:8]) + "-" + hex.EncodeToString(value[8:10]) + "-" + hex.EncodeToString(value[10:])
}

func newSecret() string {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(value[:])
}

func hashToken(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
