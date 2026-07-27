// Package audit writes and verifies append-only, hash-chained controller event
// logs. The format is newline-delimited JSON for simple inspection and replay.
package audit

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/open-ships/teleop"
)

const FormatVersion = 1

var ErrClosed = errors.New("teleop/audit: recorder closed")

type Options struct {
	HashChain       bool
	FlushEveryEvent bool
}

type Option func(*Options)

func WithHashChain(enabled bool) Option {
	return func(options *Options) { options.HashChain = enabled }
}

func WithFlushEveryEvent(enabled bool) Option {
	return func(options *Options) { options.FlushEveryEvent = enabled }
}

type diskRecord struct {
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

// Record is the decoded, implementation-neutral audit representation.
type Record struct {
	Version      int
	RecordedAt   time.Time
	Kind         teleop.EventKind
	Header       teleop.Header
	Payload      json.RawMessage
	PreviousHash string
	Hash         string
}

type Recorder struct {
	mu       sync.Mutex
	writer   *bufio.Writer
	raw      io.Writer
	options  Options
	previous string
	count    uint64
	closed   bool
}

func NewRecorder(writer io.Writer, options ...Option) *Recorder {
	configured := Options{HashChain: true}
	for _, option := range options {
		if option != nil {
			option(&configured)
		}
	}
	return &Recorder{
		writer:  bufio.NewWriter(writer),
		raw:     writer,
		options: configured,
	}
}

// Record implements teleop.EventSink.
func (r *Recorder) Record(ctx context.Context, event teleop.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", event.Kind(), err)
	}
	record := diskRecord{
		Version:    FormatVersion,
		RecordType: "event",
		RecordedAt: time.Now().UTC(),
		Kind:       event.Kind(),
		Header:     event.Header(),
		Payload:    payload,
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.options.HashChain {
		record.PreviousHash = r.previous
		hash, err := recordHash(record)
		if err != nil {
			return err
		}
		record.Hash = hash
		r.previous = hash
	}
	if err := writeRecord(r.writer, record); err != nil {
		return err
	}
	r.count++
	if r.options.FlushEveryEvent {
		if err := r.flushAndSync(); err != nil {
			return err
		}
	}
	return nil
}

func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	footer := diskRecord{
		Version:      FormatVersion,
		RecordType:   "footer",
		RecordedAt:   time.Now().UTC(),
		EventCount:   r.count,
		PreviousHash: r.previous,
	}
	if r.options.HashChain {
		hash, err := recordHash(footer)
		if err != nil {
			return err
		}
		footer.Hash = hash
	}
	if err := writeRecord(r.writer, footer); err != nil {
		return err
	}
	r.closed = true
	return r.flushAndSync()
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

func writeRecord(writer io.Writer, record diskRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	return nil
}

func recordHash(record diskRecord) (string, error) {
	record.Hash = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("marshal audit hash input: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

// ReadAll decodes and verifies a complete audit stream.
func ReadAll(reader io.Reader) ([]Record, error) {
	return readAll(reader, true)
}

// ReadPartial verifies all available records but permits a missing footer. It
// is intended for inspecting a log from an interrupted or running process.
func ReadPartial(reader io.Reader) ([]Record, error) {
	return readAll(reader, false)
}

func readAll(reader io.Reader, requireFooter bool) ([]Record, error) {
	scanner := bufio.NewScanner(reader)
	// Raw controller reports can be larger than Scanner's small default.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var (
		result   []Record
		previous string
		line     int
		footer   bool
	)
	for scanner.Scan() {
		line++
		var record diskRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("decode audit line %d: %w", line, err)
		}
		if record.Version != FormatVersion {
			return nil, fmt.Errorf("audit line %d: unsupported version %d", line, record.Version)
		}
		if footer {
			return nil, fmt.Errorf("audit line %d: data follows footer", line)
		}
		if record.Hash != "" {
			if record.PreviousHash != previous {
				return nil, fmt.Errorf("audit line %d: hash chain is discontinuous", line)
			}
			expected, err := recordHash(record)
			if err != nil {
				return nil, err
			}
			if record.Hash != expected {
				return nil, fmt.Errorf("audit line %d: hash mismatch", line)
			}
			previous = record.Hash
		}
		if record.RecordType == "footer" {
			if record.EventCount != uint64(len(result)) {
				return nil, fmt.Errorf(
					"audit line %d: footer count %d does not match %d events",
					line,
					record.EventCount,
					len(result),
				)
			}
			footer = true
			continue
		}
		if record.RecordType != "event" {
			return nil, fmt.Errorf("audit line %d: unknown record type %q", line, record.RecordType)
		}
		result = append(result, Record{
			Version:      record.Version,
			RecordedAt:   record.RecordedAt,
			Kind:         record.Kind,
			Header:       record.Header,
			Payload:      append(json.RawMessage(nil), record.Payload...),
			PreviousHash: record.PreviousHash,
			Hash:         record.Hash,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read audit log: %w", err)
	}
	if requireFooter && !footer {
		return nil, errors.New("audit log is incomplete: footer is missing")
	}
	return result, nil
}
