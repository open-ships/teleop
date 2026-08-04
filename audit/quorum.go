package audit

import (
	"context"
	"errors"
	"fmt"
)

// QuorumAnchor publishes each checkpoint to independent Anchor adapters and
// acknowledges it after a configured number succeed. Put adapters in separate
// administrative and failure domains; several paths to one account or disk do
// not provide independent custody.
type QuorumAnchor struct {
	quorum  int
	anchors []Anchor
	// slots are per-adapter single-flight tokens. An adapter that violates the
	// context contract can retain at most one worker; later publications treat
	// that adapter as unavailable while healthy quorum members keep advancing.
	slots []chan struct{}
}

// NewQuorumAnchor returns an Anchor that requires quorum successful
// publications. Quorum must be positive and no larger than len(anchors).
func NewQuorumAnchor(quorum int, anchors ...Anchor) (*QuorumAnchor, error) {
	if quorum <= 0 || quorum > len(anchors) {
		return nil, fmt.Errorf(
			"teleop/audit: witness quorum %d is outside [1,%d]",
			quorum,
			len(anchors),
		)
	}
	cloned := make([]Anchor, len(anchors))
	copy(cloned, anchors)
	for index, anchor := range cloned {
		if anchor == nil {
			return nil, fmt.Errorf("teleop/audit: witness anchor %d is nil", index)
		}
	}
	slots := make([]chan struct{}, len(cloned))
	for index := range slots {
		slots[index] = make(chan struct{}, 1)
		slots[index] <- struct{}{}
	}
	return &QuorumAnchor{quorum: quorum, anchors: cloned, slots: slots}, nil
}

// Publish implements Anchor. Adapters run concurrently so a slow witness does
// not serialize otherwise independent failure domains.
func (a *QuorumAnchor) Publish(ctx context.Context, checkpoint Checkpoint) error {
	if a == nil {
		return fmt.Errorf("teleop/audit: nil quorum anchor")
	}
	if ctx == nil {
		return fmt.Errorf("teleop/audit: nil quorum anchor context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		index int
		err   error
	}
	results := make(chan outcome, len(a.anchors))
	for index, anchor := range a.anchors {
		select {
		case <-a.slots[index]:
		default:
			results <- outcome{
				index: index,
				err:   errors.New("previous publication is still running"),
			}
			continue
		}
		go func(index int, anchor Anchor) {
			err := publishAnchor(anchor, childCtx, checkpoint)
			// Release the adapter before reporting its outcome. A caller may begin
			// the next checkpoint as soon as this result satisfies quorum.
			a.slots[index] <- struct{}{}
			results <- outcome{index: index, err: err}
		}(index, anchor)
	}

	succeeded := 0
	remaining := len(a.anchors)
	var failures error
	for remaining > 0 {
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), failures)
		case result := <-results:
			remaining--
			if result.err == nil {
				succeeded++
				if succeeded >= a.quorum {
					return nil
				}
				continue
			}
			failures = errors.Join(
				failures,
				fmt.Errorf("witness anchor %d: %w", result.index, result.err),
			)
			if succeeded+remaining < a.quorum {
				return fmt.Errorf(
					"teleop/audit: witness quorum unavailable (%d/%d required): %w",
					a.quorum,
					len(a.anchors),
					failures,
				)
			}
		}
	}
	return fmt.Errorf(
		"teleop/audit: witness quorum unavailable (%d/%d required): %w",
		a.quorum,
		len(a.anchors),
		failures,
	)
}

var _ Anchor = (*QuorumAnchor)(nil)
