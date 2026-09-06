package assured_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/open-ships/teleop/assured"
	"github.com/open-ships/teleop/audit"
)

func TestMirroredSessionRetainsEntireTrustedEvidenceInBothStores(t *testing.T) {
	first, second := &syncStore{}, &syncStore{}
	mirror, err := assured.NewMirroredStore(first, second)
	if err != nil {
		t.Fatal(err)
	}
	config, key := validConfig(t, first, &witnessAnchor{}, &acceptingActuator{})
	config.EvidenceStore = mirror
	session, err := assured.OpenSource(t.Context(), exactSource(false), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.bytes(), second.bytes()) {
		t.Fatal("evidence copies differ")
	}
	if _, _, err := audit.ReadTrusted(bytes.NewReader(second.bytes()), key); err != nil {
		t.Fatal(err)
	}
	if !first.closed || !second.closed {
		t.Fatal("mirrored stores were not closed")
	}
}

func TestMirrorFailureCannotResumeAsCompleteEvidence(t *testing.T) {
	first, second := &syncStore{}, &syncStore{}
	second.failAt = 1
	mirror, err := assured.NewMirroredStore(first, second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := mirror.Sync(); !errors.Is(err, errStoreSync) {
		t.Fatalf("Sync: %v", err)
	}
	second.failAt = 0
	if _, err := mirror.Write([]byte("later")); !errors.Is(err, errStoreSync) {
		t.Fatalf("failure was not sticky: %v", err)
	}
	if !bytes.Equal(first.bytes(), []byte("first")) || !bytes.Equal(second.bytes(), []byte("first")) {
		t.Fatal("write continued after incomplete durability")
	}
	if err := mirror.Close(); !errors.Is(err, errStoreSync) {
		t.Fatalf("Close: %v", err)
	}
	if !first.closed || !second.closed {
		t.Fatal("cleanup skipped a failed store")
	}
}

func TestMirrorRejectsAliasedStores(t *testing.T) {
	store := &syncStore{}
	if _, err := assured.NewMirroredStore(store, store); err == nil {
		t.Fatal("one store counted as independent copies")
	}
}
