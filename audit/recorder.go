// Package audit writes, verifies, and replays append-only controller event
// logs. The format is newline-delimited JSON for inspection and streaming.
package audit

import (
	"bufio"
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
)

const (
	// FormatVersion is the current audit wire format.
	FormatVersion = 3
	// ControlSchemaVersion pins persisted ControlID meanings independently of
	// the record container.
	ControlSchemaVersion = "teleop.controls.v1"
	MaxRecordSize        = 16 * 1024 * 1024

	// DefaultCheckpointInterval bounds how long a log can run without emitting
	// a signed, anchorable tree head.
	DefaultCheckpointInterval = 10 * time.Second
	// DefaultCheckpointEvery bounds the same by record count, so a busy log
	// checkpoints on volume rather than only on elapsed time.
	DefaultCheckpointEvery = 10000
)

var (
	ErrClosed                 = errors.New("teleop/audit: recorder closed")
	ErrFailed                 = errors.New("teleop/audit: recorder failed")
	ErrUnverified             = errors.New("teleop/audit: stream is not hash chained")
	ErrUnauthenticated        = errors.New("teleop/audit: stream is not HMAC authenticated")
	ErrAuthenticationRequired = errors.New("teleop/audit: HMAC key required")
	ErrIncomplete             = errors.New("teleop/audit: stream is incomplete")
	ErrRecordTooLarge         = errors.New("teleop/audit: record too large")
	// ErrTreeMismatch reports a checkpoint whose Merkle head does not match
	// the records that precede it.
	ErrTreeMismatch = errors.New("teleop/audit: merkle head does not match records")
)

const (
	chainNone   = "none"
	chainSHA256 = "sha256"
	chainHMAC   = "hmac-sha256"
)

// Options configures the audit writer.
type Options struct {
	HashChain       bool
	FlushEveryEvent bool
	HMACKey         []byte
	Now             func() time.Time

	Signer             crypto.Signer
	Anchor             Anchor
	AnchorTimeout      time.Duration
	AnchorQueue        int
	CheckpointInterval time.Duration
	CheckpointEvery    uint64
	Provenance         *Provenance
}

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
// empty key is rejected on the first Record or Close.
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
// Ed25519 key, giving the log non-repudiation that an HMAC chain cannot.
// Individual events are not signed: the Merkle tree and hash chain already
// bind them to the nearest signed head, so per-event signing would add cost
// without adding evidence.
//
// Prefer a crypto.Signer backed by a TPM, Secure Enclave, or HSM. A key the
// operator can read is a key the operator can be accused of having used.
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
		}
	}
}

// WithAnchorTimeout bounds a single anchor publication.
func WithAnchorTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		if timeout > 0 {
			options.AnchorTimeout = timeout
		}
	}
}

// WithCheckpoints sets how often a signed tree head is emitted, by elapsed
// time and by record count. Either bound may be zero to disable it. Frequent
// checkpoints narrow the window in which a truncation can go unwitnessed.
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
}

// Record is the decoded, implementation-neutral event representation.
type Record struct {
	Version       int
	RecordedAt    time.Time
	Kind          teleop.EventKind
	Header        teleop.Header
	Payload       json.RawMessage
	PreviousHash  string
	Hash          string
	EncodingError string
}

// Verification describes what was actually verified.
type Verification struct {
	Version       int
	Chain         string
	Integrity     bool
	Authenticated bool
	Complete      bool
	EventCount    uint64
	Durability    string

	// Signed reports that every signed record verified against the public key
	// the log itself declares. On its own this shows internal consistency, not
	// identity: a forger who replaces the whole log can also replace the
	// declared key.
	Signed bool
	// Trusted reports that signatures verified against a key the caller
	// supplied out of band. This is the property that carries evidentiary
	// weight.
	Trusted bool
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
	RequireFooter         bool
	AllowUnverified       bool
	RequireAuthentication bool
	HMACKey               []byte

	// PublicKey is the signing key the verifier trusts. When set, the log's
	// declared key must match it and every signature must verify against it.
	PublicKey ed25519.PublicKey
	// RequireSignature rejects a log that carries no signatures.
	RequireSignature bool
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

	key     *signingKey
	keyErr  error
	tree    Tree
	anchors *anchorRunner
	session teleop.SessionID

	lastCheckpoint      time.Time
	sinceCheckpoint     uint64
	checkpointsRecorded uint64
}

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
	if configured.Now == nil {
		configured.Now = time.Now
	}
	recorder := &Recorder{
		writer:  bufio.NewWriter(writer),
		raw:     writer,
		options: configured,
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
	return r.key.public
}

// AnchorStats reports external anchoring health. Non-zero Failed or Dropped
// counts mean recent tree heads were never witnessed outside this process.
func (r *Recorder) AnchorStats() AnchorStats {
	if r.anchors == nil {
		return AnchorStats{}
	}
	return r.anchors.stats()
}

// Checkpoint forces a signed tree head immediately. Callers should invoke it
// at moments worth being able to prove later, such as arming, an emergency
// stop, or a handover between operators.
func (r *Recorder) Checkpoint(reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.failure != nil {
		return errors.Join(ErrFailed, r.failure)
	}
	if err := r.startLocked(); err != nil {
		return r.failLocked(err)
	}
	if err := r.checkpointLocked(reason); err != nil {
		return r.failLocked(err)
	}
	return nil
}

// Record implements teleop.EventSink. An event that JSON cannot represent is
// retained as an encoding-error payload rather than terminating controller
// input.
func (r *Recorder) Record(ctx context.Context, event teleop.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	header := event.Header()
	payload, err := json.Marshal(event)
	var encodingError string
	if err != nil {
		encodingError = err.Error()
		payload, _ = json.Marshal(struct {
			Header teleop.Header `json:"header"`
		}{
			Header: header,
		})
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.failure != nil {
		return errors.Join(ErrFailed, r.failure)
	}
	// Bind the session before the manifest is written so a signed manifest
	// cannot be transplanted onto a different session's records.
	if !r.started {
		r.session = header.ID.Session
	}
	if err := r.startLocked(); err != nil {
		return r.failLocked(err)
	}
	now := r.options.Now().UTC()
	record := diskRecord{
		Version:       FormatVersion,
		RecordType:    "event",
		RecordedAt:    now,
		Kind:          event.Kind(),
		Payload:       payload,
		EncodingError: encodingError,
	}
	if err := r.writeLocked(&record); err != nil {
		return r.failLocked(err)
	}
	r.count++
	r.sinceCheckpoint++
	if r.checkpointDueLocked(now) {
		if err := r.checkpointLocked("interval"); err != nil {
			return r.failLocked(err)
		}
	}
	return nil
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

// checkpointLocked writes a signed tree head covering every record before it.
func (r *Recorder) checkpointLocked(reason string) error {
	checkpoint := diskRecord{
		Version:    FormatVersion,
		RecordType: "checkpoint",
		RecordedAt: r.options.Now().UTC(),
		EventCount: r.count,
		Reason:     reason,
	}
	if err := r.writeLocked(&checkpoint); err != nil {
		return err
	}
	r.checkpointsRecorded++
	r.sinceCheckpoint = 0
	r.lastCheckpoint = checkpoint.RecordedAt
	r.publishAnchorLocked(checkpoint)
	return nil
}

func (r *Recorder) publishAnchorLocked(record diskRecord) {
	if r.anchors == nil {
		return
	}
	checkpoint := Checkpoint{
		Session:    r.session,
		Size:       record.TreeSize,
		Root:       record.TreeRoot,
		ChainHead:  record.Hash,
		EventCount: record.EventCount,
		RecordedAt: record.RecordedAt,
		Signature:  record.Signature,
	}
	if r.key != nil {
		checkpoint.KeyID = r.key.id
		checkpoint.PublicKey = hex.EncodeToString(r.key.public)
	}
	r.anchors.publish(checkpoint)
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		if r.failure != nil {
			return errors.Join(ErrFailed, r.failure)
		}
		return nil
	}
	r.closed = true
	// Stop anchoring on every exit path, including failures, so a failed
	// Close cannot leak the publisher goroutine.
	defer func() {
		if r.anchors != nil {
			r.anchors.stop()
		}
	}()
	if r.failure != nil {
		return errors.Join(ErrFailed, r.failure)
	}
	if err := r.startLocked(); err != nil {
		return r.failLocked(err)
	}
	footer := diskRecord{
		Version:    FormatVersion,
		RecordType: "footer",
		RecordedAt: r.options.Now().UTC(),
		EventCount: r.count,
	}
	if err := r.writeLocked(&footer); err != nil {
		return r.failLocked(err)
	}
	r.publishAnchorLocked(footer)
	if err := r.flushAndSync(); err != nil {
		return r.failLocked(err)
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
	r.started = true
	manifest := diskRecord{
		Version:       FormatVersion,
		RecordType:    "manifest",
		RecordedAt:    r.options.Now().UTC(),
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
	if _, ok := r.raw.(interface{ Sync() error }); ok {
		return "fsync-every-record"
	}
	return "flush-every-record"
}

func (r *Recorder) writeLocked(record *diskRecord) error {
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
	return errors.Join(ErrFailed, r.failure)
}

func (r *Recorder) flushAndSync() error {
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

// ReadAll decodes and verifies a complete, chained stream.
func ReadAll(reader io.Reader) ([]Record, error) {
	records, _, err := Read(reader, VerifyOptions{RequireFooter: true})
	return records, err
}

// ReadAuthenticated verifies a complete HMAC stream with key.
func ReadAuthenticated(reader io.Reader, key []byte) ([]Record, Verification, error) {
	return Read(reader, VerifyOptions{
		RequireFooter:         true,
		RequireAuthentication: true,
		HMACKey:               append([]byte(nil), key...),
	})
}

// ReadPartial verifies all complete records and permits a missing or torn
// footer. Valid prefix records are retained when a later record is invalid.
func ReadPartial(reader io.Reader) ([]Record, error) {
	records, _, err := Read(reader, VerifyOptions{})
	return records, err
}

// Read performs streaming verification and accumulates event records.
func Read(reader io.Reader, options VerifyOptions) ([]Record, Verification, error) {
	records := make([]Record, 0)
	verification, err := Verify(reader, options, func(record Record) error {
		records = append(records, record)
		return nil
	})
	return records, verification, err
}

// Verify checks a stream incrementally and invokes consume for each verified
// event, avoiding whole-log memory growth.
func Verify(
	reader io.Reader,
	options VerifyOptions,
	consume func(Record) error,
) (Verification, error) {
	buffered := bufio.NewReaderSize(reader, 64*1024)
	var (
		verification Verification
		previous     string
		lineNumber   int
		footer       bool
		legacy       bool
		seen         = make(map[teleop.EventID]struct{})
		sequences    = make(map[string]uint64)
		session      *teleop.SessionID
		tree         Tree
		trusted      ed25519.PublicKey
		headSession  teleop.SessionID
	)
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
		var disk diskRecord
		if decodeErr := json.Unmarshal(line, &disk); decodeErr != nil {
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
					verification.Session = headSession
				}
				if disk.PublicKey != "" {
					public, keyErr := decodePublicKey(disk.PublicKey)
					if keyErr != nil {
						return verification, fmt.Errorf("audit line 1: %w", keyErr)
					}
					verification.PublicKey = public
					verification.KeyID = disk.KeyID
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
			if options.RequireSignature && trusted == nil {
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
				verification.Signed = true
				verification.Trusted = len(options.PublicKey) > 0
			}
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
		}

		switch disk.RecordType {
		case "manifest":
			if legacy || lineNumber != 1 {
				return verification, fmt.Errorf("audit line %d: misplaced manifest", lineNumber)
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
		if session == nil {
			value := header.ID.Session
			session = &value
		} else if header.ID.Session != *session {
			return verification, fmt.Errorf("audit line %d: session changed within stream", lineNumber)
		}
		if header.ID.Stream == "" || header.ID.Sequence == 0 {
			return verification, fmt.Errorf("audit line %d: invalid event ID", lineNumber)
		}
		if header.ID.Sequence != sequences[header.ID.Stream]+1 {
			return verification, fmt.Errorf(
				"audit line %d: stream %q sequence %d follows %d",
				lineNumber,
				header.ID.Stream,
				header.ID.Sequence,
				sequences[header.ID.Stream],
			)
		}
		for _, cause := range header.Causes {
			if _, ok := seen[cause]; !ok {
				return verification, fmt.Errorf(
					"audit line %d: cause %v does not precede event",
					lineNumber,
					cause,
				)
			}
		}
		sequences[header.ID.Stream] = header.ID.Sequence
		seen[header.ID] = struct{}{}
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
		if consume != nil {
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
	if options.RequireSignature && !verification.Signed {
		return verification, fmt.Errorf("%w: no signed tree head", ErrSignatureRequired)
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
	if record.Provenance != nil {
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
