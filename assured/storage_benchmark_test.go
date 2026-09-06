package assured_test

import (
	"path/filepath"
	"testing"

	"github.com/open-ships/teleop/assured"
)

// This measures one real local Write+Sync barrier, not an entire Apply or a
// crash-durability guarantee. Installed media and full path tail latency need
// separate measurement under load, faults and power loss.
func BenchmarkFileStoreWriteSync(b *testing.B) {
	store, err := assured.CreateFileStore(filepath.Join(b.TempDir(), "evidence.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Error(err)
		}
	})
	payload := make([]byte, 4096)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if n, err := store.Write(payload); err != nil || n != len(payload) {
			b.Fatalf("write=%d err=%v", n, err)
		}
		if err := store.Sync(); err != nil {
			b.Fatal(err)
		}
	}
}
