package audit

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/open-ships/teleop"
)

// A hash chain detects edits relative to a retained head; an unkeyed chain can
// be recomputed by an attacker. It also says nothing by itself about a log that
// was deleted, truncated before its final records, or never written. Retaining
// signed tree heads in an independently controlled system can expose those
// failures: a later log must extend the witnessed head consistently, and the
// external head remains evidence that a now-missing log existed.

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

// WitnessReceipt records that an Anchor adapter returned success for one
// checkpoint. AcknowledgedAt is observed by this process and is not trusted
// time; stronger receipt timestamps and custody guarantees belong to the
// concrete Anchor implementation.
type WitnessReceipt struct {
	Checkpoint     Checkpoint
	AcknowledgedAt time.Time
}

func cloneWitnessReceipt(receipt *WitnessReceipt) *WitnessReceipt {
	if receipt == nil {
		return nil
	}
	cloned := *receipt
	return &cloned
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
// path. Implementations must honor context cancellation; the recorder recovers
// panics at this adapter boundary and reports them as publication failures.
type Anchor interface {
	Publish(context.Context, Checkpoint) error
}

// AnchorFunc adapts a function to the Anchor interface.
type AnchorFunc func(context.Context, Checkpoint) error

// Publish implements Anchor.
func (fn AnchorFunc) Publish(ctx context.Context, checkpoint Checkpoint) error {
	if fn == nil {
		return fmt.Errorf("teleop/audit: nil anchor function")
	}
	return fn(ctx, checkpoint)
}

// FileAnchor appends checkpoints as JSON Lines to a writer. Point it at
// storage with a different failure and custody domain than the audit log
// itself; anchoring to the same directory proves very little.
type FileAnchor struct {
	mu     sync.Mutex
	writer io.Writer
}

// NewFileAnchor returns an Anchor that appends to writer. After each complete
// line it calls Flush and then Sync when writer exposes those methods. A
// buffering wrapper that needs durable acknowledgement should expose both and
// delegate Sync to its underlying durable store.
func NewFileAnchor(writer io.Writer) *FileAnchor {
	return &FileAnchor{writer: writer}
}

// Publish implements Anchor.
func (a *FileAnchor) Publish(ctx context.Context, checkpoint Checkpoint) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf(
				"%w: file anchor writer callback: %v",
				teleop.ErrCallbackPanic,
				recovered,
			)
		}
	}()
	if a == nil {
		return fmt.Errorf("teleop/audit: nil file anchor")
	}
	if ctx == nil {
		return fmt.Errorf("teleop/audit: nil anchor context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writer == nil {
		return fmt.Errorf("teleop/audit: file anchor writer is nil")
	}
	line := append(encoded, '\n')
	written, err := a.writer.Write(line)
	if err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if written != len(line) {
		return fmt.Errorf("write checkpoint: %w", io.ErrShortWrite)
	}
	if flusher, ok := a.writer.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			return fmt.Errorf("flush checkpoint: %w", err)
		}
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

	mu          sync.Mutex
	failure     error
	failures    uint64
	dropped     uint64
	sent        uint64
	lastReceipt *WitnessReceipt
	// Exact outcomes let the recorder distinguish failure of its latest head
	// from an older queued head. Only the latest outcome of each kind is needed:
	// one recorder serializes publications and newer heads supersede older ones.
	lastFailureCheckpoint *Checkpoint
	lastFailureErr        error
	lastDroppedCheckpoint *Checkpoint
	circuitOpen           bool
	changed               chan struct{}
	stopped               bool
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
		changed: make(chan struct{}),
	}
	go runner.run()
	return runner
}

func (r *anchorRunner) run() {
	defer func() {
		r.mu.Lock()
		r.stopped = true
		r.signalChangedLocked()
		r.mu.Unlock()
		close(r.done)
	}()
	for checkpoint := range r.queue {
		r.mu.Lock()
		circuitOpen := r.circuitOpen
		r.mu.Unlock()
		if circuitOpen {
			// A timed-out adapter may still own its worker because it violated the
			// context contract. Do not create one leaked goroutine per later head;
			// keep draining the queue and report those unattempted publications.
			r.mu.Lock()
			r.dropped++
			dropped := checkpoint
			r.lastDroppedCheckpoint = &dropped
			r.signalChangedLocked()
			r.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		published := make(chan error, 1)
		go func() {
			// Recovery belongs inside the worker: a panic in one goroutine cannot
			// be recovered by the runner goroutine waiting below.
			published <- publishAnchor(r.anchor, ctx, checkpoint)
		}()
		var (
			err      error
			timedOut bool
		)
		select {
		case err = <-published:
		case <-ctx.Done():
			err = ctx.Err()
			timedOut = true
		}
		cancel()
		r.mu.Lock()
		if timedOut {
			r.circuitOpen = true
		}
		if err != nil {
			r.failures++
			if r.failure == nil {
				r.failure = err
			}
			failed := checkpoint
			r.lastFailureCheckpoint = &failed
			r.lastFailureErr = err
		} else {
			r.sent++
			r.lastReceipt = &WitnessReceipt{
				Checkpoint:     checkpoint,
				AcknowledgedAt: time.Now().UTC(),
			}
		}
		r.signalChangedLocked()
		r.mu.Unlock()
	}
}

// publishAnchor is the process-safety boundary around externally implemented
// Anchor adapters. A witness fault must degrade evidence health and wake
// waiters; it must never take down the controller process.
func publishAnchor(anchor Anchor, ctx context.Context, checkpoint Checkpoint) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: anchor publish: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	if anchor == nil {
		return fmt.Errorf("teleop/audit: nil anchor")
	}
	return anchor.Publish(ctx, checkpoint)
}

func (r *anchorRunner) signalChangedLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *anchorRunner) publish(checkpoint Checkpoint) {
	for {
		select {
		case r.queue <- checkpoint:
			return
		default:
		}
		select {
		case dropped := <-r.queue:
			r.mu.Lock()
			r.dropped++
			droppedCheckpoint := dropped
			r.lastDroppedCheckpoint = &droppedCheckpoint
			r.signalChangedLocked()
			r.mu.Unlock()
		default:
		}
	}
}

type anchorRetryState uint8

const (
	anchorRetryPending anchorRetryState = iota
	anchorRetryAcknowledged
	anchorRetryEligible
	anchorRetryTerminal
)

// retryState classifies one exact publication without clearing historical
// degradation. An ordinary adapter error or queue eviction may recover on one
// later head. A panic, an open timeout circuit, or a stopped runner must not
// spawn another adapter worker automatically.
func (r *anchorRunner) retryState(target Checkpoint) anchorRetryState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastReceipt != nil && checkpointWitnessCovers(
		r.lastReceipt.Checkpoint,
		target,
	) {
		return anchorRetryAcknowledged
	}
	if sameCheckpoint(r.lastFailureCheckpoint, target) {
		if r.circuitOpen || errors.Is(r.lastFailureErr, teleop.ErrCallbackPanic) {
			return anchorRetryTerminal
		}
		return anchorRetryEligible
	}
	if sameCheckpoint(r.lastDroppedCheckpoint, target) {
		if r.circuitOpen {
			return anchorRetryTerminal
		}
		return anchorRetryEligible
	}
	if r.stopped {
		return anchorRetryTerminal
	}
	return anchorRetryPending
}

func sameCheckpoint(candidate *Checkpoint, target Checkpoint) bool {
	if candidate == nil {
		return false
	}
	return candidate.RecordType == target.RecordType &&
		candidate.Session == target.Session &&
		candidate.Size == target.Size &&
		candidate.ChainHead == target.ChainHead &&
		candidate.EventCount == target.EventCount
}

func (r *anchorRunner) stop() {
	close(r.queue)
	<-r.done
}

func (r *anchorRunner) stats() AnchorStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return AnchorStats{
		Published:   r.sent,
		Failed:      r.failures,
		Dropped:     r.dropped,
		Err:         r.failure,
		LastReceipt: cloneWitnessReceipt(r.lastReceipt),
	}
}

func (r *anchorRunner) observation() (AnchorStats, <-chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return AnchorStats{
		Published:   r.sent,
		Failed:      r.failures,
		Dropped:     r.dropped,
		Err:         r.failure,
		LastReceipt: cloneWitnessReceipt(r.lastReceipt),
	}, r.changed, r.stopped
}

func (r *anchorRunner) wait(
	ctx context.Context,
	eventCount uint64,
) (WitnessReceipt, error) {
	for {
		stats, changed, stopped := r.observation()
		if stats.LastReceipt != nil &&
			stats.LastReceipt.Checkpoint.EventCount >= eventCount {
			return *cloneWitnessReceipt(stats.LastReceipt), nil
		}
		if stats.Failed > 0 {
			return WitnessReceipt{}, errors.Join(ErrWitnessRequired, stats.Err)
		}
		if stats.Dropped > 0 {
			return WitnessReceipt{}, fmt.Errorf(
				"%w: %d witness publication(s) were dropped",
				ErrWitnessRequired,
				stats.Dropped,
			)
		}
		if stopped {
			return WitnessReceipt{}, fmt.Errorf(
				"%w: recorder closed before event count %d was acknowledged",
				ErrWitnessRequired,
				eventCount,
			)
		}
		select {
		case <-ctx.Done():
			return WitnessReceipt{}, ctx.Err()
		case <-changed:
		case <-r.done:
		}
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
	// LastReceipt is the high-water acknowledgement returned by the Anchor.
	// Its checkpoint is isolated from the runner's internal state.
	LastReceipt *WitnessReceipt
}
