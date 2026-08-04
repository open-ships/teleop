package audit_test

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
)

func TestTimedCheckpointContainsRecorderCallbackPanics(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (*audit.Recorder, func())
	}{
		{
			name: "clock",
			setup: func(t *testing.T) (*audit.Recorder, func()) {
				var armed atomic.Bool
				recorder := audit.NewRecorder(
					&syncBuffer{},
					audit.WithClock(func() time.Time {
						if armed.Load() {
							panic("clock fault")
						}
						return time.Now()
					}),
					audit.WithCheckpoints(10*time.Millisecond, 0),
				)
				return recorder, func() { armed.Store(true) }
			},
		},
		{
			name: "signer",
			setup: func(t *testing.T) (*audit.Recorder, func()) {
				_, private, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				signer := &armablePanicSigner{private: private}
				return audit.NewRecorder(
					&syncBuffer{},
					audit.WithSigner(signer),
					audit.WithCheckpoints(10*time.Millisecond, 0),
				), func() { signer.armed.Store(true) }
			},
		},
		{
			name: "writer",
			setup: func(t *testing.T) (*audit.Recorder, func()) {
				writer := &armablePanicWriter{}
				return audit.NewRecorder(
					writer,
					audit.WithCheckpoints(10*time.Millisecond, 0),
				), func() { writer.armed.Store(true) }
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder, arm := test.setup(t)
			if err := recorder.Record(t.Context(), testButtonEvent(1)); err != nil {
				t.Fatal(err)
			}
			arm()
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if err := recorder.EvidenceStatus().Err; errors.Is(err, teleop.ErrCallbackPanic) {
					if closeErr := recorder.Close(); !errors.Is(closeErr, teleop.ErrCallbackPanic) {
						t.Fatalf("Close error = %v, want callback panic", closeErr)
					}
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatalf("timer callback panic was not made sticky: %+v", recorder.EvidenceStatus())
		})
	}
}

func TestRecorderRejectsInvalidSignerOutputBeforeWriting(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	recorder := audit.NewRecorder(&output, audit.WithSigner(invalidSignatureSigner{public: public}))
	if err := recorder.Record(t.Context(), testButtonEvent(1)); !errors.Is(err, audit.ErrSignature) {
		t.Fatalf("Record error = %v, want ErrSignature", err)
	}
	if output.Len() != 0 {
		t.Fatalf("invalid signer output wrote %d bytes", output.Len())
	}
}

func TestRecorderContainsSignerPublicKeyPanic(t *testing.T) {
	recorder := audit.NewRecorder(&bytes.Buffer{}, audit.WithSigner(publicPanicSigner{}))
	if err := recorder.Record(t.Context(), testButtonEvent(1)); !errors.Is(
		err,
		teleop.ErrCallbackPanic,
	) {
		t.Fatalf("Record error = %v, want callback panic", err)
	}
}

type armablePanicSigner struct {
	private ed25519.PrivateKey
	armed   atomic.Bool
}

func (signer *armablePanicSigner) Public() crypto.PublicKey {
	return signer.private.Public()
}

func (signer *armablePanicSigner) Sign(
	random io.Reader,
	digest []byte,
	options crypto.SignerOpts,
) ([]byte, error) {
	if signer.armed.Load() {
		panic("signer fault")
	}
	return signer.private.Sign(random, digest, options)
}

type armablePanicWriter struct {
	mu    sync.Mutex
	data  bytes.Buffer
	armed atomic.Bool
}

func (writer *armablePanicWriter) Write(payload []byte) (int, error) {
	if writer.armed.Load() {
		panic("writer fault")
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.data.Write(payload)
}

func (*armablePanicWriter) Sync() error { return nil }

type invalidSignatureSigner struct {
	public ed25519.PublicKey
}

func (signer invalidSignatureSigner) Public() crypto.PublicKey { return signer.public }
func (invalidSignatureSigner) Sign(
	io.Reader,
	[]byte,
	crypto.SignerOpts,
) ([]byte, error) {
	return make([]byte, ed25519.SignatureSize), nil
}

type publicPanicSigner struct{}

func (publicPanicSigner) Public() crypto.PublicKey { panic("public key fault") }
func (publicPanicSigner) Sign(
	io.Reader,
	[]byte,
	crypto.SignerOpts,
) ([]byte, error) {
	return nil, nil
}
