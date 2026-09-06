package audit

import (
	"crypto/ed25519"
	"fmt"
	"strings"

	"github.com/open-ships/teleop"
)

func prepareWitnessVerification(options VerifyOptions) (map[uint64]Checkpoint, error) {
	if options.RequireWitness && len(options.Witnesses) == 0 {
		return nil, ErrWitnessRequired
	}
	if len(options.Witnesses) > 0 && len(options.PublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: retained checkpoints require an independent public key", ErrUntrustedKey)
	}
	result := make(map[uint64]Checkpoint, len(options.Witnesses))
	for _, witness := range options.Witnesses {
		if err := VerifyCheckpoint(options.PublicKey, witness); err != nil {
			return nil, err
		}
		if previous, duplicate := result[witness.Size]; duplicate {
			if previous.Session != witness.Session || previous.RecordType != witness.RecordType ||
				previous.Root != witness.Root || previous.ChainHead != witness.ChainHead ||
				previous.EventCount != witness.EventCount || !previous.RecordedAt.Equal(witness.RecordedAt) {
				return nil, fmt.Errorf("%w: conflicting checkpoints at size %d", ErrWitnessMismatch, witness.Size)
			}
		}
		result[witness.Size] = witness
	}
	return result, nil
}

func witnessMatchesRecord(witness Checkpoint, record diskRecord, session teleop.SessionID) bool {
	return witness.Version == record.Version && witness.RecordType == record.RecordType &&
		witness.Session == session && witness.Size == record.TreeSize &&
		witness.Root == record.TreeRoot && witness.ChainHead == record.Hash &&
		witness.EventCount == record.EventCount && witness.RecordedAt.Equal(record.RecordedAt) &&
		strings.EqualFold(witness.Signature, record.Signature)
}
