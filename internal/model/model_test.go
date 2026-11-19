package model

import (
	"reflect"
	"testing"
)

func TestMergeRanges(t *testing.T) {
	var ranges []Range
	for _, seq := range []int64{5, 1, 2, 4, 9} {
		ranges = MergeRange(ranges, seq)
	}
	want := []Range{{Start: 1, End: 2}, {Start: 4, End: 5}, {Start: 9, End: 9}}
	if !reflect.DeepEqual(ranges, want) {
		t.Fatalf("ranges = %#v, want %#v", ranges, want)
	}
	if AckThrough(ranges) != -1 {
		t.Fatalf("ack_through = %d, want -1", AckThrough(ranges))
	}
	ranges = MergeRange(ranges, 0)
	if AckThrough(ranges) != 2 {
		t.Fatalf("ack_through = %d, want 2", AckThrough(ranges))
	}
	if !Contains(ranges, 4) || Contains(ranges, 8) {
		t.Fatalf("unexpected containment: %#v", ranges)
	}
}

func TestMergeBackendRanges(t *testing.T) {
	got := MergeRanges(
		[]Range{{Start: 0, End: 1}},
		[]Range{{Start: 3, End: 5}, {Start: 2, End: 2}, {Start: 7, End: 8}},
	)
	want := []Range{{Start: 0, End: 5}, {Start: 7, End: 8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
}
