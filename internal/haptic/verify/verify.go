package verify

import (
	"path/filepath"

	"github.com/liang/hapticfeedback/internal/haptic/branches"
	"github.com/liang/hapticfeedback/internal/haptic/checkpoint"
	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
	"github.com/liang/hapticfeedback/internal/haptic/pipeline"
	"github.com/liang/hapticfeedback/internal/haptic/reference"
	"github.com/liang/hapticfeedback/internal/haptic/samplegen"
)

type NameValue struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

type BranchReport struct {
	Branch       int                  `json:"branch"`
	OK           bool                 `json:"ok"`
	Fingerprint  string               `json:"fingerprint"`
	SemanticDiff *branches.Diff       `json:"semantic_diff,omitempty"`
	PacketDiff   *reference.Violation `json:"packet_diff,omitempty"`
}

type ExportReport struct {
	Branch    int            `json:"branch"`
	OK        bool           `json:"ok"`
	LossCount int            `json:"loss_count"`
	Diff      *branches.Diff `json:"diff,omitempty"`
	Error     string         `json:"error,omitempty"`
}

type PathReport struct {
	Name        string               `json:"name"`
	OK          bool                 `json:"ok"`
	Code        string               `json:"code,omitempty"`
	PacketCount int                  `json:"packet_count"`
	Diff        *reference.Violation `json:"diff,omitempty"`
	Error       string               `json:"error,omitempty"`
}

type InjectionReport struct {
	Kind    string `json:"kind"`
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	FrameNo int    `json:"frame_no,omitempty"`
	Channel string `json:"channel,omitempty"`
	Seq     int    `json:"packet_seq,omitempty"`
	Message string `json:"message,omitempty"`
}

type Report struct {
	OK                  bool                 `json:"ok"`
	WorkDir             string               `json:"work_dir"`
	SampleDir           string               `json:"sample_dir"`
	SemanticFingerprint string               `json:"semantic_fingerprint"`
	TruthFingerprint    string               `json:"truth_fingerprint"`
	PacketCount         int                  `json:"packet_count"`
	TotalSamples        int                  `json:"total_samples"`
	CrossBranch         []BranchReport       `json:"cross_branch"`
	Exports             []ExportReport       `json:"exports"`
	Paths               []PathReport         `json:"paths"`
	Injections          []InjectionReport    `json:"injections"`
	IndependentReviewOK bool                 `json:"independent_review_ok"`
	ReviewViolation     *reference.Violation `json:"review_violation,omitempty"`
	LossyProbeCount     int                  `json:"lossy_probe_count"`
	Notes               []string             `json:"notes"`
}

func Run(sampleDir, workDir string) (*Report, *pipeline.SampleSet, error) {
	set, err := pipeline.GenerateAndLoad(sampleDir)
	if err != nil {
		return nil, nil, err
	}
	report := &Report{
		OK: true, SampleDir: sampleDir, WorkDir: workDir,
		SemanticFingerprint: set.Truth.SemanticFingerprint,
		TruthFingerprint:    set.Truth.TruthFingerprint,
	}

	base := set.Streams[1]
	basePlan, err := pipeline.RunOneShot(base, set.Capability, 1)
	if err != nil {
		return nil, set, err
	}
	report.PacketCount = len(basePlan.Packets)
	for _, pkt := range basePlan.Packets {
		report.TotalSamples += pkt.SampleCount
	}

	report.CrossBranch = checkCrossBranches(set, basePlan.Packets)
	report.Exports = checkExports(set)
	lossyProbe, lossCount := checkLossyProbe()
	report.LossyProbeCount = lossCount
	if lossyProbe != nil {
		report.Exports = append(report.Exports, *lossyProbe)
	}
	report.Paths = checkPaths(set, workDir, basePlan.Packets)
	report.Injections = checkInjections(workDir)
	report.IndependentReviewOK, report.ReviewViolation = reviewDatabase(filepath.Join(workDir, "resume.db"), set.Truth.SemanticFingerprint, report.PacketCount)

	for _, check := range report.CrossBranch {
		if !check.OK {
			report.OK = false
		}
	}
	for _, check := range report.Exports {
		if !check.OK {
			report.OK = false
		}
	}
	for _, check := range report.Paths {
		if !check.OK {
			report.OK = false
		}
	}
	for _, check := range report.Injections {
		if !check.OK {
			report.OK = false
		}
	}
	if !report.IndependentReviewOK {
		report.OK = false
	}
	return report, set, nil
}

func reviewDatabase(path, fingerprint string, expectedPackets int) (bool, *reference.Violation) {
	store, err := checkpoint.Open(path)
	if err != nil {
		return false, &reference.Violation{Item: "open_checkpoint", Message: err.Error()}
	}
	defer store.Close()
	packets, err := store.LoadPackets()
	if err != nil {
		return false, &reference.Violation{Item: "load_checkpoint", Message: err.Error()}
	}
	if len(packets) != expectedPackets {
		return false, &reference.Violation{Item: "packet_count", Expected: expectedPackets, Actual: len(packets), Message: "review database does not contain the complete packet sequence"}
	}
	if violation := reference.ReviewRecords(packets, fingerprint); violation != nil {
		return false, violation
	}
	return true, nil
}

func checkCrossBranches(set *pipeline.SampleSet, basePackets []model.Packet) []BranchReport {
	out := []BranchReport{}
	base := set.Streams[1]
	for branch := 1; branch <= 3; branch++ {
		stream := set.Streams[branch]
		item := BranchReport{Branch: branch, Fingerprint: stream.Fingerprint(), OK: true}
		if branch != 1 {
			diff := branches.CompareStreams(base, stream)
			if !diff.Equal {
				item.OK = false
				item.SemanticDiff = &diff
			}
		}
		if item.OK {
			run, err := pipeline.RunOneShot(stream, set.Capability, branch)
			if err != nil {
				item.OK = false
				item.PacketDiff = &reference.Violation{Item: "build", Message: err.Error()}
			} else if diff := reference.FirstPacketDifference(basePackets, run.Packets); diff != nil {
				item.OK = false
				item.PacketDiff = diff
			}
		}
		out = append(out, item)
	}
	return out
}

func checkExports(set *pipeline.SampleSet) []ExportReport {
	out := []ExportReport{}
	base := set.Streams[1]
	for branch := 1; branch <= 3; branch++ {
		item := ExportReport{Branch: branch, OK: true}
		exported, err := branches.Export(*base, branch)
		if err != nil {
			item.OK = false
			item.Error = err.Error()
			out = append(out, item)
			continue
		}
		item.LossCount = len(exported.Losses)
		reimported, err := branches.Load(exported.Data)
		if err != nil {
			item.OK = false
			item.Error = err.Error()
			out = append(out, item)
			continue
		}
		direct := set.Streams[branch]
		diff := branches.CompareStreams(direct, reimported)
		if !diff.Equal {
			item.OK = false
			item.Diff = &diff
		}
		out = append(out, item)
	}
	return out
}

func checkLossyProbe() (*ExportReport, int) {
	canonical := samplegen.CanonicalStream()
	exported, err := branches.Export(canonical, 1)
	if err != nil {
		return &ExportReport{Branch: 1, OK: false, Error: err.Error()}, 0
	}
	if len(exported.Losses) == 0 {
		return &ExportReport{Branch: 1, OK: false, Error: "expected E_LOSSY designer_note item"}, 0
	}
	found := false
	for _, loss := range exported.Losses {
		if loss.InternalName == "designer_note" && loss.Disposition == "omit" {
			found = true
		}
	}
	return &ExportReport{Branch: 0, OK: found, LossCount: len(exported.Losses), Error: map[bool]string{true: "", false: "designer_note loss missing"}[found]}, len(exported.Losses)
}

func checkPaths(set *pipeline.SampleSet, workDir string, expected []model.Packet) []PathReport {
	out := []PathReport{}
	stream := set.Streams[1]
	capability := set.Capability

	full, err := pipeline.RunOneShot(stream, capability, 1)
	out = append(out, makePathReport("full_recompute", full, err, expected))

	realtimeDB := filepath.Join(workDir, "realtime.db")
	realtime, err := pipeline.RunCheckpointed(stream, capability, 1, pipeline.RunOptions{DBPath: realtimeDB})
	out = append(out, makePathReport("realtime_sqlite", realtime, err, expected))

	resumeDB := filepath.Join(workDir, "resume.db")
	partial, err := pipeline.RunCheckpointed(stream, capability, 1, pipeline.RunOptions{DBPath: resumeDB, StopAfter: 2})
	if err != nil {
		out = append(out, PathReport{Name: "checkpoint_partial", OK: false, Error: err.Error()})
	} else if len(partial.Packets) != 2 {
		out = append(out, PathReport{Name: "checkpoint_partial", OK: false, Error: "expected exactly two committed prefix packets"})
	} else {
		out = append(out, PathReport{Name: "checkpoint_partial", OK: true, PacketCount: len(partial.Packets)})
	}
	resumed, err := pipeline.RunCheckpointed(stream, capability, 1, pipeline.RunOptions{DBPath: resumeDB, Resume: true})
	out = append(out, makePathReport("resume_sqlite", resumed, err, expected))
	if resumed != nil {
		out[len(out)-1].PacketCount = len(resumed.Packets)
		if resumed.LoadedCheckpoint != 2 {
			out[len(out)-1].OK = false
			out[len(out)-1].Error = "resume did not load exactly two persisted packets"
		}
	}
	return out
}

func makePathReport(name string, result *pipeline.RunResult, err error, expected []model.Packet) PathReport {
	if err != nil {
		return PathReport{Name: name, OK: false, Error: err.Error()}
	}
	diff := reference.FirstPacketDifference(expected, result.Packets)
	report := PathReport{Name: name, OK: diff == nil, PacketCount: len(result.Packets), Diff: diff}
	if diff != nil {
		report.Code = string(errs.EDiff)
	}
	return report
}

func checkInjections(workDir string) []InjectionReport {
	out := []InjectionReport{}

	corruptBytes, _ := samplegen.InjectedBranchBytes(samplegen.InjectCorrupt, 1)
	_, err := branches.Load(corruptBytes)
	out = append(out, injectionReport(samplegen.InjectCorrupt, errs.EDecode, 3, "ch_left", err))

	orderBytes, _ := samplegen.InjectedBranchBytes(samplegen.InjectOrder, 1)
	orderStream, loadErr := branches.Load(orderBytes)
	if loadErr != nil {
		out = append(out, injectionReport(samplegen.InjectOrder, errs.EOrder, 0, "", loadErr))
	} else {
		_, planErr := pipeline.BuildPlan(orderStream, samplegen.Capability(), 1)
		out = append(out, injectionReport(samplegen.InjectOrder, errs.EOrder, 3, "ch_left", planErr))
	}

	flagBytes, _ := samplegen.InjectedBranchBytes(samplegen.InjectFlag, 2)
	_, flagErr := branches.Load(flagBytes)
	out = append(out, injectionReport(samplegen.InjectFlag, errs.EFlag, 6, "ch_left", flagErr))

	stageDB := filepath.Join(workDir, "injection-stage.db")
	canonical := samplegen.CanonicalStream()
	_, stageErr := pipeline.RunCheckpointed(&canonical, samplegen.Capability(), 1,
		pipeline.RunOptions{DBPath: stageDB, FailPacketSeq: 2})
	report := injectionReport("packet_stage", errs.EStage, 0, "", stageErr)
	if he, ok := errs.As(stageErr); ok {
		report.Seq = he.Seq
		if he.FrameNo != 0 {
			report.FrameNo = he.FrameNo
		}
		if he.Channel != "" {
			report.Channel = he.Channel
		}
	}
	out = append(out, report)
	return out
}

func injectionReport(kind string, expectedCode errs.Code, frame int, channel string, err error) InjectionReport {
	if err == nil {
		return InjectionReport{Kind: kind, OK: false, Code: string(expectedCode), FrameNo: frame, Channel: channel, Message: "expected error but got nil"}
	}
	he, ok := errs.As(err)
	if !ok {
		return InjectionReport{Kind: kind, OK: false, Code: string(errs.CodeOf(err)), Message: err.Error()}
	}
	okCode := he.Code == expectedCode
	okFrame := frame == 0 || he.FrameNo == frame
	okChannel := channel == "" || he.Channel == channel
	return InjectionReport{
		Kind: kind, OK: okCode && okFrame && okChannel, Code: string(he.Code),
		FrameNo: he.FrameNo, Channel: he.Channel, Seq: he.Seq, Message: he.Error(),
	}
}
