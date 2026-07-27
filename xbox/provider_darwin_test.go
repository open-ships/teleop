//go:build darwin && cgo

package xbox

import (
	"testing"

	"github.com/open-ships/teleop"
)

func TestParseDarwinID(t *testing.T) {
	t.Parallel()

	index, err := parseDarwinID("gamecontroller:3")
	if err != nil || index != 3 {
		t.Fatalf("parse = %d, %v", index, err)
	}
	if _, err := parseDarwinID(teleop.DeviceID("bad")); err == nil {
		t.Fatal("invalid ID parsed successfully")
	}
}
