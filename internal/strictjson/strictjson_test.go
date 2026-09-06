package strictjson

import (
	"strings"
	"testing"
)

func TestValidateRefusesAmbiguousAndUnboundedDocuments(t *testing.T) {
	for _, raw := range []string{"", "{} {}", `{"x":0,"x":1}`, `{"x":0,"\u0078":1}`, `{"nested":[{"x":0,"x":1}]}`, "{", "[}", "\"\xff\"", strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34)} {
		if err := Validate([]byte(raw), 1024); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if err := Validate([]byte(`{"x":[]}`), 2); err == nil {
		t.Fatal("size bound ignored")
	}
	for _, raw := range []string{`{"x":{},"y":[1,true,null,"text"]}`, "0", "[]"} {
		if err := Validate([]byte(raw), 1024); err != nil {
			t.Fatalf("valid %q: %v", raw, err)
		}
	}
}

func TestObjectRequiresExactCaseSensitiveNonNullFields(t *testing.T) {
	for _, raw := range []string{`{"X":1}`, `{"x":null}`, `{}`, `{"x":1,"y":2}`, `[]`, `null`} {
		if _, err := Object([]byte(raw), "x"); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := Object([]byte(`{"x":0}`), "x"); err != nil {
		t.Fatal(err)
	}
}
