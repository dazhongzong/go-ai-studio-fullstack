package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Message is the persisted part of a conversation.
type Message struct {
	Role    string
	Content string
}

type Conversation struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MessageCount int       `json:"message_count"`
}

var ErrConversationNotFound = errors.New("conversation not found")

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
		`CREATE TABLE IF NOT EXISTS conversations (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			owner_id VARCHAR(64) NOT NULL DEFAULT '',
			title VARCHAR(255) NOT NULL DEFAULT '',
			created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			INDEX idx_conversations_owner_updated (owner_id, updated_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS messages (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			conversation_id VARCHAR(64) NOT NULL,
			role VARCHAR(16) NOT NULL,
			content MEDIUMTEXT NOT NULL,
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
	return nil
}

func (s *MySQLStore) EnsureConversation(ctx context.Context, ownerID, id, title string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ensure conversation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existingOwner string
	err = tx.QueryRowContext(ctx, `SELECT owner_id FROM conversations WHERE id = ? FOR UPDATE`, id).Scan(&existingOwner)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversations (id, owner_id, title) VALUES (?, ?, ?)`, id, ownerID, title); err != nil {
			return fmt.Errorf("insert conversation: %w", err)
		}
	case err != nil:
		return fmt.Errorf("load conversation owner: %w", err)
	case existingOwner != ownerID:
		return fmt.Errorf("conversation belongs to another client")
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, id); err != nil {
			return fmt.Errorf("touch conversation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit conversation: %w", err)
	}
	return nil
}

func (s *MySQLStore) LoadMessages(ctx context.Context, ownerID, conversationID string, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.conversation_id = ? AND c.owner_id = ?
		ORDER BY m.id DESC
		LIMIT ?`, conversationID, ownerID, limit)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer rows.Close()

	var reversed []Message
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.Role, &message.Content); err != nil {
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
		INSERT INTO messages (conversation_id, role, content, request_id, model)
		VALUES (?, ?, ?, ?, ?)`, conversationID, message.Role, message.Content, requestID, model); err != nil {
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
