package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/internal/eventorder"
)

const (
	// FormatVersion is the current audit wire format.
	FormatVersion = 3
	// ControlSchemaVersion pins persisted ControlID meanings independently of
	// the record container.
	ControlSchemaVersion = "teleop.controls.v1"
	// MaxRecordSize is the largest encoded JSON Lines record accepted or
	// produced by this package.
	MaxRecordSize = 16 * 1024 * 1024

	// DefaultCheckpointInterval bounds how long a log can run without emitting
	// an anchorable tree head. WithSigner makes that head signed.
	DefaultCheckpointInterval = 10 * time.Second
	// DefaultCheckpointEvery bounds the same by record count, so a busy log
	// checkpoints on volume rather than only on elapsed time.
	DefaultCheckpointEvery = 10000
	// DefaultMaxPendingBytes bounds event data withheld from a Verify callback
	// while it waits for the next required signed tree head.
	DefaultMaxPendingBytes = 64 * 1024 * 1024
	// maxIdleWitnessRetries bounds locally appended retry heads for unchanged
	// event coverage. New activity resets the budget for its newer coverage.
	maxIdleWitnessRetries = 1
)

var (
	// ErrClosed reports use of a recorder after Close.
	ErrClosed = errors.New("teleop/audit: recorder closed")
	// ErrFailed reports a recorder whose earlier permanent failure is sticky.
	ErrFailed = errors.New("teleop/audit: recorder failed")
	// ErrUnverified reports a stream without an integrity chain.
	ErrUnverified = errors.New("teleop/audit: stream is not hash chained")
	// ErrUnauthenticated reports a stream without an HMAC chain when one was
	// required.
	ErrUnauthenticated = errors.New("teleop/audit: stream is not HMAC authenticated")
	// ErrAuthenticationRequired reports an HMAC stream read without its key.
	ErrAuthenticationRequired = errors.New("teleop/audit: HMAC key required")
	// ErrIncomplete reports a missing footer or torn final record.
	ErrIncomplete = errors.New("teleop/audit: stream is incomplete")
	// ErrRecordTooLarge reports a JSON Lines record larger than MaxRecordSize.
	ErrRecordTooLarge = errors.New("teleop/audit: record too large")
	// ErrSessionRequired reports an event or checkpoint that cannot be bound to
	// a non-zero controller session.
	ErrSessionRequired = errors.New("teleop/audit: controller session is required")
	// ErrTreeMismatch reports a checkpoint whose Merkle head does not match
	// the records that precede it.
	ErrTreeMismatch = errors.New("teleop/audit: merkle head does not match records")
	// ErrPendingLimit reports that events awaiting a required signed tree head
	// exceed VerifyOptions.MaxPendingBytes.
	ErrPendingLimit = errors.New("teleop/audit: pending signature buffer limit exceeded")
	// ErrInvalidEvent reports an event that cannot be represented faithfully or
	// whose identity, sequence, or causality would make the completed stream
	// unverifiable.
	ErrInvalidEvent = errors.New("teleop/audit: invalid event")
	// ErrWitnessRequired reports that a recorder configured to require external
	// witnessing could not prove that its completed footer was acknowledged.
	ErrWitnessRequired = errors.New("teleop/audit: required external witness unavailable")
	ErrWitnessMismatch = errors.New("teleop/audit: evidence does not match retained witness")
)

const (
	chainNone   = "none"
	chainSHA256 = "sha256"
	chainHMAC   = "hmac-sha256"
)

// Options configures a Recorder. Applications normally use the With functions
// rather than constructing Options directly.
type Options struct {
	// HashChain enables the default SHA-256 integrity chain.
	HashChain bool
	// FlushEveryEvent flushes and, when supported, syncs each record.
	FlushEveryEvent bool
	// HMACKey selects HMAC-SHA-256 instead of an unkeyed chain.
	HMACKey []byte
	// Now supplies record timestamps.
	Now func() time.Time

	// Signer signs tree heads and must expose an Ed25519 public key.
	Signer crypto.Signer
	// Anchor receives tree heads for publication outside the log's custody.
	Anchor Anchor
	// AnchorTimeout bounds each asynchronous publication.
	AnchorTimeout time.Duration
	// AnchorQueue is the number of pending publications retained.
	AnchorQueue int
	// RequireWitness makes Close fail unless every attempted publication
	// succeeded and the final footer was acknowledged by Anchor. It does not make
	// network I/O block controller input while the session is active.
	RequireWitness bool
	// CheckpointInterval and CheckpointEvery bound the gap between tree heads.
	CheckpointInterval time.Duration
	CheckpointEvery    uint64
	// Provenance describes the build and operating context.
	Provenance *Provenance
}

// Option configures a Recorder.
type Option func(*Options)

// WithHashChain disables or enables integrity chaining. ReadAll rejects
// unchained streams; use Read with AllowUnverified for an intentional
// unverified log.
func WithHashChain(enabled bool) Option {
	return func(options *Options) { options.HashChain = enabled }
}

// WithFlushEveryEvent controls flush/fsync after each record. It defaults to
// true so a crash loses at most an in-flight system write.
func WithFlushEveryEvent(enabled bool) Option {
	return func(options *Options) { options.FlushEveryEvent = enabled }
}

// WithHMAC authenticates the chain with HMAC-SHA-256. The key is copied; an
// empty key is rejected on the first Record or Close. Use a high-entropy key
// generated by a cryptographically secure source; 32 random bytes retain the
// full security strength of SHA-256.
func WithHMAC(key []byte) Option {
	return func(options *Options) {
		options.HMACKey = make([]byte, len(key))
		copy(options.HMACKey, key)
		options.HashChain = true
	}
}

// WithClock supplies deterministic record timestamps.
func WithClock(now func() time.Time) Option {
	return func(options *Options) {
		if now != nil {
			options.Now = now
		}
	}
}

// WithSigner signs the manifest, every checkpoint, and the footer with an
// Ed25519 key, allowing verification without sharing signing capability.
// Individual events are not signed: the Merkle tree and hash chain already
// bind them to the nearest signed head, so per-event signing would add cost
// without adding evidence.
//
// Prefer an Ed25519-capable hardware or remote crypto.Signer whose private key
// cannot be exported. A key the operator can read is a key the operator can be
// accused of having used.
func WithSigner(signer crypto.Signer) Option {
	return func(options *Options) {
		if signer != nil {
			options.Signer = signer
			options.HashChain = true
		}
	}
}

// WithAnchor publishes each checkpoint to storage outside this process, so a
// destroyed or truncated log can be detected rather than merely suspected.
// Publication is asynchronous and never blocks the input path; use
// Recorder.AnchorStats to observe its health.
func WithAnchor(anchor Anchor) Option {
	return func(options *Options) {
		if anchor != nil {
			options.Anchor = anchor
			options.HashChain = true
		}
	}
}

// WithRequiredWitness controls whether Close requires successful external
// acknowledgement of the final footer. This is a completion policy, not a
// trusted-time claim: the Anchor adapter still determines what acknowledgement
// means and must live in a separate custody domain to provide useful evidence.
func WithRequiredWitness(required bool) Option {
	return func(options *Options) { options.RequireWitness = required }
}

// WithAnchorTimeout bounds a single anchor publication.
func WithAnchorTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		if timeout > 0 {
			options.AnchorTimeout = timeout
		}
	}
}

// WithCheckpoints sets how often a tree head is emitted, by elapsed time and
// by record count. WithSigner makes each head signed. Either bound may be zero
// to disable it. Frequent checkpoints narrow nominal witness lag while the
// adapter is healthy; they cannot impose a bound during a witness outage. When
// interval is positive, its timer also appends at most one retry head for the
// same event coverage after an exact recoverable Anchor failure or queue drop.
// Historical failure and drop accounting remains sticky after recovery.
func WithCheckpoints(interval time.Duration, every uint64) Option {
	return func(options *Options) {
		options.CheckpointInterval = interval
		options.CheckpointEvery = every
	}
}

// WithProvenance records the build, host, configuration, and operator behind
// the session in the manifest.
func WithProvenance(provenance Provenance) Option {
	return func(options *Options) {
		cloned := provenance.Clone()
		options.Provenance = &cloned
	}
}

// The first nine fields retain the v1 order for legacy hash verification.
type diskRecord struct {
	Version      int              `json:"version"`
	RecordType   string           `json:"record_type"`
	RecordedAt   time.Time        `json:"recorded_at"`
	Kind         teleop.EventKind `json:"kind,omitempty"`
	Header       *teleop.Header   `json:"header,omitempty"`
	Payload      json.RawMessage  `json:"payload,omitempty"`
	EventCount   uint64           `json:"event_count,omitempty"`
	PreviousHash string           `json:"previous_hash,omitempty"`
	Hash         string           `json:"hash,omitempty"`

	Chain         string `json:"chain,omitempty"`
	ControlSchema string `json:"control_schema,omitempty"`
	Durability    string `json:"durability,omitempty"`
	EncodingError string `json:"encoding_error,omitempty"`

	// Fields below are format version 3 and later.

	// Session binds the manifest to the controller session it describes, so a
	// signed manifest cannot be moved to another log.
	Session *teleop.SessionID `json:"session,omitempty"`
	// TreeSize and TreeRoot describe the Merkle tree over every record written
	// before this one. They appear on manifests, checkpoints, and footers.
	TreeSize uint64 `json:"tree_size"`
	TreeRoot string `json:"tree_root,omitempty"`
	// Signature covers the tree head together with this record's own hash, so
	// it can neither be replayed onto another record nor detached from the
	// history beneath it.
	Signature  string      `json:"signature,omitempty"`
	KeyID      string      `json:"key_id,omitempty"`
	PublicKey  string      `json:"public_key,omitempty"`
	Provenance *Provenance `json:"provenance,omitempty"`
	// Reason explains why a checkpoint was taken.
	Reason string `json:"reason,omitempty"`

	// provenanceRaw retains the exact authenticated wire representation during
	// verification. It is unset while recording, where Provenance is marshaled
	// directly into the hash input.
	provenanceRaw json.RawMessage
}

// Record is the decoded, implementation-neutral representation of a verified
// event record.
type Record struct {
	// Version is the audit wire-format version.
	Version int
	// RecordedAt is when the recorder persisted the event.
	RecordedAt time.Time
	// Kind and Header identify the persisted event.
	Kind   teleop.EventKind
	Header teleop.Header
	// Payload is an isolated copy of the original event JSON.
	Payload json.RawMessage
	// PreviousHash and Hash are the encoded chain links.
	PreviousHash string
	Hash         string
	// EncodingError explains why Payload contains only the event header.
	EncodingError string
}

// Verification describes what was actually verified.
type Verification struct {
	// MatchedWitnesses counts independently supplied checkpoints matched to
	// exact authenticated records in this file. It does not authenticate the
	// witness's custody or receipt time; those come from the trusted collection.
	MatchedWitnesses uint64
	WitnessedEvents  uint64
	// WitnessedTreeSize includes the matched head record itself.
	WitnessedTreeSize uint64
	// Version is the stream's wire-format version.
	Version int
	// Chain names the integrity mechanism declared by the manifest.
	Chain string
	// Integrity reports a successfully verified SHA-256 or HMAC chain.
	Integrity bool
	// Authenticated reports a successfully verified HMAC chain.
	Authenticated bool
	// Complete reports that a valid footer terminated the stream.
	Complete bool
	// EventCount is the number of event records read.
	EventCount uint64
	// Durability is the recorder policy declared by the manifest.
	Durability string

	// Signed reports that every record read is covered by a tree head verified
	// against the public key the log itself declares. On its own this shows
	// internal consistency, not identity: a forger who replaces the whole log
	// can also replace the declared key.
	Signed bool
	// Trusted reports that every record read is covered by a tree head verified
	// against a key the caller supplied out of band. This is the property that
	// carries evidentiary weight.
	Trusted bool
	// SignedTreeSize is the number of leading records covered by the latest
	// tree head verified against the log's declared key.
	SignedTreeSize uint64
	// TrustedTreeSize is the number of leading records covered by the latest
	// tree head verified against PublicKey from VerifyOptions.
	TrustedTreeSize uint64
	// KeyID and PublicKey identify the declared signing key.
	KeyID     string
	PublicKey ed25519.PublicKey

	// TreeSize and TreeRoot are the Merkle head over every record read.
	TreeSize uint64
	TreeRoot []byte
	// Checkpoints counts verified intermediate tree heads.
	Checkpoints uint64

	// Provenance is the recorded build, host, and configuration context.
	Provenance *Provenance
	// Session is the controller session the log describes.
	Session teleop.SessionID
}

// VerifyOptions configures streaming verification.
type VerifyOptions struct {
	// Witnesses must be collected independently of the evidence producer.
	// Each must occur exactly in the verified file; a valid but truncated or
	// forked signed history is rejected. PublicKey is mandatory when supplied.
	Witnesses []Checkpoint
	// RequireWitness refuses verification without any retained checkpoints.
	RequireWitness bool
	// RequireFooter rejects an interrupted or still-open stream.
	RequireFooter bool
	// AllowUnverified permits a stream that explicitly declares no hash chain.
	AllowUnverified bool
	// RequireAuthentication rejects non-HMAC chains.
	RequireAuthentication bool
	// HMACKey authenticates an HMAC-SHA-256 chain.
	HMACKey []byte

	// PublicKey is the signing key the verifier trusts. Setting it requires a
	// version 3 signed stream, makes RequireSignature implicit, and withholds
	// events from consume until a signed tree head covers them.
	PublicKey ed25519.PublicKey
	// RequireSignature requires every returned or consumed event to be covered
	// by a version 3 signed tree head. Without PublicKey, the key declared by the
	// log proves internal consistency but not signer identity.
	RequireSignature bool
	// MaxPendingBytes bounds encoded event data withheld from consume while
	// waiting for a required signed tree head. Zero selects
	// DefaultMaxPendingBytes. Increase it only when a valid recorder is
	// intentionally configured with larger gaps between checkpoints.
	MaxPendingBytes uint64
}

// EvidenceStatus reports how far evidence has progressed through the recorder.
// AcceptedEvents have been appended successfully; Recorder.Record can still
// return an error if a following checkpoint fails. A controller may also have
// additional events waiting in its own queue, which the recorder cannot see.
// LocallyDurableEvents counts events followed by a successful Sync on the
// configured writer. WitnessedEvents and WitnessedTreeSize come from the most
// recent successful Anchor acknowledgement. Closed becomes true only after
// footer finalization and required-witness evaluation have completed.
type EvidenceStatus struct {
	AcceptedEvents       uint64
	LocallyDurableEvents uint64
	WitnessedEvents      uint64
	WitnessedTreeSize    uint64
	Durability           string
	WitnessConfigured    bool
	WitnessRequired      bool
	LastWitness          *WitnessReceipt
	Closed               bool
	Err                  error
}

// Recorder is a concurrency-safe, sticky-failure audit sink.
type Recorder struct {
	mu       sync.Mutex
	writer   *bufio.Writer
	raw      io.Writer
	options  Options
	previous string
	count    uint64
	started  bool
	closed   bool
	failure  error
	closeErr error

	closeOnce sync.Once
	closeDone chan struct{}

	key           *signingKey
	keyErr        error
	provenanceErr error
	tree          Tree
	anchors       *anchorRunner
	session       teleop.SessionID

	lastCheckpoint      time.Time
	sinceCheckpoint     uint64
	checkpointsRecorded uint64
	published           eventorder.HighWater
	durableCount        uint64

	lastAnchorAttempt    *Checkpoint
	witnessRetryCount    uint8
	witnessRetryCoverage uint64

	checkpointTimerMu  sync.Mutex
	checkpointStop     chan struct{}
	checkpointDone     chan struct{}
	checkpointStarted  bool
	checkpointStopping bool
}

var _ teleop.CanonicalEventSink = (*Recorder)(nil)

// NewRecorder returns an audit recorder that writes to writer. The default
// SHA-256 chain detects accidental corruption but is not authentic; use
// WithHMAC or WithSigner for an adversarial setting.
func NewRecorder(writer io.Writer, options ...Option) *Recorder {
	configured := Options{
		HashChain:          true,
		FlushEveryEvent:    true,
		Now:                time.Now,
		CheckpointInterval: DefaultCheckpointInterval,
		CheckpointEvery:    DefaultCheckpointEvery,
	}
	for _, option := range options {
		if option != nil {
			option(&configured)
		}
	}
	if configured.HMACKey != nil {
		key := make([]byte, len(configured.HMACKey))
		copy(key, configured.HMACKey)
		configured.HMACKey = key
	}
	var provenanceErr error
	if configured.Provenance != nil {
		provenance, err := freezeProvenance(*configured.Provenance)
		if err != nil {
			provenanceErr = err
			configured.Provenance = nil
		} else {
			configured.Provenance = &provenance
		}
	}
	if configured.Signer != nil || configured.Anchor != nil {
		// A signature or external anchor over an empty chain/tree cannot commit
		// to events. Preserve this invariant regardless of option order or
		// custom Options.
		configured.HashChain = true
	}
	if configured.Now == nil {
		configured.Now = time.Now
	}
	recorder := &Recorder{
		writer:        bufio.NewWriter(writer),
		raw:           writer,
		options:       configured,
		closeDone:     make(chan struct{}),
		published:     eventorder.New(teleop.MaxEventStreamsPerSession),
		provenanceErr: provenanceErr,
	}
	if configured.Signer != nil {
		key, err := newSigningKey(configured.Signer)
		if err != nil {
			recorder.keyErr = err
		} else {
			recorder.key = key
		}
	}
	if configured.Anchor != nil {
		recorder.anchors = newAnchorRunner(
			configured.Anchor,
			configured.AnchorQueue,
			configured.AnchorTimeout,
		)
	}
	return recorder
}

// PublicKey returns the signing key this recorder declares, or nil when the
// log is unsigned.
func (r *Recorder) PublicKey() ed25519.PublicKey {
	if r.key == nil {
		return nil
	}
	return append(ed25519.PublicKey(nil), r.key.public...)
}

// AnchorStats reports external anchoring health. Non-zero Failed or Dropped
// counts mean recent tree heads were never witnessed outside this process.
func (r *Recorder) AnchorStats() AnchorStats {
	if r.anchors == nil {
		return AnchorStats{}
	}
	return r.anchors.stats()
}

// EvidenceStatus returns a concurrency-safe snapshot of local durability and
// external witness coverage. Returned witness data is isolated from subsequent
// recorder updates.
func (r *Recorder) EvidenceStatus() EvidenceStatus {
	r.mu.Lock()
	completed := false
	select {
	case <-r.closeDone:
		completed = true
	default:
	}
	status := EvidenceStatus{
		AcceptedEvents:       r.count,
		LocallyDurableEvents: r.durableCount,
		Durability:           r.durability(),
		WitnessConfigured:    r.anchors != nil,
		WitnessRequired:      r.options.RequireWitness,
		Closed:               completed,
		Err: errors.Join(
			r.keyErr,
			r.provenanceErr,
			r.failure,
			r.closeErr,
		),
	}
	anchors := r.anchors
	r.mu.Unlock()

	if anchors == nil {
		return status
	}
	stats := anchors.stats()
	status.LastWitness = cloneWitnessReceipt(stats.LastReceipt)
	if status.LastWitness != nil {
		status.WitnessedEvents = status.LastWitness.Checkpoint.EventCount
		// A tree head describes the records before the head while its signature
		// also commits to the head's own chain hash.
		status.WitnessedTreeSize = status.LastWitness.Checkpoint.Size + 1
	}
	if stats.Failed > 0 || stats.Dropped > 0 {
		status.Err = errors.Join(
			status.Err,
			fmt.Errorf(
				"%w: %d witness publication(s) failed and %d were dropped",
				ErrWitnessRequired,
				stats.Failed,
				stats.Dropped,
			),
			stats.Err,
		)
	}
	return status
}

// WaitForWitness waits until an Anchor acknowledges a tree head covering at
// least eventCount events. A successful Anchor return is an acknowledgement by
// that adapter; callers must still decide whether its custody and persistence
// properties are trustworthy.
func (r *Recorder) WaitForWitness(
	ctx context.Context,
	eventCount uint64,
) (WitnessReceipt, error) {
	if ctx == nil {
		return WitnessReceipt{}, errors.New("teleop/audit: nil witness context")
	}
	r.mu.Lock()
	anchors := r.anchors
	r.mu.Unlock()
	if anchors == nil {
		return WitnessReceipt{}, ErrWitnessRequired
	}
	return anchors.wait(ctx, eventCount)
}

func (r *Recorder) startCheckpointTimer() {
	if r.options.CheckpointInterval <= 0 {
		return
	}
	r.checkpointTimerMu.Lock()
	defer r.checkpointTimerMu.Unlock()
	if r.checkpointStarted || r.checkpointStopping {
		return
	}
	r.checkpointStarted = true
	r.checkpointStop = make(chan struct{})
	r.checkpointDone = make(chan struct{})
	go r.runCheckpointTimer(
		r.options.CheckpointInterval,
		r.checkpointStop,
		r.checkpointDone,
	)
}

func (r *Recorder) runCheckpointTimer(
	interval time.Duration,
	stop <-chan struct{},
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.mu.Lock()
			if !r.closed && r.failure == nil && r.started {
				reason := ""
				switch {
				case r.sinceCheckpoint > 0:
					reason = "interval"
				case r.shouldRetryWitnessLocked():
					r.witnessRetryCount++
					reason = "witness retry"
				}
				if reason != "" {
					if err := r.checkpointLocked(reason); err != nil {
						r.failLocked(err)
					}
				}
			}
			r.mu.Unlock()
		case <-stop:
			return
		}
	}
}

func (r *Recorder) shouldRetryWitnessLocked() bool {
	if r.anchors == nil || r.lastAnchorAttempt == nil ||
		r.witnessRetryCount >= maxIdleWitnessRetries ||
		r.lastAnchorAttempt.EventCount != r.count {
		return false
	}
	return r.anchors.retryState(*r.lastAnchorAttempt) == anchorRetryEligible
}

func (r *Recorder) stopCheckpointTimer() {
	r.checkpointTimerMu.Lock()
	if r.checkpointStopping {
		done := r.checkpointDone
		r.checkpointTimerMu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	r.checkpointStopping = true
	if !r.checkpointStarted {
		r.checkpointTimerMu.Unlock()
		return
	}
	close(r.checkpointStop)
	done := r.checkpointDone
	r.checkpointTimerMu.Unlock()
	<-done
}

// Checkpoint forces a tree head immediately; WithSigner makes it signed.
// Callers should invoke it at moments worth being able to prove later, such as
// arming, an emergency stop, or a handover between operators. It returns
// ErrSessionRequired until the first event binds the recorder to a controller
// session.
func (r *Recorder) Checkpoint(reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.failure != nil {
		return errors.Join(ErrFailed, r.failure)
	}
	if !r.started {
		return ErrSessionRequired
	}
	if err := r.startLocked(); err != nil {
		return r.failLocked(err)
	}
	if err := r.checkpointLocked(reason); err != nil {
		return r.failLocked(err)
	}
	return nil
}

// CheckpointAndWait writes a checkpoint across the recorder's configured local
// flush/Sync barrier and waits until Anchor acknowledges that exact head or a
// later head from this recorder whose Merkle prefix includes it. Unlike
// WaitForWitness, this barrier cannot be satisfied by an older checkpoint that
// happens to carry the same event count.
//
// ctx bounds only the external acknowledgement wait. Once the local checkpoint
// has been written and queued, cancellation cannot retract it.
func (r *Recorder) CheckpointAndWait(
	ctx context.Context,
	reason string,
) (WitnessReceipt, error) {
	if ctx == nil {
		return WitnessReceipt{}, fmt.Errorf("%w: nil checkpoint context", teleop.ErrInvalidState)
	}
	if err := ctx.Err(); err != nil {
		return WitnessReceipt{}, err
	}

	var (
		anchors  *anchorRunner
		baseline AnchorStats
		target   Checkpoint
	)
	err := func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed {
			return ErrClosed
		}
		if r.failure != nil {
			return errors.Join(ErrFailed, r.failure)
		}
		if !r.started {
			return ErrSessionRequired
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.startLocked(); err != nil {
			return r.failLocked(err)
		}
		anchors = r.anchors
		if anchors != nil {
			baseline = anchors.stats()
		}
		var checkpointErr error
		target, checkpointErr = r.writeCheckpointLocked(reason)
		if checkpointErr != nil {
			return r.failLocked(checkpointErr)
		}
		return nil
	}()
	if err != nil {
		return WitnessReceipt{}, err
	}
	if anchors == nil {
		return WitnessReceipt{}, ErrWitnessRequired
	}
	return waitForCheckpointWitness(ctx, anchors, target, baseline)
}

// Record implements teleop.EventSink. It freezes event before admission and
// permanently fails the recorder if the event cannot be represented faithfully
// or would make the stream unverifiable.
func (r *Recorder) Record(ctx context.Context, event teleop.Event) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil record context", teleop.ErrInvalidState)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	canonical, err := teleop.FreezeEvent(event)
	if err != nil {
		return r.rejectEvent(errors.Join(ErrInvalidEvent, err))
	}
	return r.RecordCanonical(ctx, canonical)
}

// RecordCanonical implements teleop.CanonicalEventSink. The controller can
// freeze an event before its asynchronous sink queue, ensuring the bytes signed
// here are exactly the bytes admitted there. Direct Record callers receive the
// same validation through teleop.FreezeEvent.
func (r *Recorder) RecordCanonical(
	ctx context.Context,
	event teleop.CanonicalEvent,
) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil record context", teleop.ErrInvalidState)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot := eventSnapshot{
		header:  event.Header(),
		kind:    event.Kind(),
		payload: append(json.RawMessage(nil), event.JSON()...),
	}
	if err := validateEventSnapshot(snapshot); err != nil {
		return r.rejectEvent(err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.failure != nil {
		return errors.Join(ErrFailed, r.failure)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.validateEventOrderLocked(snapshot.header); err != nil {
		return r.failLocked(err)
	}
	if snapshot.header.ID.Session == (teleop.SessionID{}) {
		return r.failLocked(ErrSessionRequired)
	}
	// Bind the session before the manifest is written so a signed manifest
	// cannot be transplanted onto a different session's records.
	if !r.started {
		r.session = snapshot.header.ID.Session
	} else if snapshot.header.ID.Session != r.session {
		return r.failLocked(errors.New("teleop/audit: event session changed"))
	}
	if err := r.startLocked(); err != nil {
		return r.failLocked(err)
	}
	now, err := r.now()
	if err != nil {
		return r.failLocked(err)
	}
	record := diskRecord{
		Version:    FormatVersion,
		RecordType: "event",
		RecordedAt: now,
		Kind:       snapshot.kind,
		Payload:    snapshot.payload,
	}
	if err := r.writeLocked(&record); err != nil {
		return r.failLocked(err)
	}
	if !r.published.Advance(
		snapshot.header.ID.Stream,
		snapshot.header.ID.Sequence,
	) {
		return r.failLocked(fmt.Errorf(
			"%w: event order changed during admission",
			ErrInvalidEvent,
		))
	}
	r.count++
	if r.options.FlushEveryEvent && r.syncCapable() {
		r.durableCount = r.count
	}
	r.sinceCheckpoint++
	if r.checkpointDueLocked(now) {
		if err := r.checkpointLocked("interval"); err != nil {
			return r.failLocked(err)
		}
	}
	r.startCheckpointTimer()
	return nil
}

type eventSnapshot struct {
	header  teleop.Header
	kind    teleop.EventKind
	payload json.RawMessage
}

func validateEventSnapshot(snapshot eventSnapshot) error {
	if snapshot.kind == "" {
		return fmt.Errorf("%w: event kind is empty", ErrInvalidEvent)
	}
	if len(snapshot.payload) == 0 || !json.Valid(snapshot.payload) {
		return fmt.Errorf("%w: event payload is not valid JSON", ErrInvalidEvent)
	}
	if err := rejectDuplicateJSONFields(snapshot.payload); err != nil {
		return fmt.Errorf("%w: event payload: %v", ErrInvalidEvent, err)
	}
	var envelope struct {
		Header *teleop.Header `json:"header"`
	}
	decoder := json.NewDecoder(bytes.NewReader(snapshot.payload))
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("%w: decode event header: %v", ErrInvalidEvent, err)
	}
	if envelope.Header == nil {
		return fmt.Errorf("%w: event payload has no header", ErrInvalidEvent)
	}
	expected, err := json.Marshal(snapshot.header)
	if err != nil {
		return fmt.Errorf("%w: encode Event.Header: %v", ErrInvalidEvent, err)
	}
	actual, err := json.Marshal(envelope.Header)
	if err != nil {
		return fmt.Errorf("%w: encode payload header: %v", ErrInvalidEvent, err)
	}
	if !bytes.Equal(expected, actual) {
		return fmt.Errorf("%w: payload header differs from Event.Header", ErrInvalidEvent)
	}
	return nil
}

func (r *Recorder) validateEventOrderLocked(header teleop.Header) error {
	if header.ID.Session == (teleop.SessionID{}) {
		return ErrSessionRequired
	}
	if header.ID.Stream == "" || header.ID.Sequence == 0 {
		return fmt.Errorf("%w: invalid event ID", ErrInvalidEvent)
	}
	want := r.published.Expected(header.ID.Stream)
	if want == 0 {
		return fmt.Errorf(
			"%w: event stream limit %d reached or stream %q exhausted",
			ErrInvalidEvent,
			teleop.MaxEventStreamsPerSession,
			header.ID.Stream,
		)
	}
	if header.ID.Sequence != want {
		return fmt.Errorf(
			"%w: stream %q sequence %d follows %d",
			ErrInvalidEvent,
			header.ID.Stream,
			header.ID.Sequence,
			r.published.Through(header.ID.Stream),
		)
	}
	for _, cause := range header.Causes {
		if cause.Session != header.ID.Session ||
			!r.published.Contains(cause.Stream, cause.Sequence) {
			return fmt.Errorf("%w: cause %v does not precede event", ErrInvalidEvent, cause)
		}
	}
	return nil
}

func (r *Recorder) rejectEvent(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.failure != nil {
		return errors.Join(ErrFailed, r.failure)
	}
	return r.failLocked(err)
}

func (r *Recorder) checkpointDueLocked(now time.Time) bool {
	if r.options.CheckpointEvery > 0 && r.sinceCheckpoint >= r.options.CheckpointEvery {
		return true
	}
	if r.options.CheckpointInterval > 0 && !r.lastCheckpoint.IsZero() {
		return now.Sub(r.lastCheckpoint) >= r.options.CheckpointInterval
	}
	return false
}

// checkpointLocked writes a tree head covering every record before it.
func (r *Recorder) checkpointLocked(reason string) error {
	_, err := r.writeCheckpointLocked(reason)
	return err
}

func (r *Recorder) writeCheckpointLocked(reason string) (Checkpoint, error) {
	now, err := r.now()
	if err != nil {
		return Checkpoint{}, err
	}
	checkpoint := diskRecord{
		Version:    FormatVersion,
		RecordType: "checkpoint",
		RecordedAt: now,
		EventCount: r.count,
		Reason:     reason,
	}
	if err := r.writeLocked(&checkpoint); err != nil {
		return Checkpoint{}, err
	}
	r.checkpointsRecorded++
	r.sinceCheckpoint = 0
	r.lastCheckpoint = checkpoint.RecordedAt
	return r.publishAnchorLocked(checkpoint), nil
}

func (r *Recorder) publishAnchorLocked(record diskRecord) Checkpoint {
	checkpoint := Checkpoint{
		Version:    record.Version,
		RecordType: record.RecordType,
		Session:    r.session,
		Size:       record.TreeSize,
		Root:       record.TreeRoot,
		ChainHead:  record.Hash,
		EventCount: record.EventCount,
		RecordedAt: record.RecordedAt,
		Signature:  record.Signature,
	}
	if r.key != nil {
		checkpoint.SignatureAlgorithm = SignatureAlgorithmEd25519
		checkpoint.KeyID = r.key.id
		checkpoint.PublicKey = hex.EncodeToString(r.key.public)
	}
	if r.anchors != nil {
		if checkpoint.RecordType == "checkpoint" {
			if checkpoint.EventCount != r.witnessRetryCoverage {
				r.witnessRetryCoverage = checkpoint.EventCount
				r.witnessRetryCount = 0
			}
			attempt := checkpoint
			r.lastAnchorAttempt = &attempt
		}
		r.anchors.publish(checkpoint)
	}
	return checkpoint
}

func waitForCheckpointWitness(
	ctx context.Context,
	anchors *anchorRunner,
	target Checkpoint,
	baseline AnchorStats,
) (WitnessReceipt, error) {
	for {
		stats, changed, stopped := anchors.observation()
		if stats.Failed > baseline.Failed {
			return WitnessReceipt{}, errors.Join(ErrWitnessRequired, stats.Err)
		}
		if stats.Dropped > baseline.Dropped {
			return WitnessReceipt{}, fmt.Errorf(
				"%w: %d checkpoint publication(s) were dropped while waiting",
				ErrWitnessRequired,
				stats.Dropped-baseline.Dropped,
			)
		}
		if stats.LastReceipt != nil && checkpointWitnessCovers(
			stats.LastReceipt.Checkpoint,
			target,
		) {
			return *cloneWitnessReceipt(stats.LastReceipt), nil
		}
		if stopped {
			return WitnessReceipt{}, fmt.Errorf(
				"%w: recorder closed before checkpoint %s was acknowledged",
				ErrWitnessRequired,
				target.ChainHead,
			)
		}
		select {
		case <-ctx.Done():
			return WitnessReceipt{}, ctx.Err()
		case <-changed:
		case <-anchors.done:
		}
	}
}

func checkpointWitnessCovers(receipt, target Checkpoint) bool {
	if receipt.Session != target.Session || receipt.EventCount < target.EventCount {
		return false
	}
	if target.ChainHead != "" && receipt.ChainHead == target.ChainHead {
		return true
	}
	// target itself becomes leaf target.Size. A later recorder head with a
	// larger preceding-tree size therefore commits to target in its Merkle root.
	return receipt.Size > target.Size
}

// treeHead builds the statement signed for a manifest, checkpoint, or footer.
func (r *Recorder) treeHead(record diskRecord) TreeHead {
	root, _ := hex.DecodeString(record.TreeRoot)
	return TreeHead{
		Version:    record.Version,
		RecordType: record.RecordType,
		Session:    r.session,
		Size:       record.TreeSize,
		Root:       root,
		ChainHead:  record.Hash,
		EventCount: record.EventCount,
		RecordedAt: record.RecordedAt,
	}
}

// Close writes a footer and makes any failure sticky. A failed Close is safe to
// retry: it returns the same error without appending a duplicate footer.
func (r *Recorder) Close() error {
	r.closeOnce.Do(r.closeInternal)
	<-r.closeDone
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeErr
}

func (r *Recorder) closeInternal() {
	defer close(r.closeDone)
	r.stopCheckpointTimer()

	r.mu.Lock()
	r.closed = true
	var footer *diskRecord
	if r.failure == nil {
		if err := r.startLocked(); err != nil {
			r.failLocked(err)
		} else {
			now, nowErr := r.now()
			if nowErr != nil {
				r.failLocked(nowErr)
			} else {
				candidate := diskRecord{
					Version:    FormatVersion,
					RecordType: "footer",
					RecordedAt: now,
					EventCount: r.count,
				}
				if err := r.writeLocked(&candidate); err != nil {
					r.failLocked(err)
				} else {
					footer = &candidate
					r.publishAnchorLocked(candidate)
					if err := r.flushAndSync(); err != nil {
						r.failLocked(err)
					} else if r.syncCapable() {
						r.durableCount = r.count
					}
				}
			}
		}
	}
	anchors := r.anchors
	r.mu.Unlock()

	// Drain acknowledgements without holding the recorder mutex. Anchor adapters
	// may inspect recorder status as part of their own observability.
	if anchors != nil {
		anchors.stop()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var result error
	if r.failure != nil {
		result = errors.Join(ErrFailed, r.failure)
	}
	if r.options.RequireWitness {
		result = errors.Join(result, r.requiredWitnessErrorLocked(footer))
	}
	r.clearKeyMaterialLocked()
	r.closeErr = result
}

func (r *Recorder) requiredWitnessErrorLocked(footer *diskRecord) error {
	if r.anchors == nil {
		return fmt.Errorf("%w: no Anchor is configured", ErrWitnessRequired)
	}
	stats := r.anchors.stats()
	if stats.Failed > 0 || stats.Dropped > 0 {
		return errors.Join(
			fmt.Errorf(
				"%w: %d publication(s) failed and %d were dropped",
				ErrWitnessRequired,
				stats.Failed,
				stats.Dropped,
			),
			stats.Err,
		)
	}
	if footer == nil || stats.LastReceipt == nil {
		return fmt.Errorf("%w: completed footer was not acknowledged", ErrWitnessRequired)
	}
	acknowledged := stats.LastReceipt.Checkpoint
	if acknowledged.RecordType != "footer" ||
		acknowledged.Session != r.session ||
		acknowledged.EventCount != r.count ||
		acknowledged.ChainHead != footer.Hash {
		return fmt.Errorf("%w: final acknowledgement does not cover the footer", ErrWitnessRequired)
	}
	return nil
}

func (r *Recorder) startLocked() error {
	if r.started {
		return nil
	}
	if r.keyErr != nil {
		return r.keyErr
	}
	if r.provenanceErr != nil {
		return r.provenanceErr
	}
	now, err := r.now()
	if err != nil {
		return err
	}
	r.started = true
	manifest := diskRecord{
		Version:       FormatVersion,
		RecordType:    "manifest",
		RecordedAt:    now,
		Chain:         r.chain(),
		ControlSchema: ControlSchemaVersion,
		Durability:    r.durability(),
		Provenance:    r.options.Provenance,
	}
	if r.session != (teleop.SessionID{}) {
		session := r.session
		manifest.Session = &session
	}
	if r.key != nil {
		manifest.KeyID = r.key.id
		manifest.PublicKey = hex.EncodeToString(r.key.public)
	}
	r.lastCheckpoint = manifest.RecordedAt
	if err := r.writeLocked(&manifest); err != nil {
		return err
	}
	r.publishAnchorLocked(manifest)
	return nil
}

func (r *Recorder) chain() string {
	switch {
	case r.options.HMACKey != nil:
		return chainHMAC
	case r.options.HashChain:
		return chainSHA256
	default:
		return chainNone
	}
}

func (r *Recorder) durability() string {
	if !r.options.FlushEveryEvent {
		return "buffered"
	}
	if r.syncCapable() {
		return "fsync-every-record"
	}
	return "flush-every-record"
}

func (r *Recorder) syncCapable() bool {
	_, ok := r.raw.(interface{ Sync() error })
	return ok
}

func (r *Recorder) now() (now time.Time, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			now = time.Time{}
			err = fmt.Errorf("%w: audit clock: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	return r.options.Now().UTC(), nil
}

func (r *Recorder) writeLocked(record *diskRecord) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: audit writer or signer: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	record.PreviousHash = ""
	record.Hash = ""
	record.Signature = ""

	// Manifests, checkpoints, and footers carry the Merkle head over every
	// record already written. Set it before hashing so the hash covers it, and
	// sign after hashing so the signature commits to the record's own hash.
	head := record.RecordType != "event"
	if head {
		record.TreeSize = r.tree.Size()
		record.TreeRoot = hex.EncodeToString(r.tree.Root())
	}

	chain := r.chain()
	if chain != chainNone {
		record.PreviousHash = r.previous
		value, err := recordHashV3(*record, chain, r.options.HMACKey)
		if err != nil {
			return err
		}
		record.Hash = value
	}
	if head && r.key != nil {
		signature, err := r.key.sign(r.treeHead(*record).signingBytes())
		if err != nil {
			return err
		}
		record.Signature = signature
	}

	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}
	if len(encoded)+1 > MaxRecordSize {
		return fmt.Errorf("%w: %d bytes", ErrRecordTooLarge, len(encoded)+1)
	}
	if _, err := r.writer.Write(encoded); err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	if err := r.writer.WriteByte('\n'); err != nil {
		return fmt.Errorf("write audit record delimiter: %w", err)
	}
	if r.options.FlushEveryEvent {
		if err := r.flushAndSync(); err != nil {
			return err
		}
	}
	if chain != chainNone {
		r.previous = record.Hash
		raw, decodeErr := hex.DecodeString(record.Hash)
		if decodeErr != nil {
			return fmt.Errorf("decode audit record hash: %w", decodeErr)
		}
		// Every record becomes a leaf, so the tree covers the whole log and a
		// later head commits to all earlier heads.
		r.tree.Append(HashLeaf(raw))
	}
	return nil
}

func (r *Recorder) failLocked(err error) error {
	if r.failure == nil {
		r.failure = err
	}
	r.clearKeyMaterialLocked()
	return errors.Join(ErrFailed, r.failure)
}

func (r *Recorder) clearKeyMaterialLocked() {
	clear(r.options.HMACKey)
	r.options.HMACKey = nil
	r.options.Signer = nil
	if r.key != nil {
		// Retain the public identity for PublicKey while releasing the private
		// signer reference.
		r.key.signer = nil
	}
}

func (r *Recorder) flushAndSync() (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: audit writer: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	if err := r.writer.Flush(); err != nil {
		return fmt.Errorf("flush audit log: %w", err)
	}
	if syncer, ok := r.raw.(interface{ Sync() error }); ok {
		if err := syncer.Sync(); err != nil {
			return fmt.Errorf("sync audit log: %w", err)
		}
	}
	return nil
}

// ReadAll decodes and verifies a complete, hash-chained stream. It establishes
// integrity, not signer identity; use ReadTrusted or ReadAuthenticated when
// authenticity is required.
func ReadAll(reader io.Reader) ([]Record, error) {
	records, _, err := Read(reader, VerifyOptions{RequireFooter: true})
	return records, err
}

// ReadAuthenticated verifies a complete HMAC stream with key. The caller must
// obtain and protect key outside the log.
func ReadAuthenticated(reader io.Reader, key []byte) ([]Record, Verification, error) {
	return Read(reader, VerifyOptions{
		RequireFooter:         true,
		RequireAuthentication: true,
		HMACKey:               key,
	})
}

// ReadTrusted verifies a complete version 3 stream against a public key
// obtained outside the log. It rejects unsigned records, unsigned tails,
// legacy formats, missing footers, and a self-declared replacement key.
// Streams created with both WithSigner and WithHMAC must instead use Read with
// both VerifyOptions.PublicKey and VerifyOptions.HMACKey.
func ReadTrusted(
	reader io.Reader,
	public ed25519.PublicKey,
) ([]Record, Verification, error) {
	if len(public) != ed25519.PublicKeySize {
		return nil, Verification{}, fmt.Errorf(
			"%w: trusted public key is %d bytes",
			ErrSignature,
			len(public),
		)
	}
	records, verification, err := Read(reader, VerifyOptions{
		RequireFooter:    true,
		RequireSignature: true,
		PublicKey:        append(ed25519.PublicKey(nil), public...),
	})
	if err != nil {
		return nil, verification, err
	}
	return records, verification, nil
}

// ReadPartial verifies all complete records and permits a missing or torn
// footer. Valid integrity-chained prefix records are retained when a later
// record is invalid. It does not require signature coverage.
func ReadPartial(reader io.Reader) ([]Record, error) {
	records, _, err := Read(reader, VerifyOptions{})
	return records, err
}

// Read verifies reader and accumulates the event records accepted under
// options. On error, callers must use only the returned prefix whose security
// properties are explicitly reported by Verification.
func Read(reader io.Reader, options VerifyOptions) ([]Record, Verification, error) {
	records := make([]Record, 0)
	verification, err := Verify(reader, options, func(record Record) error {
		records = append(records, record)
		return nil
	})
	return records, verification, err
}

// Verify checks a stream incrementally and invokes consume for each accepted
// event. When signature verification is required, events are buffered until a
// verified tree head covers them; an unsigned tail is never passed to consume.
// Without signature requirements, consume receives integrity-checked events as
// they are read.
func Verify(
	reader io.Reader,
	options VerifyOptions,
	consume func(Record) error,
) (Verification, error) {
	witnesses, err := prepareWitnessVerification(options)
	if err != nil {
		return Verification{}, err
	}
	signatureRequired := options.RequireSignature || len(options.PublicKey) > 0
	if len(options.PublicKey) > 0 && len(options.PublicKey) != ed25519.PublicKeySize {
		return Verification{}, fmt.Errorf(
			"%w: trusted public key is %d bytes",
			ErrSignature,
			len(options.PublicKey),
		)
	}
	options.HMACKey = append([]byte(nil), options.HMACKey...)
	defer clear(options.HMACKey)
	options.PublicKey = append(ed25519.PublicKey(nil), options.PublicKey...)
	maxPendingBytes := options.MaxPendingBytes
	if maxPendingBytes == 0 {
		maxPendingBytes = DefaultMaxPendingBytes
	}
	buffered := bufio.NewReaderSize(reader, 64*1024)
	var (
		verification   Verification
		previous       string
		lineNumber     int
		footer         bool
		legacy         bool
		published      = eventorder.New(teleop.MaxEventStreamsPerSession)
		session        *teleop.SessionID
		tree           Tree
		trusted        ed25519.PublicKey
		headSession    teleop.SessionID
		headSessionSet bool
		signedThrough  uint64
		trustedThrough uint64
		pending        []Record
		pendingBytes   uint64
	)
	updateCoverage := func() {
		verification.SignedTreeSize = signedThrough
		verification.TrustedTreeSize = trustedThrough
		verification.Signed = tree.Size() > 0 && signedThrough == tree.Size()
		verification.Trusted = tree.Size() > 0 &&
			len(options.PublicKey) > 0 &&
			trustedThrough == tree.Size()
	}
	releasePending := func() error {
		for _, record := range pending {
			if consume != nil {
				if err := consume(record); err != nil {
					return err
				}
			}
		}
		clear(pending)
		pending = pending[:0]
		pendingBytes = 0
		return nil
	}
	for {
		line, complete, err := nextLine(buffered)
		if err != nil && !errors.Is(err, io.EOF) {
			return verification, fmt.Errorf("read audit log: %w", err)
		}
		if len(line) == 0 && errors.Is(err, io.EOF) {
			break
		}
		lineNumber++
		if !complete {
			if options.RequireFooter {
				return verification, fmt.Errorf("%w: torn audit line %d", ErrIncomplete, lineNumber)
			}
			break
		}
		disk, decodeErr := decodeDiskRecord(line)
		if decodeErr != nil {
			return verification, fmt.Errorf("decode audit line %d: %w", lineNumber, decodeErr)
		}
		if disk.Version < 1 || disk.Version > FormatVersion {
			return verification, fmt.Errorf(
				"audit line %d: unsupported version %d (supported 1-%d)",
				lineNumber,
				disk.Version,
				FormatVersion,
			)
		}
		if lineNumber == 1 {
			verification.Version = disk.Version
			if signatureRequired && disk.Version < 3 {
				return verification, fmt.Errorf(
					"%w: format version %d cannot carry signed tree heads",
					ErrSignatureRequired,
					disk.Version,
				)
			}
			legacy = disk.Version == 1
			if legacy {
				verification.Chain = chainSHA256
			} else {
				if disk.RecordType != "manifest" {
					return verification, fmt.Errorf("audit line 1: manifest is required")
				}
				if disk.ControlSchema != ControlSchemaVersion {
					return verification, fmt.Errorf(
						"audit line 1: unsupported control schema %q",
						disk.ControlSchema,
					)
				}
				verification.Chain = disk.Chain
				verification.Durability = disk.Durability
				verification.Provenance = disk.Provenance
				if disk.Session != nil {
					headSession = *disk.Session
					if headSession == (teleop.SessionID{}) {
						return verification, fmt.Errorf(
							"%w: audit line 1 declares a zero controller session",
							ErrSessionRequired,
						)
					}
					headSessionSet = true
					verification.Session = headSession
				}
				if disk.PublicKey != "" {
					public, keyErr := decodePublicKey(disk.PublicKey)
					if keyErr != nil {
						return verification, fmt.Errorf("audit line 1: %w", keyErr)
					}
					if disk.KeyID == "" || disk.KeyID != KeyID(public) {
						return verification, fmt.Errorf(
							"%w: audit line 1 key identifier does not match its public key",
							ErrUntrustedKey,
						)
					}
					verification.PublicKey = public
					verification.KeyID = disk.KeyID
				} else if disk.KeyID != "" {
					return verification, fmt.Errorf(
						"%w: audit line 1 declares a key identifier without a public key",
						ErrUntrustedKey,
					)
				}
			}
			if options.RequireAuthentication && verification.Chain != chainHMAC {
				return verification, ErrUnauthenticated
			}
			if verification.Chain == chainNone && !options.AllowUnverified {
				return verification, ErrUnverified
			}
			if verification.Chain == chainHMAC && len(options.HMACKey) == 0 {
				return verification, ErrAuthenticationRequired
			}
			// A key the log declares about itself proves only internal
			// consistency. A key the caller supplies is what makes the
			// signature evidence, so a mismatch is fatal rather than ignored.
			if len(options.PublicKey) > 0 {
				if verification.PublicKey == nil {
					return verification, fmt.Errorf(
						"%w: log declares no signing key",
						ErrSignatureRequired,
					)
				}
				if !verification.PublicKey.Equal(options.PublicKey) {
					return verification, ErrUntrustedKey
				}
				trusted = options.PublicKey
			} else if verification.PublicKey != nil {
				trusted = verification.PublicKey
			}
			if verification.PublicKey != nil && verification.Chain == chainNone {
				return verification, fmt.Errorf(
					"%w: a signed stream requires an integrity chain",
					ErrSignature,
				)
			}
			if signatureRequired && trusted == nil {
				return verification, ErrSignatureRequired
			}
		} else if disk.Version != verification.Version {
			return verification, fmt.Errorf("audit line %d: version changed within stream", lineNumber)
		}
		if footer {
			return verification, fmt.Errorf("audit line %d: data follows footer", lineNumber)
		}

		if verification.Chain != chainNone {
			if disk.Hash == "" {
				return verification, fmt.Errorf("audit line %d: required hash is missing", lineNumber)
			}
			if disk.PreviousHash != previous {
				return verification, fmt.Errorf("audit line %d: hash chain is discontinuous", lineNumber)
			}
			var expected string
			var hashErr error
			switch {
			case legacy:
				expected, hashErr = recordHashV1(disk)
			case disk.Version == 2:
				expected, hashErr = recordHashV2(
					disk,
					verification.Chain,
					options.HMACKey,
				)
			default:
				expected, hashErr = recordHashV3(
					disk,
					verification.Chain,
					options.HMACKey,
				)
			}
			if hashErr != nil {
				return verification, hashErr
			}
			if !hmac.Equal([]byte(disk.Hash), []byte(expected)) {
				return verification, fmt.Errorf("audit line %d: hash mismatch", lineNumber)
			}
			previous = disk.Hash
		} else if disk.Hash != "" || disk.PreviousHash != "" {
			return verification, fmt.Errorf("audit line %d: unchained manifest contains hashes", lineNumber)
		}

		// Manifests, checkpoints, and footers assert a Merkle head over
		// everything before them. Checking it here, before this record joins
		// the tree, is what detects a log whose prefix was rewritten or whose
		// middle records were removed.
		head := disk.RecordType != "event"
		headVerified := false
		if head && disk.Version >= 3 && trusted == nil && disk.Signature != "" {
			return verification, fmt.Errorf(
				"%w: %s at audit line %d has a signature but no declared public key",
				ErrSignature,
				disk.RecordType,
				lineNumber,
			)
		}
		if head && disk.Version >= 3 && verification.Chain != chainNone {
			if disk.TreeSize != tree.Size() {
				return verification, fmt.Errorf(
					"%w: audit line %d claims tree size %d over %d records",
					ErrTreeMismatch,
					lineNumber,
					disk.TreeSize,
					tree.Size(),
				)
			}
			if disk.TreeRoot != hex.EncodeToString(tree.Root()) {
				return verification, fmt.Errorf(
					"%w: audit line %d root does not cover the preceding records",
					ErrTreeMismatch,
					lineNumber,
				)
			}
			if trusted != nil {
				if disk.Signature == "" {
					return verification, fmt.Errorf(
						"%w: unsigned %s at audit line %d",
						ErrSignatureRequired,
						disk.RecordType,
						lineNumber,
					)
				}
				root, decodeErr := hex.DecodeString(disk.TreeRoot)
				if decodeErr != nil {
					return verification, fmt.Errorf(
						"audit line %d: decode tree root: %w",
						lineNumber,
						decodeErr,
					)
				}
				signed := TreeHead{
					Version:    disk.Version,
					RecordType: disk.RecordType,
					Session:    headSession,
					Size:       disk.TreeSize,
					Root:       root,
					ChainHead:  disk.Hash,
					EventCount: disk.EventCount,
					RecordedAt: disk.RecordedAt,
				}
				if sigErr := VerifyTreeHead(trusted, signed, disk.Signature); sigErr != nil {
					return verification, fmt.Errorf("audit line %d: %w", lineNumber, sigErr)
				}
				headVerified = true
				signedThrough = tree.Size() + 1
				if len(options.PublicKey) > 0 {
					trustedThrough = tree.Size() + 1
				}
			}
		}
		if witness, ok := witnesses[tree.Size()]; ok {
			if !headVerified || !witnessMatchesRecord(witness, disk, headSession) {
				return verification, fmt.Errorf("%w: checkpoint at record %d", ErrWitnessMismatch, tree.Size())
			}
			verification.MatchedWitnesses++
			verification.WitnessedEvents = max(verification.WitnessedEvents, witness.EventCount)
			verification.WitnessedTreeSize = max(verification.WitnessedTreeSize, tree.Size()+1)
			delete(witnesses, tree.Size())
		}
		if disk.Version >= 3 && verification.Chain != chainNone {
			raw, decodeErr := hex.DecodeString(disk.Hash)
			if decodeErr != nil {
				return verification, fmt.Errorf(
					"audit line %d: decode record hash: %w",
					lineNumber,
					decodeErr,
				)
			}
			tree.Append(HashLeaf(raw))
			verification.TreeSize = tree.Size()
			updateCoverage()
		}

		switch disk.RecordType {
		case "manifest":
			if legacy || lineNumber != 1 {
				return verification, fmt.Errorf("audit line %d: misplaced manifest", lineNumber)
			}
			if signatureRequired && headVerified {
				if err := releasePending(); err != nil {
					return verification, err
				}
			}
			continue
		case "checkpoint":
			if disk.Version < 3 {
				return verification, fmt.Errorf(
					"audit line %d: checkpoint requires format version 3",
					lineNumber,
				)
			}
			if disk.EventCount != verification.EventCount {
				return verification, fmt.Errorf(
					"audit line %d: checkpoint count %d does not match %d events",
					lineNumber,
					disk.EventCount,
					verification.EventCount,
				)
			}
			verification.Checkpoints++
			if signatureRequired && headVerified {
				if err := releasePending(); err != nil {
					return verification, err
				}
			}
			continue
		case "footer":
			if disk.EventCount != verification.EventCount {
				return verification, fmt.Errorf(
					"audit line %d: footer count %d does not match %d events",
					lineNumber,
					disk.EventCount,
					verification.EventCount,
				)
			}
			footer = true
			verification.Complete = true
			if signatureRequired && headVerified {
				if err := releasePending(); err != nil {
					return verification, err
				}
			}
			continue
		case "event":
		default:
			return verification, fmt.Errorf(
				"audit line %d: unknown record type %q",
				lineNumber,
				disk.RecordType,
			)
		}
		if disk.Kind == "" {
			return verification, fmt.Errorf("audit line %d: event kind is empty", lineNumber)
		}

		header, headerErr := eventHeader(disk)
		if headerErr != nil {
			return verification, fmt.Errorf("audit line %d: %w", lineNumber, headerErr)
		}
		if disk.Version >= 3 {
			if !headSessionSet {
				return verification, fmt.Errorf(
					"%w: audit line 1 does not bind a controller session",
					ErrSessionRequired,
				)
			}
			if header.ID.Session != headSession {
				return verification, fmt.Errorf(
					"audit line %d: event session does not match manifest",
					lineNumber,
				)
			}
		}
		if session == nil {
			value := header.ID.Session
			session = &value
			if verification.Session == (teleop.SessionID{}) {
				verification.Session = value
			}
		} else if header.ID.Session != *session {
			return verification, fmt.Errorf("audit line %d: session changed within stream", lineNumber)
		}
		if header.ID.Stream == "" || header.ID.Sequence == 0 {
			return verification, fmt.Errorf("audit line %d: invalid event ID", lineNumber)
		}
		wantSequence := published.Expected(header.ID.Stream)
		if wantSequence == 0 {
			return verification, fmt.Errorf(
				"audit line %d: event stream limit %d reached or stream %q exhausted",
				lineNumber,
				teleop.MaxEventStreamsPerSession,
				header.ID.Stream,
			)
		}
		if header.ID.Sequence != wantSequence {
			return verification, fmt.Errorf(
				"audit line %d: stream %q sequence %d follows %d",
				lineNumber,
				header.ID.Stream,
				header.ID.Sequence,
				published.Through(header.ID.Stream),
			)
		}
		for _, cause := range header.Causes {
			if cause.Session != header.ID.Session ||
				!published.Contains(cause.Stream, cause.Sequence) {
				return verification, fmt.Errorf(
					"audit line %d: cause %v does not precede event",
					lineNumber,
					cause,
				)
			}
		}
		if !published.Advance(header.ID.Stream, header.ID.Sequence) {
			return verification, fmt.Errorf(
				"audit line %d: event order changed during verification",
				lineNumber,
			)
		}
		verification.EventCount++
		encodingError := ""
		if disk.Version >= 2 {
			encodingError = disk.EncodingError
		}
		record := Record{
			Version:       disk.Version,
			RecordedAt:    disk.RecordedAt,
			Kind:          disk.Kind,
			Header:        header,
			Payload:       append(json.RawMessage(nil), disk.Payload...),
			PreviousHash:  disk.PreviousHash,
			Hash:          disk.Hash,
			EncodingError: encodingError,
		}
		if signatureRequired {
			if consume != nil {
				recordBytes := uint64(len(line))
				if recordBytes > maxPendingBytes ||
					pendingBytes > maxPendingBytes-recordBytes {
					return verification, fmt.Errorf(
						"%w: more than %d bytes await a signed tree head",
						ErrPendingLimit,
						maxPendingBytes,
					)
				}
				pendingBytes += recordBytes
				pending = append(pending, record)
			}
		} else if consume != nil {
			if consumeErr := consume(record); consumeErr != nil {
				return verification, consumeErr
			}
		}
	}
	if verification.Version == 0 {
		return verification, errors.New("audit log is empty")
	}
	if options.RequireFooter && !footer {
		return verification, fmt.Errorf("%w: footer is missing", ErrIncomplete)
	}
	verification.Integrity = verification.Chain == chainSHA256 ||
		verification.Chain == chainHMAC
	verification.Authenticated = verification.Chain == chainHMAC
	verification.TreeSize = tree.Size()
	verification.TreeRoot = tree.Root()
	updateCoverage()
	if signatureRequired && !verification.Signed {
		return verification, fmt.Errorf(
			"%w: %d trailing record(s) are not covered by a signed tree head",
			ErrSignatureRequired,
			tree.Size()-signedThrough,
		)
	}
	if len(witnesses) > 0 {
		return verification, fmt.Errorf("%w: %d retained checkpoints are absent from the file", ErrWitnessMismatch, len(witnesses))
	}
	return verification, nil
}

func nextLine(reader *bufio.Reader) ([]byte, bool, error) {
	line := make([]byte, 0, 64*1024)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > MaxRecordSize {
			return nil, false, ErrRecordTooLarge
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line[:len(line)-1], true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, false, io.EOF
		default:
			return line, false, err
		}
	}
}

func eventHeader(record diskRecord) (teleop.Header, error) {
	if record.Version == 1 && record.Header != nil {
		return record.Header.Clone(), nil
	}
	var envelope struct {
		Header teleop.Header `json:"header"`
	}
	if err := json.Unmarshal(record.Payload, &envelope); err != nil {
		return teleop.Header{}, fmt.Errorf("decode event header: %w", err)
	}
	return envelope.Header, nil
}

func recordHashV2(record diskRecord, chain string, key []byte) (string, error) {
	var digest hash.Hash
	switch chain {
	case chainSHA256:
		digest = sha256.New()
	case chainHMAC:
		if len(key) == 0 {
			return "", ErrAuthenticationRequired
		}
		digest = hmac.New(sha256.New, key)
	default:
		return "", fmt.Errorf("teleop/audit: unsupported chain %q", chain)
	}
	writeHashField(digest, []byte("teleop-audit-v2"))
	writeHashField(digest, []byte(fmt.Sprintf("%d", record.Version)))
	writeHashField(digest, []byte(record.RecordType))
	writeHashField(digest, []byte(record.RecordedAt.UTC().Format(time.RFC3339Nano)))
	writeHashField(digest, []byte(record.Kind))
	writeHashField(digest, record.Payload)
	writeHashField(digest, []byte(fmt.Sprintf("%d", record.EventCount)))
	writeHashField(digest, []byte(record.PreviousHash))
	writeHashField(digest, []byte(record.Chain))
	writeHashField(digest, []byte(record.ControlSchema))
	writeHashField(digest, []byte(record.Durability))
	writeHashField(digest, []byte(record.EncodingError))
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func newChainDigest(chain string, key []byte) (hash.Hash, error) {
	switch chain {
	case chainSHA256:
		return sha256.New(), nil
	case chainHMAC:
		if len(key) == 0 {
			return nil, ErrAuthenticationRequired
		}
		return hmac.New(sha256.New, key), nil
	default:
		return nil, fmt.Errorf("teleop/audit: unsupported chain %q", chain)
	}
}

// recordHashV3 covers every field except Hash and Signature. Signature is
// excluded because it is computed over the resulting hash; it is bound to the
// record through that hash rather than through the chain.
func recordHashV3(record diskRecord, chain string, key []byte) (string, error) {
	digest, err := newChainDigest(chain, key)
	if err != nil {
		return "", err
	}
	writeHashField(digest, []byte("teleop-audit-v3"))
	writeHashField(digest, []byte(strconv.Itoa(record.Version)))
	writeHashField(digest, []byte(record.RecordType))
	writeHashField(digest, []byte(record.RecordedAt.UTC().Format(time.RFC3339Nano)))
	writeHashField(digest, []byte(record.Kind))
	writeHashField(digest, record.Payload)
	writeHashField(digest, []byte(strconv.FormatUint(record.EventCount, 10)))
	writeHashField(digest, []byte(record.PreviousHash))
	writeHashField(digest, []byte(record.Chain))
	writeHashField(digest, []byte(record.ControlSchema))
	writeHashField(digest, []byte(record.Durability))
	writeHashField(digest, []byte(record.EncodingError))

	session := ""
	if record.Session != nil {
		session = record.Session.String()
	}
	writeHashField(digest, []byte(session))
	writeHashField(digest, []byte(strconv.FormatUint(record.TreeSize, 10)))
	writeHashField(digest, []byte(record.TreeRoot))
	writeHashField(digest, []byte(record.KeyID))
	writeHashField(digest, []byte(record.PublicKey))
	writeHashField(digest, []byte(record.Reason))

	var provenance []byte
	if record.provenanceRaw != nil {
		provenance = record.provenanceRaw
	} else if record.Provenance != nil {
		// encoding/json sorts map keys, so this encoding is deterministic.
		encoded, marshalErr := json.Marshal(record.Provenance)
		if marshalErr != nil {
			return "", fmt.Errorf("marshal provenance for hashing: %w", marshalErr)
		}
		provenance = encoded
	}
	writeHashField(digest, provenance)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeHashField(digest hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}

type v1Record struct {
	Version      int              `json:"version"`
	RecordType   string           `json:"record_type"`
	RecordedAt   time.Time        `json:"recorded_at"`
	Kind         teleop.EventKind `json:"kind,omitempty"`
	Header       teleop.Header    `json:"header,omitempty"`
	Payload      json.RawMessage  `json:"payload,omitempty"`
	EventCount   uint64           `json:"event_count,omitempty"`
	PreviousHash string           `json:"previous_hash,omitempty"`
	Hash         string           `json:"hash,omitempty"`
}

func recordHashV1(record diskRecord) (string, error) {
	legacy := v1Record{
		Version:      record.Version,
		RecordType:   record.RecordType,
		RecordedAt:   record.RecordedAt,
		Kind:         record.Kind,
		Payload:      record.Payload,
		EventCount:   record.EventCount,
		PreviousHash: record.PreviousHash,
	}
	if record.Header != nil {
		legacy.Header = *record.Header
	}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		return "", fmt.Errorf("marshal v1 audit hash input: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
