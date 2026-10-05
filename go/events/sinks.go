package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

func init() {
	RegisterSinkType("file", SinkType{
		Validate: func(s SinkConfig) error {
			if s.Path == "" {
				return fmt.Errorf(`path is required for a file (use "-" for stdout)`)
			}
			return nil
		},
		New: func(s SinkConfig, _ func(string) string) (Sink, error) { return newFile(s.Path) },
	})
}

// ---- file ------------------------------------------------------------------

type fileSink struct {
	mu sync.Mutex
	f  *os.File
	// stdout is shared with the process, so Close leaves it open.
	stdout bool
}

func newFile(path string) (Sink, error) {
	if path == "-" {
		return &fileSink{f: os.Stdout, stdout: true}, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errors.New("cannot open the file for appending (does its directory exist and is it writable?)")
	}
	return &fileSink{f: f}, nil
}

// Send appends one JSON line.
func (s *fileSink) Send(_ context.Context, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return &SendError{Kind: "rejected"}
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(b); err != nil {
		return &SendError{Kind: "transport"}
	}
	return nil
}

func (s *fileSink) Close() error {
	if s.stdout {
		return nil
	}
	return s.f.Close()
}
