package acceptance

import (
	"os"
	"testing"

	"dialoguefit/envelope"
	"dialoguefit/fit"
	"dialoguefit/projectio"
)

func TestGoldenArtifactEnvelopeAndFit(t *testing.T) {
	projectPath := "../artifacts/demo/project.json"
	truthPath := "../artifacts/demo/truth.json"
	if _, err := os.Stat(projectPath); err != nil {
		t.Skip("golden artifact not present")
	}
	project, truth, err := projectio.LoadBundle(projectPath, truthPath)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	truthByID := map[string]int{}
	for i, td := range truth.Dialogues {
		truthByID[td.DialogueID] = i
	}
	for _, d := range project.Dialogues {
		td := truth.Dialogues[truthByID[d.ID]]
		frames, err := envelope.Extract(d, project.Envelope)
		if err != nil {
			t.Fatalf("%s envelope: %v", d.ID, err)
		}
		if len(frames) != len(td.FrameValues) {
			t.Fatalf("%s frame count %d != %d", d.ID, len(frames), len(td.FrameValues))
		}
		for i, f := range frames {
			if f.Value != td.FrameValues[i] {
				t.Fatalf("%s frame %d value %.17g != truth %.17g", d.ID, i, f.Value, td.FrameValues[i])
			}
		}
		segments, err := fit.Allocate(d)
		if err != nil {
			t.Fatalf("%s fit: %v", d.ID, err)
		}
		for i, got := range segments {
			want := td.Stretched[i]
			if got.StartSample != want.StartSample || got.EndSample != want.EndSample {
				t.Fatalf("%s segment %d range %d-%d != truth %d-%d", d.ID, i,
					got.StartSample, got.EndSample, want.StartSample, want.EndSample)
			}
			if got.StretchFactor != want.StretchFactor {
				t.Fatalf("%s segment %d factor %.17g != truth %.17g", d.ID, i,
					got.StretchFactor, want.StretchFactor)
			}
			if got.HitCap != want.HitCap || got.Basis != want.Basis {
				t.Fatalf("%s segment %d cap/basis %v/%s != %v/%s", d.ID, i,
					got.HitCap, got.Basis, want.HitCap, want.Basis)
			}
		}
	}
}
