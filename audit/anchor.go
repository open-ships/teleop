package audit

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/open-ships/teleop"
)

// A hash chain proves that a log was not edited. It proves nothing about a log
// that was deleted, truncated before its final records, or never written at
// all — the failure mode most likely to matter when a session ends badly.
// Publishing each signed tree head to storage the recorder does not control
// converts those from invisible losses into detectable ones: a log that cannot
// produce a consistency proof against its last published head has been
// rewritten, and a missing log whose head was published is provably missing.

// Checkpoint is a self-describing tree head suitable for publication outside
// the log. A recorder configured with WithSigner populates its key and
// signature fields. Use VerifyCheckpoint with an independently trusted public
// key before relying on a signed checkpoint.
type Checkpoint struct {
	// Version and RecordType select the signed statement format.
	Version    int    `json:"version"`
	RecordType string `json:"record_type"`
	// SignatureAlgorithm identifies how Signature was produced.
	SignatureAlgorithm string `json:"signature_algorithm,omitempty"`
	// Session binds the head to one controller session.
	Session teleop.SessionID `json:"session"`
	// Size and Root are the Merkle head over records preceding this head.
	Size uint64 `json:"size"`
	Root string `json:"root"`
	// ChainHead is the hash of the manifest, checkpoint, or footer itself.
	ChainHead string `json:"chain_head"`
	// EventCount is the number of events committed by this head.
	EventCount uint64 `json:"event_count"`
	// RecordedAt is the recorder timestamp included in the signature.
	RecordedAt time.Time `json:"recorded_at"`
	// KeyID and PublicKey identify the self-declared signer. Verifiers must not
	// use them as the source of trust.
	KeyID     string `json:"key_id,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
	// Signature is the hex-encoded detached signature over the tree head.
	Signature string `json:"signature,omitempty"`
}

// VerifyCheckpoint verifies checkpoint against public, which must be obtained
// outside the checkpoint. It also rejects a conflicting self-declared key or
// key identifier.
func VerifyCheckpoint(public ed25519.PublicKey, checkpoint Checkpoint) error {
	if len(public) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: public key is %d bytes", ErrSignature, len(public))
	}
	if checkpoint.Version != FormatVersion {
		return fmt.Errorf(
			"%w: checkpoint format version is %d, want %d",
			ErrSignature,
			checkpoint.Version,
			FormatVersion,
		)
	}
	switch checkpoint.RecordType {
	case "manifest", "checkpoint", "footer":
	default:
		return fmt.Errorf(
			"%w: checkpoint record type %q is invalid",
			ErrSignature,
			checkpoint.RecordType,
		)
	}
	if checkpoint.Session == (teleop.SessionID{}) &&
		(checkpoint.RecordType == "checkpoint" || checkpoint.EventCount > 0) {
		return fmt.Errorf("%w: checkpoint has no controller session", ErrSessionRequired)
	}
	if checkpoint.SignatureAlgorithm != SignatureAlgorithmEd25519 {
		return fmt.Errorf(
			"%w: checkpoint signature algorithm %q is unsupported",
			ErrSignature,
			checkpoint.SignatureAlgorithm,
		)
	}
	if checkpoint.PublicKey != "" {
		declared, err := decodePublicKey(checkpoint.PublicKey)
		if err != nil {
			return err
		}
		if !declared.Equal(public) {
			return ErrUntrustedKey
		}
	}
	if checkpoint.KeyID != "" && checkpoint.KeyID != KeyID(public) {
		return fmt.Errorf("%w: checkpoint key identifier does not match", ErrUntrustedKey)
	}
	root, err := hex.DecodeString(checkpoint.Root)
	if err != nil {
		return fmt.Errorf("%w: decode checkpoint root: %v", ErrSignature, err)
	}
	if len(root) != HashSize {
		return fmt.Errorf("%w: checkpoint root is %d bytes", ErrSignature, len(root))
	}
	chainHead, err := hex.DecodeString(checkpoint.ChainHead)
	if err != nil {
		return fmt.Errorf("%w: decode checkpoint chain head: %v", ErrSignature, err)
	}
	if len(chainHead) != HashSize {
		return fmt.Errorf(
			"%w: checkpoint chain head is %d bytes",
			ErrSignature,
			len(chainHead),
		)
	}
	return VerifyTreeHead(public, TreeHead{
		Version:    checkpoint.Version,
		RecordType: checkpoint.RecordType,
		Session:    checkpoint.Session,
		Size:       checkpoint.Size,
		Root:       root,
		ChainHead:  checkpoint.ChainHead,
		EventCount: checkpoint.EventCount,
		RecordedAt: checkpoint.RecordedAt,
	}, checkpoint.Signature)
}

// Anchor publishes checkpoints to append-only storage outside the recorder's
// control: object storage under a WORM or legal-hold policy, a transparency
// log, or simply a host under separate custody.
//
// Publish is called from a dedicated goroutine and never blocks the input
// path. Implementations should apply their own timeouts.
type Anchor interface {
	Publish(context.Context, Checkpoint) error
}

// AnchorFunc adapts a function to the Anchor interface.
type AnchorFunc func(context.Context, Checkpoint) error

// Publish implements Anchor.
func (fn AnchorFunc) Publish(ctx context.Context, checkpoint Checkpoint) error {
	return fn(ctx, checkpoint)
}

// FileAnchor appends checkpoints as JSON Lines to a writer. Point it at
// storage with a different failure and custody domain than the audit log
// itself; anchoring to the same directory proves very little.
type FileAnchor struct {
	mu     sync.Mutex
	writer io.Writer
}

// NewFileAnchor returns an Anchor that appends to writer.
func NewFileAnchor(writer io.Writer) *FileAnchor {
	return &FileAnchor{writer: writer}
}

// Publish implements Anchor.
func (a *FileAnchor) Publish(ctx context.Context, checkpoint Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.writer.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if syncer, ok := a.writer.(interface{ Sync() error }); ok {
		if err := syncer.Sync(); err != nil {
			return fmt.Errorf("sync checkpoint: %w", err)
		}
	}
	return nil
}

// anchorRunner publishes checkpoints asynchronously. The queue is bounded and
// drops the oldest pending checkpoint when full: a later checkpoint supersedes
// an earlier one, so under sustained anchor slowness the freshest head is the
// one worth keeping. Drops are counted rather than hidden.
type anchorRunner struct {
	anchor  Anchor
	queue   chan Checkpoint
	done    chan struct{}
	timeout time.Duration

	mu       sync.Mutex
	failure  error
	failures uint64
	dropped  uint64
	sent     uint64
}

func newAnchorRunner(anchor Anchor, depth int, timeout time.Duration) *anchorRunner {
	if depth <= 0 {
		depth = 8
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	runner := &anchorRunner{
		anchor:  anchor,
		queue:   make(chan Checkpoint, depth),
		done:    make(chan struct{}),
		timeout: timeout,
	}
	go runner.run()
	return runner
}

func (r *anchorRunner) run() {
	defer close(r.done)
	for checkpoint := range r.queue {
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		err := r.anchor.Publish(ctx, checkpoint)
		cancel()
		r.mu.Lock()
		if err != nil {
			r.failures++
			if r.failure == nil {
				r.failure = err
			}
		} else {
			r.sent++
		}
		r.mu.Unlock()
	}
}

func (r *anchorRunner) publish(checkpoint Checkpoint) {
	for {
		select {
		case r.queue <- checkpoint:
			return
		default:
		}
		select {
		case <-r.queue:
			r.mu.Lock()
			r.dropped++
			r.mu.Unlock()
		default:
		}
	}
}

func (r *anchorRunner) stop() {
	close(r.queue)
	<-r.done
}

func (r *anchorRunner) stats() AnchorStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return AnchorStats{
		Published: r.sent,
		Failed:    r.failures,
		Dropped:   r.dropped,
		Err:       r.failure,
	}
}

// AnchorStats reports external anchoring health. A non-zero Failed or Dropped
// count means the log's recent heads are not externally witnessed, so recent
// records are integrity protected but not protected against destruction.
type AnchorStats struct {
	// Published is the number of heads successfully sent.
	Published uint64
	// Failed is the number of attempted publications that returned errors.
	Failed uint64
	// Dropped is the number evicted from a saturated queue.
	Dropped uint64
	// Err is the first publication error observed.
	Err error
}
