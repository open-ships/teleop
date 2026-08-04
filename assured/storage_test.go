package assured_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-ships/teleop/assured"
)

func TestCreateFileStoreIsExclusiveAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	store, err := assured.CreateFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write([]byte("evidence\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "evidence\n" {
		t.Fatalf("content = %q", content)
	}
	if _, err := assured.CreateFileStore(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive create error = %v, want os.ErrExist", err)
	}
}

func TestFileStoreRejectsUseAfterClose(t *testing.T) {
	store, err := assured.CreateFileStore(filepath.Join(t.TempDir(), "closed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write([]byte("late")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write after close = %v, want os.ErrClosed", err)
	}
	if err := store.Sync(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Sync after close = %v, want os.ErrClosed", err)
	}
}
