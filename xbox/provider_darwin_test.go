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
