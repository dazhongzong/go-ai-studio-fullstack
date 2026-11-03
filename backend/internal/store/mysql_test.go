package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestIsRetryableLockError(t *testing.T) {
	tests := []struct {
		name   string
		number uint16
		want   bool
	}{
		{name: "lock wait timeout", number: 1205, want: true},
		{name: "deadlock", number: 1213, want: true},
		{name: "duplicate key", number: 1062, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &mysql.MySQLError{Number: tt.number})
			if got := isRetryableLockError(err); got != tt.want {
				t.Fatalf("isRetryableLockError() = %v, want %v", got, tt.want)
			}
		})
	}

	if isRetryableLockError(errors.New("ordinary database error")) {
		t.Fatal("ordinary errors must not be retried")
	}
}
