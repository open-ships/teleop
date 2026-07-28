package audit

import (
	"context"
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

// Checkpoint is a signed tree head suitable for publication outside the log.
// It is 32 bytes of root plus metadata, so anchoring is cheap enough to run
// continuously.
type Checkpoint struct {
	Session    teleop.SessionID `json:"session"`
	Size       uint64           `json:"size"`
	Root       string           `json:"root"`
	ChainHead  string           `json:"chain_head"`
	EventCount uint64           `json:"event_count"`
	RecordedAt time.Time        `json:"recorded_at"`
	KeyID      string           `json:"key_id,omitempty"`
	PublicKey  string           `json:"public_key,omitempty"`
	Signature  string           `json:"signature,omitempty"`
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
	Published uint64
	Failed    uint64
	Dropped   uint64
	Err       error
}
