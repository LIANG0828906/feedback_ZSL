package uuid

import "testing"

func TestNewAndParseRoundTrip(t *testing.T) {
	value := New()
	if value[6]>>4 != 4 || value[8]>>6 != 2 {
		t.Fatalf("unexpected version/variant: %x", value)
	}
	parsed, err := Parse(value.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != value {
		t.Fatal("parse changed UUID")
	}
}

func TestNewSHA1IsStable(t *testing.T) {
	a := NewSHA1(Nil, []byte("terrain-normal"))
	b := NewSHA1(Nil, []byte("terrain-normal"))
	if a != b || a[6]>>4 != 5 || a[8]>>6 != 2 {
		t.Fatalf("unexpected SHA1 UUID: %s", a)
	}
}
