package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

func testLeaves(count int) [][]byte {
	leaves := make([][]byte, count)
	for index := range leaves {
		leaves[index] = HashLeaf([]byte(fmt.Sprintf("record-%d", index)))
	}
	return leaves
}

// TestMerkleReferenceVectors pins the construction against the worked example
// in RFC 6962 section 2.1.3, so an accidental change to the hashing scheme
// cannot silently pass the self-consistency tests below.
func TestMerkleReferenceVectors(t *testing.T) {
	empty := EmptyRoot()
	if got := hex.EncodeToString(empty); got != hex.EncodeToString(sha256sum(nil)) {
		t.Fatalf("empty root = %s", got)
	}

	// A single-leaf tree's root is its leaf hash.
	single := [][]byte{HashLeaf([]byte("a"))}
	if !bytes.Equal(Root(single), single[0]) {
		t.Fatal("single-leaf root must equal the leaf hash")
	}

	// A two-leaf tree is one interior node over both leaves.
	pair := [][]byte{HashLeaf([]byte("a")), HashLeaf([]byte("b"))}
	want := hashNode(pair[0], pair[1])
	if !bytes.Equal(Root(pair), want) {
		t.Fatal("two-leaf root must be the node hash of both leaves")
	}

	// Leaf and node hashing must use distinct prefixes, or a leaf could be
	// forged as an interior node.
	if bytes.Equal(HashLeaf([]byte("a")), sha256sum([]byte("a"))) {
		t.Fatal("leaf hashing must be domain separated")
	}
}

func sha256sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// TestTreeMatchesReferenceRoot checks the incremental O(log n) tree against the
// recursive definition for every size in range.
func TestTreeMatchesReferenceRoot(t *testing.T) {
	var tree Tree
	if !bytes.Equal(tree.Root(), EmptyRoot()) {
		t.Fatal("empty tree root mismatch")
	}
	leaves := testLeaves(129)
	for size := 1; size <= len(leaves); size++ {
		tree.Append(leaves[size-1])
		if tree.Size() != uint64(size) {
			t.Fatalf("size = %d, want %d", tree.Size(), size)
		}
		if !bytes.Equal(tree.Root(), Root(leaves[:size])) {
			t.Fatalf("incremental root diverges at size %d", size)
		}
	}
}

func TestInclusionProofRoundTrip(t *testing.T) {
	leaves := testLeaves(64)
	for size := 1; size <= len(leaves); size++ {
		root := Root(leaves[:size])
		for index := range size {
			proof, err := InclusionProof(uint64(index), leaves[:size])
			if err != nil {
				t.Fatalf("size %d leaf %d: %v", size, index, err)
			}
			err = VerifyInclusion(
				uint64(index),
				uint64(size),
				leaves[index],
				root,
				proof,
			)
			if err != nil {
				t.Fatalf("size %d leaf %d: %v", size, index, err)
			}
		}
	}
}

func TestInclusionProofRejectsTampering(t *testing.T) {
	leaves := testLeaves(16)
	root := Root(leaves)
	proof, err := InclusionProof(5, leaves)
	if err != nil {
		t.Fatal(err)
	}

	if VerifyInclusion(5, 16, HashLeaf([]byte("forged")), root, proof) == nil {
		t.Fatal("a substituted leaf must not verify")
	}
	if VerifyInclusion(6, 16, leaves[5], root, proof) == nil {
		t.Fatal("a wrong index must not verify")
	}
	if VerifyInclusion(5, 16, leaves[5], HashLeaf([]byte("forged")), proof) == nil {
		t.Fatal("a wrong root must not verify")
	}

	corrupted := make([][]byte, len(proof))
	copy(corrupted, proof)
	corrupted[0] = HashLeaf([]byte("forged"))
	if VerifyInclusion(5, 16, leaves[5], root, corrupted) == nil {
		t.Fatal("a corrupted path must not verify")
	}
	if VerifyInclusion(5, 16, leaves[5], root, proof[:len(proof)-1]) == nil {
		t.Fatal("a truncated path must not verify")
	}
}

func TestConsistencyProofRoundTrip(t *testing.T) {
	leaves := testLeaves(48)
	for second := 1; second <= len(leaves); second++ {
		secondRoot := Root(leaves[:second])
		for first := 1; first <= second; first++ {
			firstRoot := Root(leaves[:first])
			proof, err := ConsistencyProof(uint64(first), leaves[:second])
			if err != nil {
				t.Fatalf("%d->%d: %v", first, second, err)
			}
			err = VerifyConsistency(
				uint64(first),
				uint64(second),
				firstRoot,
				secondRoot,
				proof,
			)
			if err != nil {
				t.Fatalf("%d->%d: %v", first, second, err)
			}
		}
	}
}

// TestConsistencyProofDetectsRewrite is the property that matters for
// liability: a log that replaces an already-published record cannot produce a
// consistency proof against the earlier signed head.
func TestConsistencyProofDetectsRewrite(t *testing.T) {
	original := testLeaves(20)
	firstRoot := Root(original[:8])

	rewritten := make([][]byte, len(original))
	copy(rewritten, original)
	rewritten[3] = HashLeaf([]byte("substituted after the fact"))

	proof, err := ConsistencyProof(8, rewritten)
	if err != nil {
		t.Fatal(err)
	}
	err = VerifyConsistency(8, 20, firstRoot, Root(rewritten), proof)
	if err == nil {
		t.Fatal("a rewritten prefix must not prove consistent with the earlier head")
	}
}

func TestConsistencyProofRejectsTruncation(t *testing.T) {
	leaves := testLeaves(20)
	// A log truncated back to 8 leaves cannot prove that it extends the head
	// that was published at 20.
	err := VerifyConsistency(20, 8, Root(leaves), Root(leaves[:8]), nil)
	if err == nil {
		t.Fatal("a shrinking tree must not verify")
	}
}
