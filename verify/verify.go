package verify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"lipsync/domain"
	"lipsync/truth"
	_ "modernc.org/sqlite"
)

type Report struct {
	Databases   []string `json:"databases"`
	Shards      int      `json:"shards"`
	Packages    int      `json:"packages"`
	Frames      int      `json:"frames"`
	Fingerprint string   `json:"truth_fingerprint"`
}

type shardSnapshot struct {
	Index            int
	DialogueID       string
	State            string
	SourceVersion    string
	ContentHash      string
	QuantVersion     string
	AlgorithmVersion string
	TruthFingerprint string
	PackageSummary   string
	Packages         []domain.ScheduledPackage
	CommitSeq        int64
}

type snapshot struct {
	Path   string
	Shards map[int]shardSnapshot
	Frames map[int64][]string
}

type diffCandidate struct {
	Frame      int64
	DialogueID string
	ShardIndex int
	Field      string
	Expected   string
	Actual     string
	Metadata   bool
}

// VerifyDB reads only SQLite artifacts and the sealed truth manifest. It does
// not call the main scheduler, merger, or pipeline.
func VerifyDB(ctx context.Context, dbPath string, m *truth.Manifest) (*Report, error) {
	if err := m.VerifyFingerprint(); err != nil {
		return nil, err
	}
	snap, err := readSnapshot(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	candidates := verifySnapshot(snap, m)
	if len(candidates) > 0 {
		first := candidates[0]
		return nil, diffError(first)
	}
	return &Report{
		Databases: []string{dbPath}, Shards: len(snap.Shards),
		Packages: countPackages(snap), Frames: len(snap.Frames),
		Fingerprint: m.Fingerprint,
	}, nil
}

// CompareDBs compares persisted shard products and commit sequences only.
func CompareDBs(ctx context.Context, paths ...string) (*Report, error) {
	if len(paths) < 2 {
		return nil, &domain.Error{
			Code: domain.CodeParam, Field: "databases",
			Message: "at least two databases are required",
		}
	}
	snaps := make([]snapshot, 0, len(paths))
	for _, path := range paths {
		s, err := readSnapshot(ctx, path)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	for i := 1; i < len(snaps); i++ {
		if d, ok := firstSnapshotDiff(snaps[0], snaps[i]); ok {
			d.Actual = fmt.Sprintf("%s: %s", snaps[i].Path, d.Actual)
			d.Expected = fmt.Sprintf("%s: %s", snaps[0].Path, d.Expected)
			return nil, diffError(d)
		}
	}
	fingerprint := ""
	for _, s := range snaps[0].Shards {
		fingerprint = s.TruthFingerprint
		break
	}
	return &Report{
		Databases: paths, Shards: len(snaps[0].Shards),
		Packages: countPackages(snaps[0]), Frames: len(snaps[0].Frames),
		Fingerprint: fingerprint,
	}, nil
}

func readSnapshot(ctx context.Context, path string) (snapshot, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", path))
	if err != nil {
		return snapshot{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	out := snapshot{Path: path, Shards: make(map[int]shardSnapshot), Frames: make(map[int64][]string)}
	rows, err := db.QueryContext(ctx, `SELECT
		shard_index,dialogue_id,state,source_version,content_hash,quant_version,
		algorithm_version,truth_fingerprint,package_summary,packages_json,commit_seq
		FROM shards ORDER BY shard_index`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var s shardSnapshot
		var packagesJSON []byte
		if err := rows.Scan(&s.Index, &s.DialogueID, &s.State, &s.SourceVersion,
			&s.ContentHash, &s.QuantVersion, &s.AlgorithmVersion, &s.TruthFingerprint,
			&s.PackageSummary, &packagesJSON, &s.CommitSeq); err != nil {
			rows.Close()
			return out, err
		}
		if err := json.Unmarshal(packagesJSON, &s.Packages); err != nil {
			rows.Close()
			return out, err
		}
		out.Shards[s.Index] = s
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	frameRows, err := db.QueryContext(ctx,
		`SELECT frame,package_ids_json FROM frames ORDER BY frame,shard_index`)
	if err != nil {
		return out, err
	}
	for frameRows.Next() {
		var frame int64
		var idsJSON []byte
		if err := frameRows.Scan(&frame, &idsJSON); err != nil {
			frameRows.Close()
			return out, err
		}
		var ids []string
		if err := json.Unmarshal(idsJSON, &ids); err != nil {
			frameRows.Close()
			return out, err
		}
		out.Frames[frame] = append(out.Frames[frame], ids...)
	}
	if err := frameRows.Close(); err != nil {
		return out, err
	}
	for f := range out.Frames {
		sort.Strings(out.Frames[f])
	}
	return out, nil
}

func verifySnapshot(snap snapshot, m *truth.Manifest) []diffCandidate {
	var diffs []diffCandidate
	if len(snap.Shards) != len(m.Dialogues) {
		diffs = append(diffs, diffCandidate{
			Frame: -1, Field: "shards.count", Metadata: true,
			Expected: fmt.Sprintf("%d", len(m.Dialogues)), Actual: fmt.Sprintf("%d", len(snap.Shards)),
		})
	}
	expectedDialogue := make(map[int]truth.Dialogue)
	for _, d := range m.Dialogues {
		expectedDialogue[d.ShardIndex] = d
	}
	expectedPkg := make(map[string]truth.Package)
	for _, p := range m.Packages {
		expectedPkg[p.ID] = p
	}
	for _, idx := range sortedShardIndexes(snap.Shards) {
		s := snap.Shards[idx]
		td, ok := expectedDialogue[idx]
		if !ok {
			diffs = append(diffs, diffCandidate{
				Frame: -1, ShardIndex: idx, DialogueID: s.DialogueID,
				Field: "shard.index", Metadata: true, Actual: fmt.Sprintf("%d", idx),
			})
			continue
		}
		if s.DialogueID != td.ID {
			diffs = append(diffs, diffCandidate{
				Frame: -1, ShardIndex: idx, DialogueID: td.ID, Field: "dialogue_id",
				Metadata: true, Expected: td.ID, Actual: s.DialogueID,
			})
		}
		if s.State != "committed" {
			diffs = append(diffs, diffCandidate{
				Frame: -1, ShardIndex: idx, DialogueID: td.ID, Field: "state",
				Metadata: true, Expected: "committed", Actual: s.State,
			})
		}
		for _, field := range []struct {
			name string
			got  string
		}{
			{"content_hash", s.ContentHash},
			{"quantization_version", s.QuantVersion},
			{"algorithm_version", s.AlgorithmVersion},
		} {
			if field.got == "" {
				diffs = append(diffs, diffCandidate{
					Frame: -1, ShardIndex: idx, DialogueID: td.ID, Field: field.name,
					Metadata: true, Actual: "empty",
				})
			}
		}
		if s.AlgorithmVersion != m.AlgorithmVersion {
			diffs = append(diffs, diffCandidate{
				Frame: -1, ShardIndex: idx, DialogueID: td.ID, Field: "algorithm_version",
				Metadata: true, Expected: m.AlgorithmVersion, Actual: s.AlgorithmVersion,
			})
		}
		if s.TruthFingerprint != m.Fingerprint {
			diffs = append(diffs, diffCandidate{
				Frame: -1, ShardIndex: idx, DialogueID: td.ID, Field: "truth_fingerprint",
				Metadata: true, Expected: m.Fingerprint, Actual: s.TruthFingerprint,
			})
		}
		if s.SourceVersion == "dialogue/v3" && td.ContentHash != "" && s.ContentHash != td.ContentHash {
			diffs = append(diffs, diffCandidate{
				Frame: -1, ShardIndex: idx, DialogueID: td.ID, Field: "content_hash",
				Metadata: true, Expected: td.ContentHash, Actual: s.ContentHash,
			})
		}
		forSummary := append([]domain.ScheduledPackage(nil), s.Packages...)
		sort.Slice(forSummary, func(i, j int) bool { return forSummary[i].ID < forSummary[j].ID })
		summary, err := domain.Hash(forSummary)
		if err != nil || summary != s.PackageSummary {
			diffs = append(diffs, diffCandidate{
				Frame: -1, ShardIndex: idx, DialogueID: td.ID, Field: "package_summary",
				Metadata: true, Expected: summary, Actual: s.PackageSummary,
			})
		}
		for _, p := range s.Packages {
			w, ok := expectedPkg[p.ID]
			if !ok {
				diffs = append(diffs, diffCandidate{
					Frame: p.StartFrame, ShardIndex: idx, DialogueID: td.ID, Field: "package.id",
					Actual: p.ID,
				})
				continue
			}
			checks := []struct {
				field string
				got   string
				want  string
			}{
				{"package.dialogue_id", p.DialogueID, w.DialogueID},
				{"package.start_frame", fmt.Sprint(p.StartFrame), fmt.Sprint(w.StartFrame)},
				{"package.end_frame", fmt.Sprint(p.EndFrame), fmt.Sprint(w.EndFrame)},
				{"package.phoneme_ids", fmt.Sprint(p.PhonemeIDs), fmt.Sprint(w.PhonemeIDs)},
				{"package.status", p.Status, w.Status},
				{"package.dispatch_start_frame", fmt.Sprint(p.DispatchStartFrame), fmt.Sprint(w.DispatchStartFrame)},
				{"package.dispatch_end_frame", fmt.Sprint(p.DispatchEndFrame), fmt.Sprint(w.DispatchEndFrame)},
			}
			for _, c := range checks {
				if c.got != c.want {
					diffs = append(diffs, diffCandidate{
						Frame: p.StartFrame, ShardIndex: idx, DialogueID: td.ID,
						Field: c.field, Expected: c.want, Actual: c.got,
					})
					break
				}
			}
		}
	}
	expectedFrames := make(map[int64][]string)
	for _, fr := range m.Frames {
		ids := append([]string(nil), fr.PackageIDs...)
		sort.Strings(ids)
		expectedFrames[fr.Frame] = ids
	}
	allFrames := make(map[int64]struct{})
	for f := range expectedFrames {
		allFrames[f] = struct{}{}
	}
	for f := range snap.Frames {
		allFrames[f] = struct{}{}
	}
	frameList := make([]int64, 0, len(allFrames))
	for f := range allFrames {
		frameList = append(frameList, f)
	}
	sort.Slice(frameList, func(i, j int) bool { return frameList[i] < frameList[j] })
	for _, f := range frameList {
		got := snap.Frames[f]
		want := expectedFrames[f]
		if fmt.Sprint(got) == fmt.Sprint(want) {
			continue
		}
		dialogue, shard := frameOwner(f, snap, m)
		diffs = append(diffs, diffCandidate{
			Frame: f, DialogueID: dialogue, ShardIndex: shard,
			Field: "frames.package_ids", Expected: fmt.Sprint(want), Actual: fmt.Sprint(got),
		})
	}
	// A frame mismatch is the most actionable "first" difference. Otherwise
	// metadata/package mismatches are ordered by shard and field.
	sort.SliceStable(diffs, func(i, j int) bool {
		a, b := diffs[i], diffs[j]
		if a.Frame >= 0 && b.Frame >= 0 && a.Frame != b.Frame {
			return a.Frame < b.Frame
		}
		if a.Frame >= 0 && b.Frame < 0 {
			return true
		}
		if a.Frame < 0 && b.Frame >= 0 {
			return false
		}
		if a.ShardIndex != b.ShardIndex {
			return a.ShardIndex < b.ShardIndex
		}
		return a.Field < b.Field
	})
	return diffs
}

func frameOwner(frame int64, snap snapshot, m *truth.Manifest) (string, int) {
	for _, d := range m.Dialogues {
		for _, p := range d.Packages {
			if frame >= p.DispatchStartFrame && frame < p.DispatchEndFrame && p.Status == "accepted" {
				return d.ID, d.ShardIndex
			}
		}
	}
	for _, d := range m.Dialogues {
		for _, p := range d.Packages {
			if frame >= p.StartFrame && frame < p.EndFrame {
				return d.ID, d.ShardIndex
			}
		}
	}
	return "", 0
}

func firstSnapshotDiff(a, b snapshot) (diffCandidate, bool) {
	idxA, idxB := sortedShardIndexes(a.Shards), sortedShardIndexes(b.Shards)
	if len(idxA) != len(idxB) {
		return diffCandidate{
			Frame: -1, Field: "shards.count", Metadata: true,
			Expected: fmt.Sprint(len(idxA)), Actual: fmt.Sprint(len(idxB)),
		}, true
	}
	for i := range idxA {
		if idxA[i] != idxB[i] {
			return diffCandidate{
				Frame: -1, Field: "shard.index", Metadata: true,
				Expected: fmt.Sprint(idxA[i]), Actual: fmt.Sprint(idxB[i]),
			}, true
		}
		sa, sb := a.Shards[idxA[i]], b.Shards[idxB[i]]
		if sa.DialogueID != sb.DialogueID {
			return diffCandidate{Frame: -1, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
				Field: "dialogue_id", Metadata: true, Expected: sa.DialogueID, Actual: sb.DialogueID}, true
		}
		if sa.ContentHash != sb.ContentHash {
			return diffCandidate{Frame: -1, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
				Field: "content_hash", Metadata: true, Expected: sa.ContentHash, Actual: sb.ContentHash}, true
		}
		if sa.SourceVersion != sb.SourceVersion {
			return diffCandidate{Frame: -1, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
				Field: "source_version", Metadata: true, Expected: sa.SourceVersion, Actual: sb.SourceVersion}, true
		}
		if sa.QuantVersion != sb.QuantVersion {
			return diffCandidate{Frame: -1, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
				Field: "quantization_version", Metadata: true, Expected: sa.QuantVersion, Actual: sb.QuantVersion}, true
		}
		if sa.AlgorithmVersion != sb.AlgorithmVersion {
			return diffCandidate{Frame: -1, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
				Field: "algorithm_version", Metadata: true, Expected: sa.AlgorithmVersion, Actual: sb.AlgorithmVersion}, true
		}
		if sa.TruthFingerprint != sb.TruthFingerprint {
			return diffCandidate{Frame: -1, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
				Field: "truth_fingerprint", Metadata: true, Expected: sa.TruthFingerprint, Actual: sb.TruthFingerprint}, true
		}
		if sa.PackageSummary != sb.PackageSummary {
			return diffCandidate{Frame: -1, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
				Field: "package_summary", Metadata: true, Expected: sa.PackageSummary, Actual: sb.PackageSummary}, true
		}
		if sa.CommitSeq != sb.CommitSeq {
			return diffCandidate{Frame: -1, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
				Field: "commit_seq", Metadata: true, Expected: fmt.Sprint(sa.CommitSeq), Actual: fmt.Sprint(sb.CommitSeq)}, true
		}
		wantPkgs := make(map[string]domain.ScheduledPackage)
		for _, p := range sa.Packages {
			wantPkgs[p.ID] = p
		}
		for _, p := range sb.Packages {
			w, ok := wantPkgs[p.ID]
			if !ok {
				return diffCandidate{Frame: p.StartFrame, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
					Field: "package.id", Actual: p.ID}, true
			}
			if w.StartFrame != p.StartFrame {
				return diffCandidate{Frame: p.StartFrame, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
					Field: "package.start_frame", Expected: fmt.Sprint(w.StartFrame), Actual: fmt.Sprint(p.StartFrame)}, true
			}
			if w.EndFrame != p.EndFrame {
				return diffCandidate{Frame: p.StartFrame, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
					Field: "package.end_frame", Expected: fmt.Sprint(w.EndFrame), Actual: fmt.Sprint(p.EndFrame)}, true
			}
			if fmt.Sprint(w.PhonemeIDs) != fmt.Sprint(p.PhonemeIDs) {
				return diffCandidate{Frame: p.StartFrame, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
					Field: "package.phoneme_ids", Expected: fmt.Sprint(w.PhonemeIDs), Actual: fmt.Sprint(p.PhonemeIDs)}, true
			}
			if w.Status != p.Status {
				return diffCandidate{Frame: p.StartFrame, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
					Field: "package.status", Expected: w.Status, Actual: p.Status}, true
			}
			if w.DispatchStartFrame != p.DispatchStartFrame || w.DispatchEndFrame != p.DispatchEndFrame {
				return diffCandidate{Frame: w.DispatchStartFrame, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
					Field:    "package.dispatch_range",
					Expected: fmt.Sprintf("%d..%d", w.DispatchStartFrame, w.DispatchEndFrame),
					Actual:   fmt.Sprintf("%d..%d", p.DispatchStartFrame, p.DispatchEndFrame)}, true
			}
		}
		if len(sa.Packages) != len(sb.Packages) {
			return diffCandidate{Frame: -1, DialogueID: sa.DialogueID, ShardIndex: sa.Index,
				Field: "packages.count", Metadata: true,
				Expected: fmt.Sprint(len(sa.Packages)), Actual: fmt.Sprint(len(sb.Packages))}, true
		}
	}
	allFrames := make(map[int64]struct{})
	for f := range a.Frames {
		allFrames[f] = struct{}{}
	}
	for f := range b.Frames {
		allFrames[f] = struct{}{}
	}
	var frames []int64
	for f := range allFrames {
		frames = append(frames, f)
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i] < frames[j] })
	for _, f := range frames {
		if fmt.Sprint(a.Frames[f]) != fmt.Sprint(b.Frames[f]) {
			return diffCandidate{
				Frame: f, Field: "frames.package_ids",
				Expected: fmt.Sprint(a.Frames[f]), Actual: fmt.Sprint(b.Frames[f]),
			}, true
		}
	}
	return diffCandidate{}, false
}

func sortedShardIndexes(m map[int]shardSnapshot) []int {
	out := make([]int, 0, len(m))
	for idx := range m {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

func countPackages(s snapshot) int {
	n := 0
	for _, shard := range s.Shards {
		n += len(shard.Packages)
	}
	return n
}

func diffError(d diffCandidate) error {
	return &domain.Error{
		Code: domain.CodeDiff, Field: d.Field,
		DialogueID: d.DialogueID, ShardIndex: d.ShardIndex, Frame: d.Frame,
		Expected: d.Expected, Actual: d.Actual,
		Message: "first persisted artifact difference",
	}
}
