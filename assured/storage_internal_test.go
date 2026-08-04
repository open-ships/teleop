package assured

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateFileStoreRemovesFileAfterDirectorySyncFailure(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	syncErr := errors.New("directory durability unavailable")
	var syncCalls int
	store, err := createFileStore(path, func(string) error {
		syncCalls++
		return syncErr
	})
	if store != nil {
		t.Fatal("failed initialization returned a FileStore")
	}
	if !errors.Is(err, syncErr) {
		t.Fatalf("CreateFileStore error = %v, want directory sync failure", err)
	}
	if syncCalls != 2 {
		t.Fatalf("parent sync calls = %d, want initial attempt plus cleanup resync", syncCalls)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed evidence artifact still exists: %v", statErr)
	}

	// Cleanup restores the exclusive-create path for a corrected retry.
	retried, retryErr := CreateFileStore(path)
	if retryErr != nil {
		t.Fatalf("retry CreateFileStore: %v", retryErr)
	}
	if err := retried.Close(); err != nil {
		t.Fatalf("close retried store: %v", err)
	}
}
