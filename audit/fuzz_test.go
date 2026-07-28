package audit

import (
	"bytes"
	"testing"
)

// Verify parses input that is, by construction, potentially adversarial: a log
// offered as evidence may have been produced or edited by the party it
// incriminates. It must reject malformed input rather than panic, hang, or
// consume unbounded memory, and it must never report a forged stream as
// verified.
func FuzzVerify(f *testing.F) {
	public, private, err := GenerateKey()
	if err != nil {
		f.Fatal(err)
	}
	valid := &bytes.Buffer{}
	signer := NewRecorder(valid, WithSigner(private))
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if err := signer.Record(f.Context(), testButtonEvent(sequence)); err != nil {
			f.Fatal(err)
		}
	}
	if err := signer.Close(); err != nil {
		f.Fatal(err)
	}
	_, validVerification, err := ReadTrusted(
		bytes.NewReader(valid.Bytes()),
		public,
	)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid.Bytes())

	unsigned := &bytes.Buffer{}
	recorder := NewRecorder(unsigned)
	_ = recorder.Record(f.Context(), testButtonEvent(1))
	_ = recorder.Close()
	f.Add(unsigned.Bytes())

	f.Add([]byte(""))
	f.Add([]byte("\n"))
	f.Add([]byte("{}"))
	f.Add([]byte(`{"version":3,"record_type":"manifest"}`))
	f.Add([]byte(`{"version":3,"record_type":"checkpoint","tree_size":18446744073709551615}`))
	f.Add([]byte(`{"version":999999,"record_type":"event"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Unverified reads must survive arbitrary input.
		_, _, _ = Read(bytes.NewReader(data), VerifyOptions{AllowUnverified: true})

		// A trusted-key read can accept byte-different but semantically
		// equivalent JSON encodings, such as top-level whitespace or uppercase
		// signature hex. It must never authenticate a different committed tree.
		_, verification, err := Read(bytes.NewReader(data), VerifyOptions{
			RequireFooter:    true,
			RequireSignature: true,
			PublicKey:        public,
		})
		if err == nil &&
			(!verification.Trusted ||
				verification.EventCount != validVerification.EventCount ||
				!bytes.Equal(verification.TreeRoot, validVerification.TreeRoot)) {
			t.Fatalf("fuzzed input verified as a different trusted tree: %+v", verification)
		}
	})
}
