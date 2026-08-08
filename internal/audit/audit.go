package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

type Event struct {
	Timestamp      time.Time `json:"timestamp"`
	Principal      string    `json:"principal"`
	Connection     string    `json:"connection,omitempty"`
	Tool           string    `json:"tool"`
	SourceName     string    `json:"source_name,omitempty"`
	SQLFingerprint string    `json:"sql_fingerprint,omitempty"`
	Tables         []string  `json:"tables,omitempty"`
	Allowed        bool      `json:"allowed"`
	Rows           int64     `json:"rows,omitempty"`
	DurationMillis int64     `json:"duration_ms"`
	Error          string    `json:"error,omitempty"`
}

type Auditor struct {
	out    io.WriteCloser
	logger *slog.Logger
	mu     sync.Mutex
}

func New(cfg config.AuditConfig, logger *slog.Logger) (*Auditor, error) {
	switch strings.ToLower(cfg.Destination) {
	case "", "stdout":
		return &Auditor{out: nopCloser{os.Stdout}, logger: logger}, nil
	case "file":
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		return &Auditor{out: f, logger: logger}, nil
	default:
		return nil, fmt.Errorf("unsupported audit destination %q", cfg.Destination)
	}
}

func (a *Auditor) Record(event Event) {
	if a == nil || a.out == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	event.Timestamp = time.Now().UTC()
	data, err := json.Marshal(event)
	if err != nil {
		if a.logger != nil {
			a.logger.Error("audit marshal failed", "error", err)
		}
		return
	}
	if _, err := a.out.Write(append(data, '\n')); err != nil && a.logger != nil {
		a.logger.Error("audit write failed", "error", err)
	}
}

func (a *Auditor) Close() error {
	if a == nil || a.out == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.out.Close()
}

type nopCloser struct {
	io.Writer
}

func (n nopCloser) Close() error { return nil }
