// teleop-verify checks incident evidence against an independently supplied
// signing key and retained witness checkpoints. It never derives trust from a
// key declared inside the evidence file.
package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
	"github.com/open-ships/teleop/internal/strictjson"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type incidentReport struct {
	Verification           audit.Verification          `json:"verification"`
	EventKinds             map[teleop.EventKind]uint64 `json:"event_kinds"`
	KnownGaps              uint64                      `json:"known_gap_events"`
	ErrorEvents            uint64                      `json:"error_events"`
	LegacyEncodingFailures uint64                      `json:"legacy_encoding_failures"`
	Error                  string                      `json:"error,omitempty"`
}

func run(args []string, output, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("teleop-verify", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	keyPath := flags.String("key", "", "independently obtained Ed25519 public key file (hex)")
	witnessPath := flags.String("witnesses", "", "independently retained checkpoint JSON Lines file")
	localOnly := flags.Bool("local-only", false, "verify signatures without an external custody claim")
	partial := flags.Bool("partial", false, "inspect interrupted evidence; unverified tails still produce a nonzero exit")
	events := flags.Bool("events", false, "emit verified event records as JSON Lines before the report, only after complete verification")
	maxBytes := flags.Int64("max-bytes", 1<<30, "maximum evidence file size")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || *keyPath == "" || *maxBytes <= 0 || *maxBytes == 1<<63-1 {
		return errors.New("usage: teleop-verify -key public.hex [-witnesses retained.jsonl | -local-only] evidence.jsonl")
	}
	if (*witnessPath == "") == !*localOnly {
		return errors.New("supply -witnesses or explicitly select -local-only")
	}
	keyBytes, err := readBoundedFile(*keyPath, 1024)
	if err != nil {
		return err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(keyBytes)))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("public key must contain exactly 32 hex-encoded bytes")
	}
	var witnesses []audit.Checkpoint
	if *witnessPath != "" {
		data, err := readBoundedFile(*witnessPath, 16<<20)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		for {
			var raw json.RawMessage
			if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return fmt.Errorf("decode witness: %w", err)
			}
			if err := strictjson.Validate(raw, 1<<20); err != nil {
				return fmt.Errorf("decode witness: %w", err)
			}
			var checkpoint audit.Checkpoint
			head := json.NewDecoder(strings.NewReader(string(raw)))
			head.DisallowUnknownFields()
			if err := head.Decode(&checkpoint); err != nil {
				return fmt.Errorf("decode witness: %w", err)
			}
			witnesses = append(witnesses, checkpoint)
		}
	}
	file, err := os.Open(flags.Arg(0))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > *maxBytes {
		return errors.New("evidence must be a regular file within -max-bytes")
	}
	// A spool keeps large sessions off the heap and avoids publishing a timeline
	// before its required witness checks succeed. It contains public evidence,
	// but remains owner-only and is removed after this invocation.
	var spool *os.File
	if *events {
		spool, err = os.CreateTemp("", "teleop-verified-events-*.jsonl")
		if err != nil {
			return err
		}
		defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	}
	report := incidentReport{EventKinds: make(map[teleop.EventKind]uint64)}
	var eventEncoder *json.Encoder
	if spool != nil {
		eventEncoder = json.NewEncoder(spool)
	}
	limited := &io.LimitedReader{R: file, N: *maxBytes + 1}
	report.Verification, err = audit.Verify(bufio.NewReader(limited), audit.VerifyOptions{
		PublicKey: ed25519.PublicKey(key), RequireFooter: !*partial,
		RequireWitness: !*localOnly, Witnesses: witnesses,
	}, func(record audit.Record) error {
		report.EventKinds[record.Kind]++
		if record.Kind == teleop.EventGap {
			report.KnownGaps++
		}
		if record.Kind == teleop.EventError {
			report.ErrorEvents++
		}
		if record.EncodingError != "" {
			report.LegacyEncodingFailures++
		}
		if eventEncoder != nil {
			return eventEncoder.Encode(struct {
				Type  string       `json:"type"`
				Event audit.Record `json:"event"`
			}{"event", record})
		}
		return nil
	})
	if limited.N == 0 {
		err = errors.Join(err, errors.New("evidence exceeds -max-bytes"))
	}
	if err != nil {
		report.Error = err.Error()
	}
	if err == nil && spool != nil {
		if _, seekErr := spool.Seek(0, io.SeekStart); seekErr != nil {
			return seekErr
		}
		if _, copyErr := io.Copy(output, spool); copyErr != nil {
			return copyErr
		}
	}
	encodeErr := json.NewEncoder(output).Encode(struct {
		Type   string         `json:"type"`
		Report incidentReport `json:"report"`
	}{"report", report})
	return errors.Join(err, encodeErr)
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("key and witness inputs must be regular files")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%s exceeds size limit", path)
	}
	return data, nil
}
