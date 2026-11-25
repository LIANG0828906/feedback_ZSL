package lipsync

import "testing"

func TestLipTransitionsAndTruth(t *testing.T) {
	set := sampleSetForTest(t, 3)
	for i, project := range set.Projects {
		timeline, err := BuildTimeline(project)
		if err != nil {
			t.Fatal(err)
		}
		if len(timeline.Transitions) == 0 {
			t.Fatalf("%s has no transition frames", project.DialogueID)
		}
		for _, transition := range timeline.Transitions {
			frame := timeline.Frames[transition.Frame]
			if !frame.Transition || frame.Lip != LipClosed {
				t.Fatalf("%s transition frame %+v does not use a transition viseme", project.DialogueID, frame)
			}
			if len(transition.InsertedFrames) != 1 || transition.InsertedFrames[0] != transition.Frame {
				t.Fatalf("%s transition insertion locations = %v", project.DialogueID, transition.InsertedFrames)
			}
		}
		truth := set.Truths[i]
		comparison := CompareFrames(project.DialogueID, "timeline_vs_truth", timeline.Frames, truth.Frames)
		if !comparison.Consistent {
			t.Fatal(comparison.Mismatch)
		}
		transitionComparison := CompareTransitions(project.DialogueID, "transitions_vs_truth", timeline.Transitions, truth.Transitions)
		if !transitionComparison.Consistent {
			t.Fatal(transitionComparison.Mismatch)
		}
	}
}

func TestLipToleranceExceeded(t *testing.T) {
	project := sampleSetForTest(t, 1).Projects[0]
	project.LipTransitionTolerance = 0
	_, err := BuildTimeline(project)
	if ErrorCode(err) != CodeLip {
		t.Fatalf("code = %s, want E_LIP: %v", ErrorCode(err), err)
	}
	de, _ := asDomainError(err)
	if de.Frame == nil || de.Left == "" || de.Right == "" {
		t.Fatalf("E_LIP context = %+v", de)
	}
}

func TestLipIndependentOfSubtitleBoundaries(t *testing.T) {
	project := sampleSetForTest(t, 1).Projects[0]
	timeline, err := BuildTimeline(project)
	if err != nil {
		t.Fatal(err)
	}
	modified := timeline.Schedule
	modified.Frames = append([]CharSpan(nil), timeline.Schedule.Frames...)
	for i := range modified.Frames {
		modified.Frames[i].CharStart = 0
		modified.Frames[i].CharEnd = 1
	}
	mapped, err := MapLips(project, timeline.Timing, modified)
	if err != nil {
		t.Fatal(err)
	}
	for f := range timeline.Frames {
		if mapped.Frames[f].BaseLip != timeline.Frames[f].BaseLip ||
			mapped.Frames[f].Lip != timeline.Frames[f].Lip ||
			mapped.Frames[f].Transition != timeline.Frames[f].Transition {
			t.Fatalf("subtitle-only change altered lip at frame %d", f)
		}
	}
	if len(mapped.Transitions) != len(timeline.Transitions) {
		t.Fatalf("transition count changed from %d to %d", len(timeline.Transitions), len(mapped.Transitions))
	}
}

func TestHigherFPSKeepsBaseLipOrder(t *testing.T) {
	project := sampleSetForTest(t, 1).Projects[0]
	high := project
	high.FPS = project.FPS * 2
	lowTimeline, err := BuildTimeline(project)
	if err != nil {
		t.Fatal(err)
	}
	highTimeline, err := BuildTimeline(high)
	if err != nil {
		t.Fatal(err)
	}
	collapse := func(frames []FrameResult) []LipCategory {
		out := make([]LipCategory, 0)
		for _, frame := range frames {
			if len(out) == 0 || out[len(out)-1] != frame.BaseLip {
				out = append(out, frame.BaseLip)
			}
		}
		return out
	}
	lowOrder := collapse(lowTimeline.Frames)
	highOrder := collapse(highTimeline.Frames)
	if len(lowOrder) != len(highOrder) {
		t.Fatalf("collapsed order lengths differ: %d vs %d", len(lowOrder), len(highOrder))
	}
	for i := range lowOrder {
		if lowOrder[i] != highOrder[i] {
			t.Fatalf("collapsed lip order differs at %d: %s vs %s", i, lowOrder[i], highOrder[i])
		}
	}
}
