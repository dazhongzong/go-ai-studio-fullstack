package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
)

// Message is the persisted part of a conversation.
type Message struct {
	Role    string
	Content string
}

// ConversationStore persists conversation history independently of an API
// instance. That makes history available when a load balancer sends the next
// request to a different backend.
type ConversationStore interface {
	EnsureConversation(context.Context, string, string) error
	LoadMessages(context.Context, string, int) ([]Message, error)
	SaveMessage(context.Context, string, Message, string, string) error
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

func (s *MySQLStore) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS conversations (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			title VARCHAR(255) NOT NULL DEFAULT '',
			created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)
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
	return nil
}

func (s *MySQLStore) EnsureConversation(ctx context.Context, id, title string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversations (id, title) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE updated_at = updated_at`, id, title)
	if err != nil {
		return fmt.Errorf("ensure conversation: %w", err)
	}
	return nil
}

func (s *MySQLStore) LoadMessages(ctx context.Context, conversationID string, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content
		FROM messages
		WHERE conversation_id = ?
		ORDER BY id DESC
		LIMIT ?`, conversationID, limit)
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

func (s *MySQLStore) SaveMessage(ctx context.Context, conversationID string, message Message, requestID, model string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save message: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (conversation_id, role, content, request_id, model)
		VALUES (?, ?, ?, ?, ?)`, conversationID, message.Role, message.Content, requestID, model); err != nil {
		return fmt.Errorf("save message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations SET updated_at = CURRENT_TIMESTAMP(6)
		WHERE id = ?`, conversationID); err != nil {
		return fmt.Errorf("update conversation time: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit message: %w", err)
	}
	return nil
}
