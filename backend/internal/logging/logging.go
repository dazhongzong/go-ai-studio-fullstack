// Package logging configures the process-wide standard logger.
package logging

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Setup writes standard-library logs to both stderr and logs/<component>.log.
// LOG_DIR can override the default logs directory. Existing log files are
// appended to so restarting a service does not erase the previous run.
func Setup(component string) (func(), error) {
	dir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if dir == "" {
		dir = "logs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(
		filepath.Join(dir, component+".log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0o644,
	)
	if err != nil {
		return nil, err
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(os.Stderr, file))
	return func() {
		_ = file.Sync()
		_ = file.Close()
	}, nil
}
