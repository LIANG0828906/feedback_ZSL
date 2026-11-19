package dnswire

import (
	"encoding/binary"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	msg := Message{
		ID:                 7,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   true,
		RecursionAvailable: true,
		RCode:              RCodeNoError,
		Questions:          []Question{{Name: "www.example.test.", Type: TypeA, Class: ClassIN}},
		Answers: []RR{
			{Name: "www.example.test.", Type: TypeCNAME, TTL: 60, Target: "edge.example.test."},
			{Name: "edge.example.test.", Type: TypeA, TTL: 30, A: "192.0.2.10"},
		},
		Authorities: []RR{{
			Name: "example.test.", Type: TypeSOA, TTL: 15,
			SOA: &SOA{MName: "ns.example.test.", RName: "hostmaster.example.test.", Minimum: 15},
		}},
		UDPPayloadSize: 1232,
		Options:        []Option{{Code: 65001, Data: []byte{1, 2, 3}}},
	}
	wire, err := Pack(msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || !got.Authoritative || len(got.Answers) != 2 {
		t.Fatalf("unexpected message: %+v", got)
	}
	if got.Answers[0].Target != "edge.example.test." || got.Answers[1].A != "192.0.2.10" {
		t.Fatalf("unexpected answers: %+v", got.Answers)
	}
	if data, ok := got.Option(65001); !ok || len(data) != 3 {
		t.Fatalf("EDNS option was not preserved")
	}
}

func TestCompressedName(t *testing.T) {
	msg := Message{
		ID:        9,
		Response:  true,
		Questions: []Question{{Name: "a.example.test.", Type: TypeA, Class: ClassIN}},
		Answers:   []RR{{Name: "a.example.test.", Type: TypeA, TTL: 10, A: "127.0.0.1"}},
	}
	wire, err := Pack(msg)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the second owner name with a pointer to the question name.
	qEnd := 12 + 1 + len("a") + 1 + len("example") + 1 + len("test") + 1 + 4
	ownerNameLen := 1 + len("a") + 1 + len("example") + 1 + len("test") + 1
	tail := wire[qEnd+ownerNameLen:]
	compressed := append([]byte(nil), wire[:qEnd]...)
	compressed = append(compressed, 0xc0, 0x0c)
	compressed = append(compressed, tail...)
	binary.BigEndian.PutUint16(compressed[6:8], 1)
	got, err := Parse(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Answers[0].Name != "a.example.test." {
		t.Fatalf("pointer name = %q", got.Answers[0].Name)
	}
}

func TestCanonicalName(t *testing.T) {
	got, err := CanonicalName("WWW.Example.Test")
	if err != nil {
		t.Fatal(err)
	}
	if got != "www.example.test." {
		t.Fatalf("got %q", got)
	}
}
