//go:build darwin && cgo

package xbox

import (
	"testing"

	"github.com/open-ships/teleop"
)

func TestParseDarwinID(t *testing.T) {
	t.Parallel()

	const identifier = uint64(0x1234abcd)
	id := formatDarwinID(identifier)
	got, err := parseDarwinID(id)
	if err != nil || got != identifier {
		t.Fatalf("parse %q = %#x, %v; want %#x", id, got, err, identifier)
	}
	for _, invalid := range []teleop.DeviceID{
		"",
		"bad",
		"gamecontroller:0",
		"gamecontroller:0000000000000000",
		"gamecontroller:000000000000000g",
		"gamecontroller:00000000000000001",
	} {
		if _, err := parseDarwinID(invalid); err == nil {
			t.Errorf("invalid ID %q parsed successfully", invalid)
		}
	}
}

func TestNewDarwinDescriptorUsesOpaqueIdentifier(t *testing.T) {
	t.Parallel()

	const identifier = uint64(0x1234abcd)
	descriptor, ok := newDarwinDescriptor(
		identifier,
		7,
		"Xbox Wireless Controller",
		"Xbox One",
		0,
	)
	if !ok {
		t.Fatal("Xbox descriptor rejected")
	}
	if descriptor.ID != formatDarwinID(identifier) {
		t.Fatalf("descriptor ID = %q, want %q", descriptor.ID, formatDarwinID(identifier))
	}
	if got := descriptor.Properties["gamecontroller_identifier"]; got != "000000001234abcd" {
		t.Fatalf("identifier property = %q", got)
	}
}

func TestNewDarwinDescriptorAdvertisesNativeHaptics(t *testing.T) {
	t.Parallel()

	descriptor, ok := newDarwinDescriptor(
		1,
		0,
		"Xbox Wireless Controller",
		"Xbox One",
		64,
	)
	if !ok {
		t.Fatal("Xbox descriptor rejected")
	}
	if !descriptor.Capability.Rumble {
		t.Fatal("Game Controller haptics were not exposed as rumble")
	}
}

func TestIsXboxIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		vendorName      string
		productCategory string
		want            bool
	}{
		{
			name:            "generic vendor with Xbox category",
			vendorName:      "Controller",
			productCategory: "Xbox One",
			want:            true,
		},
		{
			name:       "Microsoft vendor",
			vendorName: "Microsoft Corporation",
			want:       true,
		},
		{
			name:       "Xbox vendor",
			vendorName: "Xbox Wireless Controller",
			want:       true,
		},
		{
			name:            "other controller",
			vendorName:      "Sony Interactive Entertainment",
			productCategory: "DualSense",
			want:            false,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isXboxIdentity(test.vendorName, test.productCategory); got != test.want {
				t.Fatalf("isXboxIdentity(%q, %q) = %t, want %t",
					test.vendorName, test.productCategory, got, test.want)
			}
		})
	}
}

func TestDarwinControllerName(t *testing.T) {
	t.Parallel()

	if got := darwinControllerName("Controller", "Xbox One"); got != "Xbox One" {
		t.Fatalf("generic name = %q, want Xbox One", got)
	}
	if got := darwinControllerName("Xbox Elite Wireless Controller", "Xbox One"); got != "Xbox Elite Wireless Controller" {
		t.Fatalf("specific name = %q", got)
	}
}
