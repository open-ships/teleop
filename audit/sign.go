package audit

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/open-ships/teleop"
)

// A hash chain establishes integrity and an HMAC chain establishes
// authenticity, but an HMAC cannot provide third-party attribution: its
// verifier holds the same key that could have produced it. An asymmetric
// signature separates the writing and verification roles. When the public key
// is bound to a device through trusted provisioning and the private key's
// lifecycle is controlled, an Ed25519-capable hardware or remote signer can
// support evidence about that device rather than merely about possession of an
// exportable key.

const (
	signatureDomain = "teleop.audit.sth.v1"
	// SignatureAlgorithmEd25519 identifies Ed25519 signatures in published
	// checkpoints.
	SignatureAlgorithmEd25519 = "ed25519"
)

var (
	// ErrSignature reports a missing, malformed, or invalid signature.
	ErrSignature = errors.New("teleop/audit: signature is invalid")
	// ErrSignatureRequired reports that verification demanded a signed stream.
	ErrSignatureRequired = errors.New("teleop/audit: signature required")
	// ErrUntrustedKey reports a stream signed by a key other than the one the
	// verifier was told to trust.
	ErrUntrustedKey = errors.New("teleop/audit: stream signed by an untrusted key")
)

// GenerateKey creates an Ed25519 signing key. Production deployments should
// prefer an Ed25519-capable hardware or remote crypto.Signer so the private key
// cannot be exported; this exists for tests and for development logs.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate audit signing key: %w", err)
	}
	return public, private, nil
}

// KeyID returns the stable short display identifier recorded alongside a
// signature. It is not a trust anchor; security decisions must compare the
// complete public key obtained through a trusted channel.
func KeyID(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:8])
}

// signingKey binds a crypto.Signer to the Ed25519 public key it represents.
type signingKey struct {
	signer crypto.Signer
	public ed25519.PublicKey
	id     string
}

func newSigningKey(signer crypto.Signer) (key *signingKey, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			key = nil
			err = fmt.Errorf("%w: signer public key callback: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	if signer == nil {
		return nil, fmt.Errorf("%w: nil signer", ErrSignature)
	}
	publicValue := signer.Public()
	public, ok := publicValue.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf(
			"%w: signer public key is %T, want ed25519.PublicKey",
			ErrSignature,
			publicValue,
		)
	}
	if len(public) != ed25519.PublicKeySize {
		return nil, fmt.Errorf(
			"%w: signer public key is %d bytes",
			ErrSignature,
			len(public),
		)
	}
	public = append(ed25519.PublicKey(nil), public...)
	return &signingKey{signer: signer, public: public, id: KeyID(public)}, nil
}

// sign produces a detached Ed25519 signature. Ed25519 hashes internally, so
// crypto.Hash(0) instructs the signer to consume the message directly.
func (k *signingKey) sign(message []byte) (encoded string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			encoded = ""
			err = fmt.Errorf("%w: signer callback: %v", teleop.ErrCallbackPanic, recovered)
		}
	}()
	if k == nil || k.signer == nil {
		return "", fmt.Errorf("%w: signer is unavailable", ErrSignature)
	}
	signature, err := k.signer.Sign(rand.Reader, message, crypto.Hash(0))
	if err != nil {
		return "", fmt.Errorf("sign audit record: %w", err)
	}
	if len(signature) != ed25519.SignatureSize {
		return "", fmt.Errorf(
			"%w: signer returned a %d-byte signature",
			ErrSignature,
			len(signature),
		)
	}
	if !ed25519.Verify(k.public, message, signature) {
		return "", fmt.Errorf("%w: signer returned an invalid Ed25519 signature", ErrSignature)
	}
	return hex.EncodeToString(signature), nil
}

// TreeHead is the statement a recorder signs. Binding the record hash, the
// Merkle root, and the tree size together means a signature cannot be lifted
// from one record and replayed onto another, and that a signed head commits to
// every record beneath it.
type TreeHead struct {
	// Version and RecordType select the signed statement format.
	Version    int
	RecordType string
	// Session binds the statement to one controller session.
	Session teleop.SessionID
	// Size and Root are the Merkle head over preceding records.
	Size uint64
	Root []byte
	// ChainHead is the hash of the head record itself.
	ChainHead string
	// EventCount is the number of events committed by the statement.
	EventCount uint64
	// RecordedAt is the head record's timestamp.
	RecordedAt time.Time
}

// signingBytes returns an unambiguous encoding of the head. Every field is
// length prefixed so no combination of values can be reinterpreted as a
// different head.
func (h TreeHead) signingBytes() []byte {
	buffer := &bytes.Buffer{}
	writeSigned(buffer, []byte(signatureDomain))
	writeSigned(buffer, []byte(strconv.Itoa(h.Version)))
	writeSigned(buffer, []byte(h.RecordType))
	writeSigned(buffer, h.Session[:])
	writeSignedUint(buffer, h.Size)
	writeSigned(buffer, h.Root)
	writeSigned(buffer, []byte(h.ChainHead))
	writeSignedUint(buffer, h.EventCount)
	writeSigned(buffer, []byte(h.RecordedAt.UTC().Format(time.RFC3339Nano)))
	return buffer.Bytes()
}

func writeSigned(buffer *bytes.Buffer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	buffer.Write(length[:])
	buffer.Write(value)
}

func writeSignedUint(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	writeSigned(buffer, encoded[:])
}

// VerifyTreeHead checks a hex-encoded signature over head against public.
func VerifyTreeHead(public ed25519.PublicKey, head TreeHead, signature string) error {
	if len(public) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: public key is %d bytes", ErrSignature, len(public))
	}
	raw, err := hex.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSignature, err)
	}
	if len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature is %d bytes", ErrSignature, len(raw))
	}
	if !ed25519.Verify(public, head.signingBytes(), raw) {
		return fmt.Errorf("%w: tree head signature does not verify", ErrSignature)
	}
	return nil
}

func decodePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: decode public key: %v", ErrSignature, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: public key is %d bytes", ErrSignature, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}
