package teleop

import (
	"context"
	"errors"
	"testing"
)

func TestWaitForCommandResultPrefersBufferedOutcomeOverCancellation(t *testing.T) {
	controller := &Controller{done: make(chan struct{})}
	response := make(chan commandResult, 1)
	wantID := EventID{Session: SessionID{1}, Stream: "command", Sequence: 7}
	wantErr := errors.New("exact sink result")
	response <- commandResult{id: wantID, err: wantErr}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	gotID, gotErr := controller.waitForCommandResult(ctx, response)
	if gotID != wantID || !errors.Is(gotErr, wantErr) {
		t.Fatalf("result = (%v, %v), want buffered (%v, %v)", gotID, gotErr, wantID, wantErr)
	}
	if errors.Is(gotErr, ErrCommandPublicationUncertain) {
		t.Fatalf("buffered exact result was marked uncertain: %v", gotErr)
	}
}
