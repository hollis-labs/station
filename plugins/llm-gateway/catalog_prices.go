package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/hollis-labs/libs/llm/core/pricesource"
	"github.com/hollis-labs/libs/llm/core/usageledger"
)

const maxPriceFileBytes = 8 << 20

var errPriceFileNotConfigured = errors.New("price_file_not_configured")

// PriceFileStatus reports local loads, independently from models.dev freshness.
// LastSuccess is a load time; AsOf is the operator's price provenance. No path or
// raw parser/filesystem error is exposed. State stays loaded after a failed reload.
type PriceFileStatus struct {
	State               string    `json:"state"` // disabled, empty, loaded
	Refreshing          bool      `json:"refreshing"`
	Attempts            uint64    `json:"attempts"`
	ConsecutiveFailures uint64    `json:"consecutive_failures"`
	LastAttempt         time.Time `json:"last_attempt,omitzero"`
	LastSuccess         time.Time `json:"last_success,omitzero"`
	LastError           string    `json:"last_error,omitempty"`
	AsOf                time.Time `json:"as_of,omitzero"`
	Entries             int       `json:"entries"`
}

// filePriceSource serializes reloads and publishes table + health together.
// Readers continue using the immutable previous table while a reload is in flight.
type filePriceSource struct {
	path   string
	gate   chan struct{}
	mu     sync.RWMutex
	table  *pricesource.Table
	health PriceFileStatus
}

func newFilePriceSource(path string) *filePriceSource {
	s := &filePriceSource{path: path, gate: make(chan struct{}, 1)}
	s.health.State = "empty"
	if path == "" {
		s.health.State = "disabled"
	}
	return s
}

func (s *filePriceSource) Snapshot(provider, model string) (usageledger.PriceSnapshot, bool) {
	s.mu.RLock()
	t := s.table
	s.mu.RUnlock()
	return t.Snapshot(provider, model)
}

func (s *filePriceSource) Status() PriceFileStatus {
	if s == nil {
		return PriceFileStatus{State: "disabled"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.health
}

func (s *filePriceSource) Reload(ctx context.Context) error {
	if s.path == "" {
		return errPriceFileNotConfigured
	}
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.health.Attempts++
	s.health.LastAttempt = time.Now().UTC()
	s.health.Refreshing = true
	s.mu.Unlock()
	t, err := loadPriceFile(s.path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	s.health.Refreshing = false
	if err != nil {
		s.health.LastError = err.Error() // loadPriceFile returns fixed safe codes only.
		if errors.Is(err, context.Canceled) {
			s.health.LastError = "canceled"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			s.health.LastError = "timeout"
		}
		s.health.ConsecutiveFailures++
		return err
	}
	s.table = t
	s.health.State = "loaded"
	s.health.LastSuccess = time.Now().UTC()
	s.health.AsOf, s.health.Entries = t.AsOf(), t.Len()
	s.health.LastError, s.health.ConsecutiveFailures = "", 0
	return nil
}

func loadPriceFile(path string) (*pricesource.Table, error) {
	// Reject special files before opening, including FIFOs. This is an operator-
	// managed local file; regular filesystem reads themselves are not cancellable.
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("file_read")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("file_not_regular")
	}
	if info.Size() > maxPriceFileBytes {
		return nil, errors.New("file_too_large")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("file_read")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPriceFileBytes+1))
	if err != nil {
		return nil, errors.New("file_read")
	}
	if len(b) > maxPriceFileBytes {
		return nil, errors.New("file_too_large")
	}
	// Require explicit entries, including [] to clear overrides. Unmarshal also
	// rejects trailing JSON values; ParseTable validates fields and rates.
	var envelope struct {
		Entries json.RawMessage `json:"entries"`
	}
	if json.Unmarshal(b, &envelope) != nil || len(envelope.Entries) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Entries), []byte("null")) {
		return nil, errors.New("invalid_price_file")
	}
	t, err := pricesource.ParseTable(bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("invalid_price_file")
	}
	return t, nil
}
