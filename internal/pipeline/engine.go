package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"

	"prosodyalign/internal/checkpoint"
	"prosodyalign/internal/dag"
	"prosodyalign/internal/envelope"
	"prosodyalign/internal/errs"
	"prosodyalign/internal/fit"
	"prosodyalign/internal/model"
	"prosodyalign/internal/sched"
)

type Engine struct {
	Name                string
	SnapshotID          string
	Projects            []*model.Project
	Params              map[string]model.FitParameters
	Store               *checkpoint.Store
	Scheduler           *sched.Scheduler
	FullRecompute       bool
	Results             map[string]*model.DialogResult
	CommitKeys          []string
	committed           int
	CrashAfterCommitKey string
}

func CloneProjects(src []*model.Project) ([]*model.Project, error) {
	out := make([]*model.Project, 0, len(src))
	for _, p := range src {
		data, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		var cp model.Project
		if err := json.Unmarshal(data, &cp); err != nil {
			return nil, err
		}
		out = append(out, &cp)
	}
	return out, nil
}

func NewEngine(name string, projects []*model.Project, store *checkpoint.Store, sch *sched.Scheduler, full bool) *Engine {
	params := map[string]model.FitParameters{}
	for _, p := range projects {
		params[p.ID] = p.FitParameters()
	}
	if sch == nil && !full {
		sch = sched.New(2, 8192, 16384)
	}
	return &Engine{
		Name: name, SnapshotID: SnapshotID, Projects: projects, Params: params,
		Store: store, Scheduler: sch, FullRecompute: full,
		Results: map[string]*model.DialogResult{},
	}
}

func (e *Engine) RunSequence(edits []Edit) (*PathRun, error) {
	run := &PathRun{Name: e.Name, CommitSequence: []string{}}
	for epoch, edit := range edits {
		out, err := e.RunEpoch(epoch, edit)
		if out != nil {
			run.Epochs = append(run.Epochs, out)
		}
		if errors.Is(err, ErrSimulatedKill) {
			run.Terminated = true
			run.CommitSequence = append([]string(nil), e.CommitKeys...)
			return run, err
		}
		if err != nil {
			return run, err
		}
	}
	run.CommitSequence = append([]string(nil), e.CommitKeys...)
	return run, nil
}

func (e *Engine) applyEdit(edit Edit) {
	for _, p := range e.Projects {
		params := e.Params[p.ID]
		if edit.TargetDurationSamples != nil {
			params.TargetDurationSamples = *edit.TargetDurationSamples
		}
		if edit.DefaultMaxStretch != nil {
			params.DefaultMaxStretch = *edit.DefaultMaxStretch
		}
		if edit.AccentTolerance != nil {
			params.AccentTolerance = *edit.AccentTolerance
		}
		if edit.PhonemeWeight != nil && (edit.PhonemeWeight.DialogID == "" || edit.PhonemeWeight.DialogID == p.ID) {
			idx := edit.PhonemeWeight.PhonemeIndex
			if idx >= 0 && idx < len(p.Phonemes) {
				p.Phonemes[idx].WeightPer10k = edit.PhonemeWeight.WeightPer10k
				p.Phonemes[idx].Edited = true
			}
		}
		e.Params[p.ID] = params
	}
}

func parameterHash(p *model.Project, params model.FitParameters) (envHash, allHash string) {
	envHash = model.HashCanonical([]any{envelope.AlgorithmVersion, p.EnvelopeParams})
	fitHash := model.HashCanonical([]any{fit.AlgorithmVersion, AlgorithmVersion, params})
	allHash = model.HashCanonical([]any{envHash, fitHash})
	return envHash, allHash
}

func (e *Engine) RunEpoch(epoch int, edit Edit) (*EpochOutput, error) {
	e.applyEdit(edit)
	out := &EpochOutput{Epoch: epoch, Edit: edit, OrderedFragmentIDs: []string{}, Outcomes: []Outcome{}, CommitSequence: []string{}}
	out.Manifest = InvalidationManifest{Epoch: epoch, Edit: edit.Description, Items: []InvalidationItem{}, ReusedItems: []InvalidationItem{}}

	for ordinal, p := range e.Projects {
		p.ContentHash = model.CanonicalContentHash(p)
		params := e.Params[p.ID]
		envHash, paramHash := parameterHash(p, params)
		fragmentID := p.ID
		commitKey := fmt.Sprintf("%d/%s", epoch, fragmentID)
		logicalSeq := int64(epoch*len(e.Projects) + ordinal)
		out.OrderedFragmentIDs = append(out.OrderedFragmentIDs, fragmentID)

		resultCacheKey := fmt.Sprintf("fragment-result:%s:%s:%s", p.ContentHash[:16], paramHash[:16], fragmentID)
		if e.Store != nil {
			committed, err := e.Store.IsCommitted(e.SnapshotID, epoch, fragmentID, p.ContentHash, paramHash, AlgorithmVersion)
			if err != nil {
				return out, err
			}
			if committed {
				if entry, ok, err := e.Store.GetCache(resultCacheKey); err == nil && ok {
					var cached model.DialogResult
					if json.Unmarshal([]byte(entry.PayloadJSON), &cached) == nil {
						e.Results[fragmentID] = &cached
						e.addCommitKey(commitKey)
						out.CommitSequence = append(out.CommitSequence, commitKey)
						out.Outcomes = append(out.Outcomes, Outcome{FragmentID: fragmentID, DialogID: p.ID, Ordinal: ordinal, OK: true, Result: &cached})
						out.Manifest.ReusedItems = append(out.Manifest.ReusedItems, InvalidationItem{DialogID: p.ID, NodeKey: resultCacheKey, NodeKind: "fragment_result", Reason: "durable_commit_match", Scope: "whole_fragment"})
						e.committed++
						if e.CrashAfterCommitKey == commitKey {
							return out, ErrSimulatedKill
						}
						continue
					}
				}
			}
		}

		graph, envKeys := buildDialogGraph(p, p.ContentHash, envHash, paramHash)
		out.GraphNodes = append(out.GraphNodes, graphNodeList(graph)...)
		old := e.Results[fragmentID]

		frames, envFromCache, envReusedKeys, envEntries, err := e.loadOrExtractEnvelope(p, envKeys, envHash, paramHash, edit)
		if err != nil {
			detail := errorDetail(err)
			out.Outcomes = append(out.Outcomes, Outcome{FragmentID: fragmentID, DialogID: p.ID, Ordinal: ordinal, OK: false, ErrorDetail: detail})
			e.appendEnvelopeManifest(&out.Manifest, p, envFromCache, envReusedKeys, epoch == 0 || old == nil)
			continue
		}

		accent, accentErr := envelope.LocateAccent(p, frames, params.AccentTolerance)
		if accent == nil {
			accent = &model.AccentResult{ToleranceSamples: params.AccentTolerance}
		}
		if accentErr != nil && !isAccentToleranceError(accentErr) {
			detail := errorDetail(accentErr)
			out.Outcomes = append(out.Outcomes, Outcome{FragmentID: fragmentID, DialogID: p.ID, Ordinal: ordinal, OK: false, ErrorDetail: detail})
			continue
		}

		fitResult, fitErr := fit.Allocate(p, params, accent)
		if fitResult == nil {
			fitResult = &model.FitResult{}
		}
		digest := model.HashCanonical(frames)
		result := &model.DialogResult{
			DialogID: p.ID, FragmentID: fragmentID, ContentHash: p.ContentHash,
			ParameterHash: paramHash, AlgorithmVersion: AlgorithmVersion,
			Frames: frames, Accent: *accent, Fit: *fitResult, EnvelopeDigest: digest,
		}
		if metrics := schedulerMetrics(e.Scheduler); metrics != nil {
			result.Scheduler = map[string]any{"scheduler": metrics}
		}

		if accentErr != nil || fitErr != nil {
			detail := errorDetail(firstNonNil(accentErr, fitErr))
			e.appendDiffManifest(&out.Manifest, p, old, result, edit, envFromCache, envReusedKeys, nil, nil)
			out.Outcomes = append(out.Outcomes, Outcome{FragmentID: fragmentID, DialogID: p.ID, Ordinal: ordinal, OK: false, Result: result, ErrorDetail: detail})
			continue
		}
		nodeEntries, stretchHits, rangeHits, err := e.persistSegmentNodes(p, result, paramHash, old)
		if err != nil {
			return out, err
		}
		e.appendDiffManifest(&out.Manifest, p, old, result, edit, envFromCache, envReusedKeys, stretchHits, rangeHits)

		resultEntry := checkpoint.CacheEntry{
			Key: resultCacheKey, DialogID: p.ID, PhonemeIndex: -1, Kind: "fragment_result",
			PayloadJSON: checkpoint.MustJSON(result), ContentHash: p.ContentHash,
			ParameterHash: paramHash, AlgorithmVersion: AlgorithmVersion,
		}
		entries := append(append(envEntries, nodeEntries...), resultEntry)
		checkpointRow := checkpoint.FragmentCheckpoint{
			SnapshotID: e.SnapshotID, LogicalSeq: logicalSeq, Epoch: epoch, Ordinal: ordinal,
			FragmentID: fragmentID, DialogID: p.ID, EnvelopeDigest: digest,
			AccentSample: result.Accent.Sample, StretchJSON: checkpoint.MustJSON(result.Fit),
			ContentHash: p.ContentHash, ParameterHash: paramHash, AlgorithmVersion: AlgorithmVersion,
			FrameCount: len(frames),
		}
		if e.Store != nil {
			if err := e.Store.CommitFragmentWithEntries(checkpointRow, entries); err != nil {
				return out, err
			}
		}
		e.Results[fragmentID] = result
		e.addCommitKey(commitKey)
		out.CommitSequence = append(out.CommitSequence, commitKey)
		out.Outcomes = append(out.Outcomes, Outcome{FragmentID: fragmentID, DialogID: p.ID, Ordinal: ordinal, OK: true, Result: result})
		e.committed++
		if e.CrashAfterCommitKey == commitKey {
			return out, ErrSimulatedKill
		}
	}
	return out, nil
}

func (e *Engine) addCommitKey(key string) {
	for _, existing := range e.CommitKeys {
		if existing == key {
			return
		}
	}
	e.CommitKeys = append(e.CommitKeys, key)
}

func (e *Engine) loadOrExtractEnvelope(p *model.Project, envKeys []string, envHash, paramHash string, edit Edit) ([]model.FrameResult, bool, map[int]string, []checkpoint.CacheEntry, error) {
	reusedFrom := map[int]string{}
	if !e.FullRecompute && e.Store != nil {
		var combined []model.FrameResult
		allHit := true
		for i, key := range envKeys {
			entry, ok, err := e.Store.GetCache(key)
			if err != nil {
				return nil, false, nil, nil, err
			}
			if ok {
				var frames []model.FrameResult
				if err := json.Unmarshal([]byte(entry.PayloadJSON), &frames); err != nil {
					allHit = false
					break
				}
				combined = append(combined, frames...)
				continue
			}
			latest, found, err := e.Store.FindLatestCache(p.ID, "phoneme_envelope", i)
			if err != nil {
				return nil, false, nil, nil, err
			}
			weightEditLeavesEnvelopeUnchanged := edit.PhonemeWeight != nil && (edit.PhonemeWeight.DialogID == "" || edit.PhonemeWeight.DialogID == p.ID)
			if found && weightEditLeavesEnvelopeUnchanged {
				var frames []model.FrameResult
				if err := json.Unmarshal([]byte(latest.PayloadJSON), &frames); err == nil {
					combined = append(combined, frames...)
					reusedFrom[i] = latest.Key
					continue
				}
			}
			allHit = false
			break
		}
		if allHit {
			sortFrames(combined)
			return combined, true, reusedFrom, nil, nil
		}
	}

	frames, _, err := envelope.Extract(p, e.Scheduler)
	if err != nil {
		return nil, false, nil, nil, err
	}
	var entries []checkpoint.CacheEntry
	if e.Store != nil && !e.FullRecompute {
		for i, key := range envKeys {
			var sub []model.FrameResult
			for _, f := range frames {
				if f.PhonemeIndex == i {
					sub = append(sub, f)
				}
			}
			entries = append(entries, checkpoint.CacheEntry{
				Key: key, DialogID: p.ID, PhonemeIndex: i, Kind: "phoneme_envelope",
				PayloadJSON: checkpoint.MustJSON(sub), ContentHash: p.ContentHash,
				ParameterHash: envHash, AlgorithmVersion: envelope.AlgorithmVersion,
			})
		}
	}
	return frames, false, reusedFrom, entries, nil
}

func buildDialogGraph(p *model.Project, contentHash, envHash, fitHash string) (*dag.Graph, []string) {
	g := dag.NewGraph()
	contentKey := dag.ContentKey(contentHash)
	g.Add(dag.Node{Key: contentKey, Kind: dag.NodeContent, DialogID: p.ID})
	envParamKey := "parameter:envelope:" + envHash[:12]
	fitParamKey := "parameter:fit:" + fitHash[:12]
	accentParamKey := "parameter:accent:" + fitHash[:12]
	g.Add(dag.Node{Key: envParamKey, Kind: "parameter_version", DialogID: p.ID})
	g.Add(dag.Node{Key: fitParamKey, Kind: "parameter_version", DialogID: p.ID})
	g.Add(dag.Node{Key: accentParamKey, Kind: "parameter_version", DialogID: p.ID})

	envKeys := make([]string, len(p.Phonemes))
	rangeKeys := make([]string, len(p.Phonemes))
	for i, ph := range p.Phonemes {
		idx := i
		envKeys[i] = dag.CacheKey(contentHash, ph.ID, envHash[:16], dag.NodeEnvelope, contentKey, envParamKey)
		g.Add(dag.Node{Key: envKeys[i], Kind: dag.NodeEnvelope, DialogID: p.ID, PhonemeIndex: &idx, DependsOn: []string{contentKey, envParamKey}})

		stretchKey := dag.CacheKey(contentHash, ph.ID, fitHash[:16], dag.NodeStretch, contentKey, fitParamKey)
		g.Add(dag.Node{Key: stretchKey, Kind: dag.NodeStretch, DialogID: p.ID, PhonemeIndex: &idx, DependsOn: []string{contentKey, fitParamKey}})

		deps := []string{stretchKey}
		if i > 0 {
			deps = append(deps, rangeKeys[i-1])
		}
		rangeKeys[i] = dag.CacheKey(contentHash, ph.ID, fitHash[:16], dag.NodeRange, deps...)
		g.Add(dag.Node{Key: rangeKeys[i], Kind: dag.NodeRange, DialogID: p.ID, PhonemeIndex: &idx, DependsOn: deps})
	}
	accentKey := dag.CacheKey(contentHash, p.ID+":accent", fitHash[:16], dag.NodeAccent, append(append([]string{}, envKeys...), accentParamKey)...)
	g.Add(dag.Node{Key: accentKey, Kind: dag.NodeAccent, DialogID: p.ID, DependsOn: append(envKeys, accentParamKey)})
	accentPh := 0
	if len(p.Phonemes) > 3 {
		accentPh = 3
	}
	warpKey := dag.CacheKey(contentHash, p.ID+":warp", fitHash[:16], dag.NodeWarp, accentKey, rangeKeys[accentPh])
	g.Add(dag.Node{Key: warpKey, Kind: dag.NodeWarp, DialogID: p.ID, PhonemeIndex: &accentPh, DependsOn: []string{accentKey, rangeKeys[accentPh]}})
	return g, envKeys
}

func graphNodeList(g *dag.Graph) []dag.Node {
	keys := g.Keys()
	out := make([]dag.Node, 0, len(keys))
	for _, k := range keys {
		if n, ok := g.Node(k); ok {
			out = append(out, n)
		}
	}
	return out
}

func sortFrames(frames []model.FrameResult) {
	for i := 1; i < len(frames); i++ {
		for j := i; j > 0 && frames[j-1].FrameIndex > frames[j].FrameIndex; j-- {
			frames[j-1], frames[j] = frames[j], frames[j-1]
		}
	}
}

func schedulerMetrics(s *sched.Scheduler) any {
	if s == nil {
		return nil
	}
	return s.Metrics()
}

func errorDetail(err error) *errs.Detail {
	if ce, ok := errs.As(err); ok {
		d := ce.Detail
		return &d
	}
	return &errs.Detail{Code: errs.EState, Message: err.Error()}
}

func firstNonNil(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func isAccentToleranceError(err error) bool {
	ce, ok := errs.As(err)
	return ok && ce.Detail.Code == errs.EFit
}
