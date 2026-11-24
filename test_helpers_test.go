package syncgame

import (
	"encoding/json"
	"sync"
	"testing"
)

var (
	fixtureOnce sync.Once
	fixtureBase *Sample
	fixtureErr  error
)

func testFixture(t *testing.T) *Sample {
	t.Helper()
	fixtureOnce.Do(func() {
		fixtureBase, fixtureErr = Generate(DefaultGeneratorOptions())
	})
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	return fixtureBase
}

func cloneSample(t *testing.T, sample *Sample) *Sample {
	t.Helper()
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := ParseSample(data)
	if err != nil {
		t.Fatal(err)
	}
	return cloned
}

func requireCode(t *testing.T, err error, code string) *GameError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	gameError, ok := err.(*GameError)
	if !ok {
		t.Fatalf("expected *GameError %s, got %T: %v", code, err, err)
	}
	if gameError.Code != code {
		t.Fatalf("expected %s, got %s: %v", code, gameError.Code, err)
	}
	return gameError
}
