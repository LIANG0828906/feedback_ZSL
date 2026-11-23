package errs_test

import (
	"testing"

	"glyphbrush/internal/errs"
)

func TestCodeQueries(t *testing.T) {
	if errs.IsCode(nil, errs.CodeStage) {
		t.Fatal("nil error matched a code")
	}
	e := errs.New(errs.CodeDens, "density").WithSegment(4)
	if !errs.IsCode(e, errs.CodeDens) || errs.IsCode(e, errs.CodeSelf) {
		t.Fatal("single error code query failed")
	}
	batch := &errs.BatchError{Errors: []*errs.Error{
		errs.New(errs.CodeStage, "stage"),
		e,
	}}
	if !errs.IsCode(batch, errs.CodeDens) {
		t.Fatal("batch code query missed nested code")
	}
}
