package assured

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"

	"github.com/open-ships/teleop"
)

// MirroredStore preserves complete evidence bytes in every configured store.
// Successful Sync means every store synchronized the same admitted writes.
// Independent hardware, administration and retention are deployment properties;
// two files on the producer's disk are not independent custody.
type MirroredStore struct {
	mu       sync.Mutex
	stores   []EvidenceStore
	err      error
	closed   bool
	closeErr error
}

// NewMirroredStore takes ownership on success. Stores must be distinct non-nil
// instances. Any write or durability failure becomes sticky, so partial
// replication cannot silently resume as a complete evidence history.
func NewMirroredStore(stores ...EvidenceStore) (*MirroredStore, error) {
	if len(stores) < 2 {
		return nil, fmt.Errorf("%w: at least two evidence stores required", ErrInvalidConfig)
	}
	for i, store := range stores {
		if store == nil || nilInterface(store) || reflect.ValueOf(store).Kind() != reflect.Pointer {
			return nil, fmt.Errorf("%w: mirror %d must be a non-nil pointer instance", ErrInvalidConfig, i)
		}
		for _, previous := range stores[:i] {
			if sameStore(store, previous) {
				return nil, fmt.Errorf("%w: duplicate evidence store", ErrInvalidConfig)
			}
		}
	}
	return &MirroredStore{stores: append([]EvidenceStore(nil), stores...)}, nil
}

func sameStore(left, right EvidenceStore) bool {
	l, r := reflect.ValueOf(left), reflect.ValueOf(right)
	return l.Type() == r.Type() && l.Pointer() == r.Pointer()
}

func (store *MirroredStore) Write(payload []byte) (int, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return 0, io.ErrClosedPipe
	}
	if store.err != nil {
		return 0, store.err
	}
	minimum := len(payload)
	// Isolate each adapter from mutation by a defective neighboring adapter.
	for i, target := range store.stores {
		n, err := writeEvidenceStore(target, append([]byte(nil), payload...))
		if n < 0 || n > len(payload) {
			n, err = 0, errors.New("invalid writer byte count")
		}
		if n != len(payload) && err == nil {
			err = io.ErrShortWrite
		}
		minimum = min(minimum, n)
		if err != nil {
			store.err = errors.Join(store.err, fmt.Errorf("evidence mirror %d: %w", i, err))
		}
	}
	return minimum, store.err
}

func (store *MirroredStore) Sync() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return io.ErrClosedPipe
	}
	var syncErr error
	for i, target := range store.stores {
		if err := syncEvidenceStore(target); err != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("sync mirror %d: %w", i, err))
		}
	}
	// Preserve the original sticky fault, without retaining an unbounded error
	// chain when a supervisor repeatedly probes a failed mirror.
	if store.err == nil {
		store.err = syncErr
	}
	return store.err
}

func (store *MirroredStore) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return store.closeErr
	}
	store.closed = true
	store.closeErr = store.err
	for i, target := range store.stores {
		store.closeErr = errors.Join(store.closeErr, syncEvidenceStore(target))
		if err := closeEvidenceStore(target); err != nil {
			store.closeErr = errors.Join(store.closeErr, fmt.Errorf("close mirror %d: %w", i, err))
		}
	}
	return store.closeErr
}

func writeEvidenceStore(store EvidenceStore, payload []byte) (n int, err error) {
	defer func() {
		if value := recover(); value != nil {
			n = 0
			err = fmt.Errorf("%w: evidence writer: %v", teleop.ErrCallbackPanic, value)
		}
	}()
	return store.Write(payload)
}
