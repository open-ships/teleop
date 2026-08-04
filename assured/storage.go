// Package assured composes controller input, safety authority, durable
// evidence, external witnessing, and ordered shutdown into one operational
// session.
package assured

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// EvidenceStore is the local crash-durable storage seam required by an
// Assured Session. Sync must not return until bytes previously accepted by
// Write have reached the adapter's durable medium.
//
// This interface says nothing about adversarial deletion, retention, or legal
// hold. Those properties require an independently administered witness/WORM
// adapter in addition to the local store.
type EvidenceStore interface {
	io.Writer
	Sync() error
	Close() error
}

// FileStore is an exclusive, owner-only local EvidenceStore. It synchronizes
// the new directory entry during creation on platforms that expose directory
// fsync, and synchronizes file contents on every Sync and Close.
type FileStore struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	closed   bool
	closeErr error
}

// CreateFileStore creates a new evidence file without replacing an existing
// path. The parent directory must already exist.
func CreateFileStore(path string) (*FileStore, error) {
	return createFileStore(path, syncParentDirectory)
}

func createFileStore(path string, syncParent func(string) error) (*FileStore, error) {
	if path == "" {
		return nil, fmt.Errorf("assured: evidence path is empty")
	}
	clean := filepath.Clean(path)
	if clean == "." || filepath.Base(clean) == "." {
		return nil, fmt.Errorf("assured: evidence path %q is not a file", path)
	}
	file, err := os.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create evidence file: %w", err)
	}
	store := &FileStore{file: file, path: clean}
	if err := file.Sync(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("sync new evidence file: %w", err),
			cleanupCreatedFile(file, clean, syncParent),
		)
	}
	if err := syncParent(clean); err != nil {
		return nil, errors.Join(
			fmt.Errorf("sync evidence directory: %w", err),
			cleanupCreatedFile(file, clean, syncParent),
		)
	}
	return store, nil
}

// cleanupCreatedFile is used only before a FileStore escapes to its caller.
// O_EXCL established that clean names the file this invocation created, so the
// recovery path removes that exact artifact and makes the removal durable when
// the platform supports directory synchronization.
func cleanupCreatedFile(file *os.File, clean string, syncParent func(string) error) error {
	closeErr := file.Close()
	removeErr := os.Remove(clean)
	var directoryErr error
	if removeErr == nil {
		directoryErr = syncParent(clean)
	}
	return errors.Join(closeErr, removeErr, directoryErr)
}

// Path returns the cleaned path created by CreateFileStore.
func (s *FileStore) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// Write implements io.Writer.
func (s *FileStore) Write(payload []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil {
		return 0, os.ErrClosed
	}
	return s.file.Write(payload)
}

// Sync implements EvidenceStore.
func (s *FileStore) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil {
		return os.ErrClosed
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sync evidence file: %w", err)
	}
	return nil
}

// Close synchronizes and closes the file. It is idempotent and returns the
// same joined error after the first call.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.file == nil {
		return nil
	}
	s.closeErr = errors.Join(s.file.Sync(), s.file.Close())
	s.file = nil
	return s.closeErr
}

var _ EvidenceStore = (*FileStore)(nil)
