package toolkit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// Session is state that general-purpose tools keep across calls within
// one agent session: the shell's working directory and which files the
// model has read (edits require a fresh read, so the model never
// overwrites content it hasn't seen). Nil-safe: CLI invocations without
// a session get defaults.
type Session struct {
	mu    sync.Mutex
	cwd   string
	reads map[string]string // host-qualified path → content hash
}

// NewSession starts session state with an initial working directory.
func NewSession(cwd string) *Session {
	return &Session{cwd: cwd, reads: map[string]string{}}
}

// Cwd returns the working directory, or def when unset.
func (s *Session) Cwd(def string) string {
	if s == nil {
		return def
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cwd == "" {
		return def
	}
	return s.cwd
}

// SetCwd records a new working directory.
func (s *Session) SetCwd(dir string) {
	if s == nil || dir == "" {
		return
	}
	s.mu.Lock()
	s.cwd = dir
	s.mu.Unlock()
}

// HashContent fingerprints file content for read-before-write checks.
func HashContent(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// MarkRead records that the model saw path with this content.
func (s *Session) MarkRead(key string, content []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.reads[key] = HashContent(content)
	s.mu.Unlock()
}

// CheckFresh verifies path was read in this session and is unchanged
// since. Without a session, nothing is enforced.
func (s *Session) CheckFresh(key string, current []byte) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	h, ok := s.reads[key]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("read the file first — edits and overwrites need the current content in context")
	}
	if h != HashContent(current) {
		return fmt.Errorf("the file changed since it was last read — read it again before editing")
	}
	return nil
}
