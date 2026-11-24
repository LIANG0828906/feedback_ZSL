package state

import (
	"testing"

	"mediaforge/internal/errs"
)

func TestStateMachineLegalAndIllegalTransitions(t *testing.T) {
	legal := [][2]State{
		{Pending, Decoding}, {Decoding, Encoding}, {Encoding, Committed},
		{Decoding, Failed}, {Encoding, Failed}, {Failed, Pending}, {Failed, Decoding},
		{Decoding, Pending}, {Encoding, Pending},
	}
	for _, pair := range legal {
		if err := Validate("v", pair[0], pair[1]); err != nil {
			t.Fatalf("%s -> %s should be legal: %v", pair[0], pair[1], err)
		}
	}
	if err := Validate("v", Pending, Committed); errs.CodeOf(err) != errs.EState {
		t.Fatalf("pending -> committed code = %s, want E_STATE", errs.CodeOf(err))
	}
	if err := Validate("v", Committed, Decoding); errs.CodeOf(err) != errs.EState {
		t.Fatalf("committed -> decoding code = %s, want E_STATE", errs.CodeOf(err))
	}
}
