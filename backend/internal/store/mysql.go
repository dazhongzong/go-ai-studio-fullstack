package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// User is an application account. Passwords are never stored here; only a
// bcrypt password hash is persisted by CreateUser.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
}

// AuthSession represents one refresh-token session, normally one browser or
// device. The raw refresh token is never stored in MySQL.
type AuthSession struct {
	ID               string
	UserID           string
	RefreshTokenHash string
	UserAgent        string
	IPAddress        string
	ExpiresAt        time.Time
	RevokedAt        sql.NullTime
}

// Message is the persisted part of a conversation.
type Message struct {
	Role         string
	Content      string
	TurnID       int64
	MessageOrder int
}

type Conversation struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MessageCount int       `json:"message_count"`
}

var ErrConversationNotFound = errors.New("conversation not found")
var ErrConversationBusy = errors.New("conversation is busy")
var ErrUserNotFound = errors.New("user not found")
var ErrUserExists = errors.New("user already exists")
var ErrAuthSessionNotFound = errors.New("auth session not found")

// ConversationStore persists conversation history independently of an API
// instance. That makes history available when a load balancer sends the next
// request to a different backend.
type ConversationStore interface {
	EnsureConversation(context.Context, string, string, string) error
	LoadMessages(context.Context, string, string, int) ([]Message, error)
	SaveMessage(context.Context, string, string, Message, string, string) error
	ListConversations(context.Context, string, int) ([]Conversation, error)
	DeleteConversation(context.Context, string, string) error
}

// OrderedConversationStore adds cross-process turn reservation and ordered
// message persistence to ConversationStore. The optional interface keeps the
// in-memory test store and stateless mock mode backward compatible.
type OrderedConversationStore interface {
	AcquireConversationTurn(context.Context, string, string, string, string) (int64, error)
	ReleaseConversationTurn(context.Context, string, string, string) error
	SaveMessageAtTurn(context.Context, string, string, Message, string, string, int64, int) error
}

// UserStore is the persistence boundary used by the authentication service.
// Keeping it separate from ConversationStore makes authentication testable and
// avoids coupling HTTP handlers to a concrete database implementation.
type UserStore interface {
	CreateUser(context.Context, User) error
	FindUserByLogin(context.Context, string) (User, error)
	FindUserByID(context.Context, string) (User, error)
	CreateAuthSession(context.Context, AuthSession) error
	FindAuthSession(context.Context, string) (AuthSession, error)
	RevokeAuthSession(context.Context, string) error
}

type MySQLStore struct {
	db *sql.DB
}

func OpenMySQL(ctx context.Context, dsn string) (*MySQLStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	store := &MySQLStore{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *MySQLStore) Close() error {
	return s.db.Close()
}

func (s *MySQLStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *MySQLStore) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id CHAR(36) NOT NULL PRIMARY KEY,
			username VARCHAR(64) NOT NULL UNIQUE,
			email VARCHAR(255) NOT NULL UNIQUE,
			password_hash VARCHAR(255) NOT NULL,
			role VARCHAR(16) NOT NULL DEFAULT 'user',
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS auth_sessions (
			id CHAR(36) NOT NULL PRIMARY KEY,
			user_id CHAR(36) NOT NULL,
			refresh_token_hash CHAR(64) NOT NULL UNIQUE,
			user_agent VARCHAR(512) NOT NULL DEFAULT '',
			ip_address VARCHAR(64) NOT NULL DEFAULT '',
			expires_at TIMESTAMP(6) NOT NULL,
			last_seen_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			revoked_at TIMESTAMP(6) NULL,
			created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			INDEX idx_auth_sessions_user_id (user_id),
			CONSTRAINT fk_auth_sessions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS conversations (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			owner_id VARCHAR(64) NOT NULL DEFAULT '',
			title VARCHAR(255) NOT NULL DEFAULT '',
			next_turn BIGINT UNSIGNED NOT NULL DEFAULT 1,
			active_task_id VARCHAR(64) NULL,
			created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			INDEX idx_conversations_owner_updated (owner_id, updated_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS messages (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			conversation_id VARCHAR(64) NOT NULL,
			role VARCHAR(16) NOT NULL,
			content MEDIUMTEXT NOT NULL,
			turn_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
			message_order TINYINT UNSIGNED NOT NULL DEFAULT 0,
			request_id VARCHAR(64) NOT NULL DEFAULT '',
			model VARCHAR(128) NOT NULL DEFAULT '',
			created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			INDEX idx_messages_conversation_id_id (conversation_id, id),
			CONSTRAINT fk_messages_conversation
				FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate mysql: %w", err)
		}
	}
	var ownerColumn int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'conversations' AND COLUMN_NAME = 'owner_id'`).Scan(&ownerColumn); err != nil {
		return fmt.Errorf("check conversation owner migration: %w", err)
	}
	if ownerColumn == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE conversations ADD COLUMN owner_id VARCHAR(64) NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add conversation owner column: %w", err)
		}
	}
	if err := s.ensureColumn(ctx, "conversations", "next_turn", `ALTER TABLE conversations ADD COLUMN next_turn BIGINT UNSIGNED NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "conversations", "active_task_id", `ALTER TABLE conversations ADD COLUMN active_task_id VARCHAR(64) NULL`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "messages", "turn_id", `ALTER TABLE messages ADD COLUMN turn_id BIGINT UNSIGNED NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "messages", "message_order", `ALTER TABLE messages ADD COLUMN message_order TINYINT UNSIGNED NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return nil
}

func (s *MySQLStore) ensureColumn(ctx context.Context, table, column, alter string) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, table, column).Scan(&count); err != nil {
		return fmt.Errorf("check %s.%s migration: %w", table, column, err)
	}
	if count == 0 {
		if _, err := s.db.ExecContext(ctx, alter); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, column, err)
		}
	}
	return nil
}

func (s *MySQLStore) AcquireConversationTurn(ctx context.Context, ownerID, conversationID, taskID, title string) (int64, error) {
	// A deadlock or a short lock wait can happen when two API instances
	// reserve the same conversation at the same time. The transaction is
	// deliberately small, so retrying it is safe. The inference request is
	// never inside this transaction.
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		turn, err := s.acquireConversationTurnOnce(ctx, ownerID, conversationID, taskID, title)
		if err == nil || !isRetryableLockError(err) || attempt == maxAttempts-1 {
			return turn, err
		}
		if err := waitForRetry(ctx, time.Duration(attempt+1)*25*time.Millisecond); err != nil {
			return 0, err
		}
	}
	return 0, ctx.Err()
}

func (s *MySQLStore) acquireConversationTurnOnce(ctx context.Context, ownerID, conversationID, taskID, title string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin acquire conversation turn: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Materialize the parent row before locking it. INSERT ... ON DUPLICATE
	// KEY UPDATE serializes concurrent first-use requests for the same ID and
	// avoids the SELECT-miss-then-INSERT race caused by a gap lock.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (id, owner_id, title, next_turn, active_task_id)
		VALUES (?, ?, ?, 1, NULL)
		ON DUPLICATE KEY UPDATE id = id`, conversationID, ownerID, title); err != nil {
		return 0, fmt.Errorf("materialize conversation: %w", err)
	}

	var existingOwner, activeTask string
	var nextTurn int64
	if err := tx.QueryRowContext(ctx, `
		SELECT owner_id, next_turn, COALESCE(active_task_id, '')
		FROM conversations WHERE id = ? FOR UPDATE`, conversationID).
		Scan(&existingOwner, &nextTurn, &activeTask); err != nil {
		return 0, fmt.Errorf("lock conversation: %w", err)
	}
	if existingOwner != ownerID {
		return 0, errors.New("conversation belongs to another client")
	}
	if activeTask != "" {
		return 0, ErrConversationBusy
	}

	if nextTurn < 1 {
		nextTurn = 1
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET active_task_id = ?, next_turn = ?, updated_at = CURRENT_TIMESTAMP(6)
		WHERE id = ? AND owner_id = ?`, taskID, nextTurn+1, conversationID, ownerID); err != nil {
		return 0, fmt.Errorf("reserve conversation turn: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit conversation turn: %w", err)
	}
	return nextTurn, nil
}

func isRetryableLockError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	return mysqlErr.Number == 1205 || mysqlErr.Number == 1213
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *MySQLStore) ReleaseConversationTurn(ctx context.Context, ownerID, conversationID, taskID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE conversations
		SET active_task_id = NULL, updated_at = CURRENT_TIMESTAMP(6)
		WHERE id = ? AND owner_id = ? AND active_task_id = ?`, conversationID, ownerID, taskID)
	if err != nil {
		return fmt.Errorf("release conversation turn: %w", err)
	}
	return nil
}

func (s *MySQLStore) CreateUser(ctx context.Context, user User) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, username, email, password_hash, role, status)
		VALUES (?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, user.Email, user.PasswordHash, user.Role, user.Status)
	if err == nil {
		return nil
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return ErrUserExists
	}
	return fmt.Errorf("create user: %w", err)
}

func (s *MySQLStore) FindUserByLogin(ctx context.Context, login string) (User, error) {
	var user User
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, email, password_hash, role, status, created_at
		FROM users
		WHERE username = ? OR email = ?
		LIMIT 1`, login, login).Scan(
		&user.ID, &user.Username, &user.Email, &user.PasswordHash,
		&user.Role, &user.Status, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user: %w", err)
	}
	return user, nil
}

func (s *MySQLStore) FindUserByID(ctx context.Context, id string) (User, error) {
	var user User
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, email, password_hash, role, status, created_at
		FROM users WHERE id = ? LIMIT 1`, id).Scan(
		&user.ID, &user.Username, &user.Email, &user.PasswordHash,
		&user.Role, &user.Status, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user by id: %w", err)
	}
	return user, nil
}

func (s *MySQLStore) CreateAuthSession(ctx context.Context, session AuthSession) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_sessions
		(id, user_id, refresh_token_hash, user_agent, ip_address, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		session.ID, session.UserID, session.RefreshTokenHash,
		session.UserAgent, session.IPAddress, session.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create auth session: %w", err)
	}
	return nil
}

func (s *MySQLStore) FindAuthSession(ctx context.Context, tokenHash string) (AuthSession, error) {
	var session AuthSession
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, refresh_token_hash, user_agent, ip_address, expires_at, revoked_at
		FROM auth_sessions
		WHERE refresh_token_hash = ?
		LIMIT 1`, tokenHash).Scan(
		&session.ID, &session.UserID, &session.RefreshTokenHash,
		&session.UserAgent, &session.IPAddress, &session.ExpiresAt, &session.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthSession{}, ErrAuthSessionNotFound
	}
	if err != nil {
		return AuthSession{}, fmt.Errorf("find auth session: %w", err)
	}
	return session, nil
}

func (s *MySQLStore) RevokeAuthSession(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE auth_sessions
		SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP(6))
		WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke auth session: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrAuthSessionNotFound
	}
	return nil
}

func (s *MySQLStore) EnsureConversation(ctx context.Context, ownerID, id, title string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ensure conversation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Upsert first, then lock the concrete row. This prevents two processes
	// from both observing a missing conversation and racing to INSERT it.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (id, owner_id, title)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE id = id`, id, ownerID, title); err != nil {
		return fmt.Errorf("materialize conversation: %w", err)
	}

	var existingOwner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM conversations WHERE id = ? FOR UPDATE`, id).Scan(&existingOwner); err != nil {
		return fmt.Errorf("load conversation owner: %w", err)
	}
	if existingOwner != ownerID {
		return fmt.Errorf("conversation belongs to another client")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, id); err != nil {
		return fmt.Errorf("touch conversation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit conversation: %w", err)
	}
	return nil
}

func (s *MySQLStore) LoadMessages(ctx context.Context, ownerID, conversationID string, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, turn_id, message_order
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.conversation_id = ? AND c.owner_id = ?
		ORDER BY m.turn_id DESC, m.message_order DESC, m.id DESC
		LIMIT ?`, conversationID, ownerID, limit)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer rows.Close()

	var reversed []Message
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.Role, &message.Content, &message.TurnID, &message.MessageOrder); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		reversed = append(reversed, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	messages := make([]Message, len(reversed))
	for i := range reversed {
		messages[len(reversed)-1-i] = reversed[i]
	}
	return messages, nil
}

func (s *MySQLStore) SaveMessage(ctx context.Context, ownerID, conversationID string, message Message, requestID, model string) error {
	return s.SaveMessageAtTurn(ctx, ownerID, conversationID, message, requestID, model, message.TurnID, message.MessageOrder)
}

func (s *MySQLStore) SaveMessageAtTurn(ctx context.Context, ownerID, conversationID string, message Message, requestID, model string, turnID int64, messageOrder int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save message: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE conversations SET updated_at = CURRENT_TIMESTAMP(6)
		WHERE id = ? AND owner_id = ?`, conversationID, ownerID)
	if err != nil {
		return fmt.Errorf("check conversation owner: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrConversationNotFound
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (conversation_id, role, content, turn_id, message_order, request_id, model)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, conversationID, message.Role, message.Content, turnID, messageOrder, requestID, model); err != nil {
		return fmt.Errorf("save message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit message: %w", err)
	}
	return nil
}

func (s *MySQLStore) ListConversations(ctx context.Context, ownerID string, limit int) ([]Conversation, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.title, c.created_at, c.updated_at, COUNT(m.id)
		FROM conversations c LEFT JOIN messages m ON m.conversation_id = c.id
		WHERE c.owner_id = ?
		GROUP BY c.id, c.title, c.created_at, c.updated_at
		ORDER BY c.updated_at DESC LIMIT ?`, ownerID, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	var conversations []Conversation
	for rows.Next() {
		var conversation Conversation
		if err := rows.Scan(&conversation.ID, &conversation.Title, &conversation.CreatedAt, &conversation.UpdatedAt, &conversation.MessageCount); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		conversations = append(conversations, conversation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversations: %w", err)
	}
	return conversations, nil
}

func (s *MySQLStore) DeleteConversation(ctx context.Context, ownerID, conversationID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND owner_id = ?`, conversationID, ownerID)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrConversationNotFound
	}
	return nil
}
