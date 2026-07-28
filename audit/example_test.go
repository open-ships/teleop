package audit_test

import (
	"bytes"
	"context"
	"fmt"

	"github.com/open-ships/teleop"
	"github.com/open-ships/teleop/audit"
)

func ExampleReadTrusted() {
	public, private, err := audit.GenerateKey()
	if err != nil {
		panic(err)
	}

	var encoded bytes.Buffer
	recorder := audit.NewRecorder(&encoded, audit.WithSigner(private))
	var session teleop.SessionID
	session[0] = 1
	err = recorder.Record(context.Background(), teleop.ButtonEvent{
		Meta: teleop.Header{
			ID: teleop.EventID{Session: session, Stream: "input", Sequence: 1},
		},
		Button:  teleop.ButtonFaceSouth,
		Pressed: true,
		Phase:   teleop.PhasePressed,
	})
	if err != nil {
		panic(err)
	}
	if err := recorder.Close(); err != nil {
		panic(err)
	}

	records, verification, err := audit.ReadTrusted(
		bytes.NewReader(encoded.Bytes()),
		public,
	)
	fmt.Println(len(records), verification.Trusted, err)
	// Output: 1 true <nil>
}
