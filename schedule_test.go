package lipsync

import "testing"

func TestSubtitleScheduleCoversEveryFrame(t *testing.T) {
	set := sampleSetForTest(t, 4)
	for _, project := range set.Projects {
		timeline, err := BuildTimeline(project)
		if err != nil {
			t.Fatal(err)
		}
		spans := timeline.Schedule.Spans
		if len(spans) != project.RuneCount() {
			t.Fatalf("%s spans = %d, want %d", project.DialogueID, len(spans), project.RuneCount())
		}
		if spans[0].CharStart != 0 || spans[len(spans)-1].CharEnd != project.RuneCount() {
			t.Fatalf("%s does not cover the subtitle", project.DialogueID)
		}
		if spans[0].StartFrame != 0 || spans[len(spans)-1].EndFrame != timeline.Timing.FrameCount {
			t.Fatalf("%s does not cover the frame range", project.DialogueID)
		}
		for i, span := range spans {
			if span.StartFrame >= span.EndFrame {
				t.Fatalf("%s span %d has no frames", project.DialogueID, i)
			}
			if span.EndFrame-span.StartFrame < project.MinReadableFrames && !timeline.Schedule.Compressed {
				t.Fatalf("%s span %d duration %d below minimum %d", project.DialogueID, i, span.EndFrame-span.StartFrame, project.MinReadableFrames)
			}
			if i > 0 && spans[i-1].EndFrame != span.StartFrame {
				t.Fatalf("%s spans are not contiguous at %d", project.DialogueID, i)
			}
		}
		for f, frame := range timeline.Frames {
			if frame.CharStart < 0 || frame.CharEnd > project.RuneCount() || frame.CharStart >= frame.CharEnd {
				t.Fatalf("%s frame %d char interval = [%d,%d)", project.DialogueID, f, frame.CharStart, frame.CharEnd)
			}
		}
	}
}

func TestSubtitleMinimumCompression(t *testing.T) {
	project := sampleSetForTest(t, 1).Projects[0]
	project.Subtitle.Text = "abc"
	project.Subtitle.Weights = []int{1, 1, 1}
	project.MinReadableFrames = 5
	schedule, err := ScheduleSubtitle(project, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !schedule.Compressed {
		t.Fatal("expected minimum compression")
	}
	if schedule.EffectiveMinimum != 1 {
		t.Fatalf("effective minimum = %d, want 1", schedule.EffectiveMinimum)
	}
	total := 0
	for _, span := range schedule.Spans {
		if span.EndFrame <= span.StartFrame {
			t.Fatal("compressed schedule contains an empty character")
		}
		total += span.EndFrame - span.StartFrame
	}
	if total != 10 || len(schedule.Frames) != 10 {
		t.Fatalf("covered frames = %d/%d, want 10", total, len(schedule.Frames))
	}
}

func TestSubtitleImpossibleReturnsSched(t *testing.T) {
	project := sampleSetForTest(t, 1).Projects[0]
	project.Subtitle.Text = "abc"
	project.Subtitle.Weights = []int{1, 1, 1}
	_, err := ScheduleSubtitle(project, 2)
	if ErrorCode(err) != CodeSched {
		t.Fatalf("code = %s, want E_SCHED: %v", ErrorCode(err), err)
	}
	de, _ := asDomainError(err)
	if de.Frame == nil || *de.Frame != 2 || de.DialogueID != project.DialogueID {
		t.Fatalf("E_SCHED context = %+v", de)
	}
}
