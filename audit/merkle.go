package audit

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/bits"
)

// This file implements the Merkle tree defined by RFC 6962 (Certificate
// Transparency).
//
// A linear hash chain proves only that a log was not edited in place: to
// establish that any single record is authentic, a verifier must be handed the
// entire log. A Merkle tree additionally supports
//
//   - inclusion proofs, which show that one record belongs to a signed tree
//     head in O(log n) hashes, without disclosing the other records, and
//   - consistency proofs, which show that a later tree head is an append-only
//     extension of an earlier one, so a log cannot be silently rewritten
//     between disclosures.
//
// Selective disclosure matters in litigation: a session log may contain other
// operators' activity or commercially sensitive telemetry that is not
// discoverable, yet the authenticity of the relevant records must still be
// demonstrable.

const (
	leafPrefix byte = 0x00
	nodePrefix byte = 0x01
	// HashSize is the length in bytes of every hash in the tree.
	HashSize = sha256.Size
)

// ErrProof reports a Merkle proof that does not verify.
var ErrProof = errors.New("teleop/audit: merkle proof is invalid")

// EmptyRoot is the RFC 6962 hash of an empty tree.
func EmptyRoot() []byte {
	sum := sha256.Sum256(nil)
	return sum[:]
}

// HashLeaf computes the RFC 6962 leaf hash of a record's bytes. The 0x00
// prefix keeps a leaf from ever colliding with an interior node, which is what
// prevents a second-preimage attack against the tree.
func HashLeaf(record []byte) []byte {
	digest := sha256.New()
	digest.Write([]byte{leafPrefix})
	digest.Write(record)
	return digest.Sum(nil)
}

// hashNode computes the RFC 6962 interior node hash.
func hashNode(left, right []byte) []byte {
	digest := sha256.New()
	digest.Write([]byte{nodePrefix})
	digest.Write(left)
	digest.Write(right)
	return digest.Sum(nil)
}

// splitPoint returns the largest power of two strictly less than n, which is
// where RFC 6962 divides a tree of n leaves.
func splitPoint(n uint64) uint64 {
	if n < 2 {
		return 0
	}
	return uint64(1) << (bits.Len64(n-1) - 1)
}

// Tree incrementally maintains a Merkle tree head. It retains one hash per set
// bit in the leaf count, so memory is O(log n) regardless of log length, which
// lets the recorder keep a live root without buffering the session.
type Tree struct {
	size uint64
	// subtrees holds the roots of complete subtrees in descending size order.
	subtrees [][]byte
}

// Append adds an already-hashed leaf. Use HashLeaf to produce one.
func (t *Tree) Append(leaf []byte) {
	t.size++
	t.subtrees = append(t.subtrees, append([]byte(nil), leaf...))
	// The count of complete subtrees always equals the population count of
	// the leaf count, so merging until that holds rebuilds the canonical
	// decomposition.
	for len(t.subtrees) > bits.OnesCount64(t.size) {
		right := t.subtrees[len(t.subtrees)-1]
		left := t.subtrees[len(t.subtrees)-2]
		t.subtrees = append(t.subtrees[:len(t.subtrees)-2], hashNode(left, right))
	}
}

// Size returns the number of leaves added.
func (t *Tree) Size() uint64 { return t.size }

// Root returns the current tree head.
func (t *Tree) Root() []byte {
	if t.size == 0 {
		return EmptyRoot()
	}
	root := t.subtrees[len(t.subtrees)-1]
	for index := len(t.subtrees) - 2; index >= 0; index-- {
		root = hashNode(t.subtrees[index], root)
	}
	return append([]byte(nil), root...)
}

// Root computes the tree head of a complete slice of leaf hashes.
func Root(leaves [][]byte) []byte {
	switch len(leaves) {
	case 0:
		return EmptyRoot()
	case 1:
		return append([]byte(nil), leaves[0]...)
	}
	split := splitPoint(uint64(len(leaves)))
	return hashNode(Root(leaves[:split]), Root(leaves[split:]))
}

// InclusionProof returns the audit path proving that the leaf at index belongs
// to the tree formed by leaves.
func InclusionProof(index uint64, leaves [][]byte) ([][]byte, error) {
	if index >= uint64(len(leaves)) {
		return nil, fmt.Errorf(
			"%w: leaf %d is outside a tree of %d",
			ErrProof,
			index,
			len(leaves),
		)
	}
	return inclusionPath(index, leaves), nil
}

func inclusionPath(index uint64, leaves [][]byte) [][]byte {
	if len(leaves) <= 1 {
		return nil
	}
	split := splitPoint(uint64(len(leaves)))
	if index < split {
		return append(inclusionPath(index, leaves[:split]), Root(leaves[split:]))
	}
	return append(inclusionPath(index-split, leaves[split:]), Root(leaves[:split]))
}

// ConsistencyProof returns the path proving that the tree of the first size
// leaves is a prefix of the tree formed by leaves.
func ConsistencyProof(first uint64, leaves [][]byte) ([][]byte, error) {
	size := uint64(len(leaves))
	if first == 0 || first > size {
		return nil, fmt.Errorf(
			"%w: cannot prove consistency from %d to %d",
			ErrProof,
			first,
			size,
		)
	}
	return consistencyPath(first, leaves, true), nil
}

func consistencyPath(first uint64, leaves [][]byte, complete bool) [][]byte {
	size := uint64(len(leaves))
	if first == size {
		if complete {
			return nil
		}
		return [][]byte{Root(leaves)}
	}
	split := splitPoint(size)
	if first <= split {
		return append(consistencyPath(first, leaves[:split], complete), Root(leaves[split:]))
	}
	return append(consistencyPath(first-split, leaves[split:], false), Root(leaves[:split]))
}

// VerifyInclusion checks an audit path against a tree head, following the
// algorithm in RFC 6962 section 2.1.1.
func VerifyInclusion(index, size uint64, leaf, root []byte, proof [][]byte) error {
	if index >= size {
		return fmt.Errorf("%w: leaf %d is outside a tree of %d", ErrProof, index, size)
	}
	node := index
	last := size - 1
	result := append([]byte(nil), leaf...)
	for _, sibling := range proof {
		if last == 0 {
			return fmt.Errorf("%w: audit path is too long", ErrProof)
		}
		if node&1 == 1 || node == last {
			result = hashNode(sibling, result)
			for node&1 == 0 && node != 0 {
				node >>= 1
				last >>= 1
			}
		} else {
			result = hashNode(result, sibling)
		}
		node >>= 1
		last >>= 1
	}
	if last != 0 {
		return fmt.Errorf("%w: audit path is too short", ErrProof)
	}
	if !bytes.Equal(result, root) {
		return fmt.Errorf("%w: computed root does not match", ErrProof)
	}
	return nil
}

// VerifyConsistency checks that the tree head at size second extends the tree
// head at size first, following RFC 6962 section 2.1.2.
func VerifyConsistency(first, second uint64, firstRoot, secondRoot []byte, proof [][]byte) error {
	if first > second {
		return fmt.Errorf("%w: tree shrank from %d to %d", ErrProof, first, second)
	}
	if first == 0 {
		return fmt.Errorf("%w: consistency from an empty tree is undefined", ErrProof)
	}
	if first == second {
		if len(proof) != 0 {
			return fmt.Errorf("%w: unnecessary consistency path", ErrProof)
		}
		if !bytes.Equal(firstRoot, secondRoot) {
			return fmt.Errorf("%w: equal sizes with unequal roots", ErrProof)
		}
		return nil
	}

	path := proof
	// A first tree that is an exact power of two contributes its own root,
	// which the prover omits because the verifier already holds it.
	if first&(first-1) == 0 {
		path = append([][]byte{firstRoot}, proof...)
	}
	if len(path) == 0 {
		return fmt.Errorf("%w: consistency path is empty", ErrProof)
	}

	node := first - 1
	last := second - 1
	for node&1 == 1 {
		node >>= 1
		last >>= 1
	}

	firstResult := append([]byte(nil), path[0]...)
	secondResult := append([]byte(nil), path[0]...)
	for _, sibling := range path[1:] {
		if last == 0 {
			return fmt.Errorf("%w: consistency path is too long", ErrProof)
		}
		if node&1 == 1 || node == last {
			firstResult = hashNode(sibling, firstResult)
			secondResult = hashNode(sibling, secondResult)
			for node&1 == 0 && node != 0 {
				node >>= 1
				last >>= 1
			}
		} else {
			secondResult = hashNode(secondResult, sibling)
		}
		node >>= 1
		last >>= 1
	}
	if last != 0 {
		return fmt.Errorf("%w: consistency path is too short", ErrProof)
	}
	if !bytes.Equal(firstResult, firstRoot) {
		return fmt.Errorf("%w: computed first root does not match", ErrProof)
	}
	if !bytes.Equal(secondResult, secondRoot) {
		return fmt.Errorf("%w: computed second root does not match", ErrProof)
	}
	return nil
}
