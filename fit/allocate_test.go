package fit_test

import (
	"errors"
	"testing"

	"dialoguefit/fit"
	"dialoguefit/generate"
	"dialoguefit/model"
)

func TestAllocateUnreachableTargetReturnsEFit(t *testing.T) {
	p := generate.GenerateProject(generate.DefaultSeed)
	d := p.Dialogues[0]
	d.Params.TargetSamples = 10_000_000
	_, err := fit.Allocate(d)
	var de *model.Error
	if !errors.As(err, &de) || de.Code != model.CodeFit {
		t.Fatalf("expected E_FIT, got %v", err)
	}
}

func TestAllocateSilenceReserveLargerThanGapReturnsEFit(t *testing.T) {
	p := generate.GenerateProject(generate.DefaultSeed)
	d := p.Dialogues[0]
	d.Params.TargetSamples = d.PCM.SampleCount + 8
	d.Params.MinSilenceSamples = 10_000
	_, err := fit.Allocate(d)
	var de *model.Error
	if !errors.As(err, &de) || de.Code != model.CodeFit || de.Field != "min_silence_samples" {
		t.Fatalf("expected min silence E_FIT, got %+v", de)
	}
}

func TestValidatePositiveCoefficient(t *testing.T) {
	if err := fit.ValidatePositiveCoefficient("dlg-x", 2, 1.1); err != nil {
		t.Fatal(err)
	}
	err := fit.ValidatePositiveCoefficient("dlg-x", 2, 0)
	var de *model.Error
	if !errors.As(err, &de) || de.Code != model.CodeFit {
		t.Fatalf("expected E_FIT for zero coefficient, got %v", err)
	}
}
