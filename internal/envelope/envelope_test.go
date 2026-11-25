package envelope_test

import (
	"testing"

	"prosodyalign/internal/envelope"
	"prosodyalign/internal/errs"
	"prosodyalign/internal/generator"
	"prosodyalign/internal/model"
	"prosodyalign/internal/sched"
)

func TestParallelAndSerialEnvelopeAreIdentical(t *testing.T) {
	suite, err := generator.GenerateSuite(generator.FixedSeed, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range suite.Dialogs {
		serial, _, err := envelope.Extract(d.Project, nil)
		if err != nil {
			t.Fatal(err)
		}
		parallel, _, err := envelope.Extract(d.Project, sched.New(4, 512, 1024))
		if err != nil {
			t.Fatal(err)
		}
		if len(serial) != len(parallel) {
			t.Fatalf("frame count %d vs %d", len(serial), len(parallel))
		}
		for i := range serial {
			if serial[i] != parallel[i] {
				t.Fatalf("frame %d mismatch serial=%+v parallel=%+v", i, serial[i], parallel[i])
			}
			if serial[i].EnvelopeFixed < 0 || serial[i].EnvelopeFixed > d.Project.EnvelopeParams.FixedPointScale {
				t.Fatalf("envelope outside domain at frame %d", i)
			}
		}
	}
}

func TestEnvelopeDomainErrorContainsFrameAndPhoneme(t *testing.T) {
	err := envelope.ValidateFixedPoint([]model.FrameResult{{FrameIndex: 7, PhonemeIndex: 2, EnvelopeFixed: -1}}, 1_000_000)
	ce, ok := errs.As(err)
	if !ok || ce.Detail.Code != errs.EEnv || ce.Detail.FrameIndex == nil || ce.Detail.PhonemeIndex == nil {
		t.Fatalf("expected E_ENV with frame and phoneme, got %+v", err)
	}
}
