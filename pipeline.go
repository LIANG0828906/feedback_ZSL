package ocrflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Config struct {
	DBPath        string       `json:"db_path"`
	Budget        BudgetConfig `json:"budget"`
	ChunkLineSize int          `json:"chunk_line_size"`
}

func DefaultConfig(dbPath string) Config {
	return Config{DBPath: dbPath, Budget: DefaultBudgetConfig(), ChunkLineSize: 2}
}

type LineOutcome struct {
	ShotID       string           `json:"shot_id"`
	LineNo       int              `json:"line_no"`
	LineID       string           `json:"line_id"`
	Status       string           `json:"status"`
	Error        *Error           `json:"error,omitempty"`
	CacheHit     bool             `json:"cache_hit"`
	DurationMS   int64            `json:"duration_ms"`
	Retries      int              `json:"retries"`
	ChunkID      string           `json:"chunk_id"`
	Keys         LineKeys         `json:"keys"`
	Box          Rect             `json:"box"`
	Quad         Quad             `json:"quad"`
	BoxSummary   string           `json:"box_summary"`
	TextSummary  string           `json:"text_summary"`
	Recognition  *LineRecognition `json:"recognition,omitempty"`
	Checkpointed bool             `json:"checkpointed"`
}

type ShotReport struct {
	ShotID      string         `json:"shot_id"`
	ContentHash string         `json:"content_hash"`
	Width       int            `json:"width"`
	Height      int            `json:"height"`
	Status      string         `json:"status"`
	Error       *Error         `json:"error,omitempty"`
	Lines       []LineOutcome  `json:"lines"`
	Accuracy    AccuracyReport `json:"accuracy"`
	DetectMS    int64          `json:"detect_ms"`
	CacheHit    bool           `json:"detection_cache_hit"`
}

type RunReport struct {
	Mode                  string             `json:"mode"`
	RecipeSourceVersion   string             `json:"recipe_source_version"`
	RecipeVersion         string             `json:"recipe_version"`
	ParamVersion          string             `json:"param_version"`
	AlgorithmVersion      string             `json:"algorithm_version"`
	TemplateVersion       string             `json:"template_version"`
	Shots                 []ShotReport       `json:"shots"`
	LineSequence          []string           `json:"line_sequence"`
	CommittedLineSequence []string           `json:"committed_line_sequence"`
	CommitSequence        []string           `json:"commit_sequence"`
	CacheHits             int                `json:"cache_hits"`
	CacheMisses           int                `json:"cache_misses"`
	RecomputeMS           int64              `json:"recompute_ms"`
	Budget                BudgetSnapshot     `json:"budget"`
	Invalidation          []InvalidationItem `json:"invalidation,omitempty"`
	Recovery              *RecoveryReport    `json:"recovery,omitempty"`
	StartedUnixNS         int64              `json:"started_unix_ns"`
	FinishedUnixNS        int64              `json:"finished_unix_ns"`
}

type InvalidationItem struct {
	ShotID            string   `json:"shot_id"`
	LineNo            int      `json:"line_no"`
	LineID            string   `json:"line_id"`
	Reason            string   `json:"reason"`
	Field             string   `json:"field,omitempty"`
	Stage             string   `json:"stage"`
	InvalidatedStages []string `json:"invalidated_stages"`
	CacheKeys         []string `json:"cache_keys"`
}

type Service struct {
	cfg    Config
	store  *Store
	sched  *Scheduler
	recipe *Recipe
	lib    *TemplateLibrary
	shots  []*Screenshot

	mu            sync.RWMutex
	last          *RunReport
	graph         *DependencyGraph
	invalidations []InvalidationItem
	recovery      *RecoveryReport

	faultMu      sync.Mutex
	decodeFaults map[string]bool
	lineFaults   map[string]string
	retryCounts  map[string]int
}

func NewService(cfg Config, recipe *Recipe, lib *TemplateLibrary, shots []*Screenshot) (*Service, error) {
	if recipe == nil {
		recipe = DefaultRecipe()
	}
	if err := recipe.ValidateRuntimeParameters(); err != nil {
		return nil, err
	}
	if lib == nil {
		lib = DefaultTemplateLibrary()
	}
	if cfg.ChunkLineSize <= 0 {
		cfg.ChunkLineSize = 2
	}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	sched, err := NewScheduler(cfg.Budget)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return &Service{
		cfg: cfg, store: store, sched: sched, recipe: recipe, lib: lib, shots: shots,
		graph: NewDependencyGraph(), decodeFaults: map[string]bool{}, lineFaults: map[string]string{}, retryCounts: map[string]int{},
	}, nil
}

func (s *Service) Store() *Store             { return s.store }
func (s *Service) Scheduler() *Scheduler     { return s.sched }
func (s *Service) Recipe() *Recipe           { s.mu.RLock(); defer s.mu.RUnlock(); return s.recipe }
func (s *Service) Library() *TemplateLibrary { return s.lib }
func (s *Service) Shots() []*Screenshot      { return s.shots }

func (s *Service) Close() error { return s.store.Close() }

func (s *Service) LastReport() *RunReport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

func (s *Service) Graph() *DependencyGraph {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.graph
}

func (s *Service) InvalidationManifest() []InvalidationItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]InvalidationItem(nil), s.invalidations...)
}

func (s *Service) RecoveryReport() *RecoveryReport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.recovery
}

func (s *Service) SetRecoveryReport(r *RecoveryReport) {
	s.mu.Lock()
	s.recovery = r
	s.mu.Unlock()
}

type detectionArtifact struct {
	prep *PreparedImage
	det  *DetectionResult
	err  error
}

type lineWork struct {
	shot      *Screenshot
	det       *DetectionResult
	line      DetectedLine
	keys      LineKeys
	chunkID   string
	index     int
	outcome   LineOutcome
	result    *LineRecognition
	entries   []CacheEntry
	err       error
	duration  int64
	handle    *TaskHandle
	submitted bool
}

func (s *Service) Run(mode string) (*RunReport, error) {
	started := time.Now().UnixNano()
	recipe := s.Recipe()
	if err := recipe.ValidateRuntimeParameters(); err != nil {
		return nil, err
	}
	report := &RunReport{
		Mode: mode, RecipeSourceVersion: recipe.SourceVersion, RecipeVersion: recipe.RecipeVersion,
		ParamVersion: recipe.ParamVersion, AlgorithmVersion: RecipeAlgorithmVersion,
		TemplateVersion: s.lib.Version, StartedUnixNS: started,
		Invalidation: s.InvalidationManifest(), Recovery: s.RecoveryReport(),
	}

	artifacts := make([]*detectionArtifact, len(s.shots))
	detHandles := make([]*TaskHandle, 0, len(s.shots))
	for i, shot := range s.shots {
		idx := i
		pixels := shot.Width * shot.Height * 4
		handle, err := s.sched.Submit(Task{
			ID: fmt.Sprintf("detect:%s", shot.ShotID), Kind: "detect", ShotID: shot.ShotID, PixelBytes: pixels,
			Run: func() error {
				if s.decodeInjected(shot.ShotID) {
					err := decodeError("screenshot.png", nil, "injected truncated screenshot for %s", shot.ShotID)
					artifacts[idx] = &detectionArtifact{err: err}
					return err
				}
				start := time.Now()
				prep, err := PreprocessImage(shot.RGBA, recipe.Preprocess)
				if err != nil {
					artifacts[idx] = &detectionArtifact{err: err}
					return err
				}
				det, err := DetectLines(shot, recipe, prep)
				if err != nil {
					artifacts[idx] = &detectionArtifact{prep: prep, err: err}
					return err
				}
				artifacts[idx] = &detectionArtifact{prep: prep, det: det}
				_ = time.Since(start).Milliseconds()
				return nil
			},
		})
		if err != nil {
			return nil, err
		}
		detHandles = append(detHandles, handle)
	}
	for _, h := range detHandles {
		_ = <-h.Done()
	}

	combinedGraph := NewDependencyGraph()
	allWork := make([]*lineWork, 0)
	for i, shot := range s.shots {
		art := artifacts[i]
		sr := ShotReport{ShotID: shot.ShotID, ContentHash: shot.ContentHash, Width: shot.Width, Height: shot.Height, Lines: []LineOutcome{}}
		if art == nil || art.err != nil {
			var cause error
			if art != nil {
				cause = art.err
			}
			oe, _ := asError(cause)
			if oe == nil {
				oe = &Error{Code: CodeResource, Message: "detection failed", Cause: cause}
			}
			sr.Status = "failed"
			sr.Error = oe
			for _, tl := range shot.Lines {
				errCopy := *oe
				errCopy.ShotID = shot.ShotID
				errCopy.LineNo = tl.LineNo
				sr.Lines = append(sr.Lines, LineOutcome{ShotID: shot.ShotID, LineNo: tl.LineNo, LineID: tl.LineID, Status: "failed", Error: &errCopy, Box: tl.Quad.Rect(), Quad: tl.Quad})
				report.LineSequence = append(report.LineSequence, lineSeqID(shot.ShotID, tl.LineNo))
			}
			report.Shots = append(report.Shots, sr)
			continue
		}
		sr.DetectMS = 0
		det := art.det
		graph, keys := BuildShotGraph(shot, det, recipe, s.lib)
		mergeGraph(combinedGraph, graph)

		chunks := chunkLines(det.Lines, s.cfg.ChunkLineSize)
		chunkIndexByLine := map[int]string{}
		for ci, lines := range chunks {
			chunkID := fmt.Sprintf("chunk:%s:%02d", shot.ShotID, ci+1)
			for _, line := range lines {
				chunkIndexByLine[line.LineNo] = chunkID
			}
		}

		recs := make([]LineRecognition, 0, len(det.Lines))
		for li, line := range det.Lines {
			lk := keys[li]
			chunkID := chunkIndexByLine[line.LineNo]
			w := &lineWork{shot: shot, det: det, line: line, keys: lk, chunkID: chunkID, index: len(allWork)}
			w.outcome = LineOutcome{
				ShotID: shot.ShotID, LineNo: line.LineNo, LineID: line.LineID, Status: "pending",
				ChunkID: chunkID, Keys: lk, Box: line.Box, Quad: line.Quad, BoxSummary: boxSummary(line.Box),
			}
			allWork = append(allWork, w)
			report.LineSequence = append(report.LineSequence, lineSeqID(shot.ShotID, line.LineNo))
		}
		_ = recs
		report.Shots = append(report.Shots, sr)
	}

	// Look up persistent recognition entries first; submit only misses.
	for _, w := range allWork {
		entry, err := s.store.GetCache(w.keys.RecognitionKey)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			var rec LineRecognition
			if err := json.Unmarshal(entry.Payload, &rec); err == nil {
				w.result = &rec
				w.outcome.Status = "success"
				w.outcome.CacheHit = true
				w.outcome.Recognition = &rec
				report.CacheHits++
				continue
			}
		}
		report.CacheMisses++
		ww := w
		handle, err := s.sched.Submit(Task{
			ID: fmt.Sprintf("recognize:%s:%d", w.shot.ShotID, w.line.LineNo), Kind: "recognize",
			ShotID: w.shot.ShotID, LineNo: w.line.LineNo, PixelBytes: w.shot.Width * w.shot.Height * 4,
			Run: func() error {
				start := time.Now()
				if fault := s.lineFault(w.shot.ShotID, w.line.LineNo); fault != "" {
					e := &Error{Code: CodeResource, ShotID: w.shot.ShotID, LineNo: w.line.LineNo, Field: "injection.line_failure", Message: fault}
					ww.err = e
					return e
				}
				rec, err := RecognizeLine(ww.line, ww.det, ww.det.Mask, s.lib, recipe.Recognition)
				if err != nil {
					oe, _ := asError(err)
					ww.err = oe
					return err
				}
				ww.result = rec
				ww.duration = time.Since(start).Milliseconds()
				payload, _ := json.Marshal(rec)
				candidatePayload, _ := json.Marshal(ww.line)
				segmentPayload, _ := json.Marshal(rec.Chars)
				ww.entries = []CacheEntry{
					s.cacheEntry(ww, "line_candidate", ww.keys.CandidateKey, ww.keys.DetectionKey, ww.keys.DetectionParam, candidatePayload),
					s.cacheEntry(ww, "segmentation", ww.keys.SegmentationKey, ww.keys.CandidateKey, ww.keys.SegmentParam, segmentPayload),
					s.cacheEntry(ww, "recognition", ww.keys.RecognitionKey, ww.keys.SegmentationKey, ww.keys.RecognitionParam, payload),
				}
				return nil
			},
		})
		if err != nil {
			return nil, err
		}
		w.handle = handle
		w.submitted = true
	}

	// Commit chunks in deterministic shot/line order. Failed lines do not
	// create a checkpoint transaction.
	chunkOrder := stableChunkOrder(allWork)
	lastCommitted := ""
	for _, chunkID := range chunkOrder {
		works := worksForChunk(allWork, chunkID)
		chunkFailed := false
		var lines []CheckpointLine
		var cacheEntries []CacheEntry
		var chunkShot *Screenshot
		var detLines []DetectedLine
		for ordinal, w := range works {
			chunkShot = w.shot
			if w.submitted && w.handle != nil {
				err := <-w.handle.Done()
				if err != nil && w.err == nil {
					w.err = err
				}
			}
			if w.err != nil {
				oe, ok := asError(w.err)
				if !ok {
					oe = &Error{Code: CodeResource, Message: w.err.Error()}
				}
				oe.ShotID = w.shot.ShotID
				oe.LineNo = w.line.LineNo
				w.outcome.Status = "failed"
				w.outcome.Error = oe
				w.outcome.DurationMS = w.duration
				w.outcome.Retries = s.retryCount(w.shot.ShotID, w.line.LineNo)
				chunkFailed = true
				report.RecomputeMS += w.duration
				continue
			}
			if w.result == nil {
				w.err = fmt.Errorf("missing recognition result")
				w.outcome.Status = "failed"
				w.outcome.Error = &Error{Code: CodeResource, Message: w.err.Error()}
				chunkFailed = true
				continue
			}
			if !w.outcome.CacheHit {
				w.outcome.CacheHit = false
				report.RecomputeMS += w.duration
			}
			w.outcome.Status = "success"
			w.outcome.Recognition = w.result
			w.outcome.DurationMS = w.duration
			w.outcome.Retries = s.retryCount(w.shot.ShotID, w.line.LineNo)
			w.outcome.TextSummary = TextSummary(w.result.Text)
			for i := range w.entries {
				w.entries[i].ChunkID = chunkID
			}
			cacheEntries = append(cacheEntries, w.entries...)
			lines = append(lines, CheckpointLine{
				LineNo: w.line.LineNo, Ordinal: ordinal + 1,
				BoxSummary: w.outcome.BoxSummary, TextSummary: w.outcome.TextSummary, ContentHash: w.keys.RegionHash,
				BoxJSON: mustJSON(w.line.Box), Text: w.result.Text, ConfidenceJSON: mustJSON(w.result.Chars),
			})
			detLines = append(detLines, w.line)
		}
		if chunkFailed {
			for _, w := range works {
				if w.outcome.Status != "failed" {
					w.outcome.Status = "not_committed"
				}
			}
			continue
		}
		chunkIndex := chunkIndexFromID(chunkID)
		chunk := CheckpointChunk{
			ChunkID: chunkID, ShotID: chunkShot.ShotID, ChunkIndex: chunkIndex, PredecessorID: lastCommitted,
			ShotHash: chunkShot.ContentHash, RecipeVersion: recipe.RecipeVersion, ParamVersion: recipe.ParamVersion,
			AlgorithmVersion: RecipeAlgorithmVersion, Lines: lines,
		}
		order, err := s.store.CommitChunk(chunk, cacheEntries)
		if err != nil {
			return nil, err
		}
		_ = order
		lastCommitted = chunkID
		report.CommitSequence = append(report.CommitSequence, chunkID)
		for _, w := range works {
			w.outcome.Checkpointed = true
			report.CommittedLineSequence = append(report.CommittedLineSequence, lineSeqID(w.shot.ShotID, w.line.LineNo))
		}
	}

	// Attach outcomes to shot reports in stable line order.
	outcomesByShot := map[string][]LineOutcome{}
	for _, w := range allWork {
		if w.outcome.Status == "pending" {
			w.outcome.Status = "success"
		}
		outcomesByShot[w.shot.ShotID] = append(outcomesByShot[w.shot.ShotID], w.outcome)
	}
	for i := range report.Shots {
		sr := &report.Shots[i]
		if existing := outcomesByShot[sr.ShotID]; len(existing) > 0 {
			sort.Slice(existing, func(a, b int) bool { return existing[a].LineNo < existing[b].LineNo })
			sr.Lines = append(sr.Lines, existing...)
			recs := make([]LineRecognition, 0, len(existing))
			for _, lo := range existing {
				if lo.Recognition != nil {
					recs = append(recs, *lo.Recognition)
				}
			}
			shot := s.shots[i]
			sr.Accuracy = CompareTruth(shot, recs)
			if sr.Status == "" {
				sr.Status = "success"
			}
		}
	}

	report.Budget = s.sched.Snapshot()
	report.FinishedUnixNS = time.Now().UnixNano()
	s.mu.Lock()
	s.last = report
	s.graph = combinedGraph
	s.invalidations = append([]InvalidationItem(nil), report.Invalidation...)
	s.mu.Unlock()
	return report, nil
}

func (s *Service) cacheEntry(w *lineWork, stage, key, upstream, paramVersion string, payload []byte) CacheEntry {
	return CacheEntry{
		Key: key, ShotID: w.shot.ShotID, LineNo: w.line.LineNo, Stage: stage,
		RegionHash: w.keys.RegionHash, RecipeVersion: s.recipe.RecipeVersion, ParamVersion: s.recipe.ParamVersion,
		AlgorithmVersion: RecipeAlgorithmVersion, UpstreamKey: upstream, Payload: payload,
	}
}

func (s *Service) decodeInjected(shotID string) bool {
	s.faultMu.Lock()
	defer s.faultMu.Unlock()
	return s.decodeFaults[shotID]
}

func (s *Service) lineFault(shotID string, lineNo int) string {
	s.faultMu.Lock()
	defer s.faultMu.Unlock()
	return s.lineFaults[lineFaultKey(shotID, lineNo)]
}

func (s *Service) retryCount(shotID string, lineNo int) int {
	s.faultMu.Lock()
	defer s.faultMu.Unlock()
	return s.retryCounts[lineFaultKey(shotID, lineNo)]
}

func lineFaultKey(shotID string, lineNo int) string { return fmt.Sprintf("%s:%d", shotID, lineNo) }

func (s *Service) InjectTruncatedShot(shotID string) {
	s.faultMu.Lock()
	s.decodeFaults[shotID] = true
	s.faultMu.Unlock()
}

func (s *Service) ClearTruncatedShot(shotID string) {
	s.faultMu.Lock()
	delete(s.decodeFaults, shotID)
	s.faultMu.Unlock()
}

func (s *Service) InjectLineFailure(shotID string, lineNo int, message string) {
	s.faultMu.Lock()
	s.lineFaults[lineFaultKey(shotID, lineNo)] = message
	s.faultMu.Unlock()
}

func (s *Service) ClearLineFailure(shotID string, lineNo int) {
	s.faultMu.Lock()
	delete(s.lineFaults, lineFaultKey(shotID, lineNo))
	s.retryCounts[lineFaultKey(shotID, lineNo)]++
	s.faultMu.Unlock()
}

func lineSeqID(shotID string, lineNo int) string { return fmt.Sprintf("%s#L%02d", shotID, lineNo) }

func boxSummary(r Rect) string {
	return "boxsha256:" + HashString(fmt.Sprintf("%d,%d,%d,%d", r.MinX, r.MinY, r.MaxX, r.MaxY))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func chunkLines(lines []DetectedLine, size int) [][]DetectedLine {
	if size <= 0 {
		size = 2
	}
	var out [][]DetectedLine
	for i := 0; i < len(lines); i += size {
		end := minInt(i+size, len(lines))
		out = append(out, lines[i:end])
	}
	return out
}

func stableChunkOrder(works []*lineWork) []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, w := range works {
		if !seen[w.chunkID] {
			seen[w.chunkID] = true
			out = append(out, w.chunkID)
		}
	}
	sort.Strings(out)
	return out
}

func worksForChunk(works []*lineWork, chunkID string) []*lineWork {
	out := make([]*lineWork, 0)
	for _, w := range works {
		if w.chunkID == chunkID {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].line.LineNo < out[j].line.LineNo })
	return out
}

func chunkIndexFromID(id string) int {
	parts := strings.Split(id, ":")
	if len(parts) == 0 {
		return 0
	}
	var idx int
	_, _ = fmt.Sscanf(parts[len(parts)-1], "%02d", &idx)
	return idx
}

func mergeGraph(dst, src *DependencyGraph) {
	if src == nil {
		return
	}
	for id, n := range src.Nodes {
		dst.Nodes[id] = n
	}
	dst.Edges = append(dst.Edges, src.Edges...)
}

// CompareRunReports locates the first field-level divergence.
func CompareRunReports(full, incremental *RunReport) error {
	if full == nil || incremental == nil {
		return diffError("", 0, "report", full, incremental)
	}
	if len(full.Shots) != len(incremental.Shots) {
		return diffError("", 0, "shot_count", len(full.Shots), len(incremental.Shots))
	}
	if stringsJoin(full.LineSequence) != stringsJoin(incremental.LineSequence) {
		return diffError("", 0, "line_sequence", full.LineSequence, incremental.LineSequence)
	}
	if stringsJoin(full.CommittedLineSequence) != stringsJoin(incremental.CommittedLineSequence) {
		return diffError("", 0, "committed_line_sequence", full.CommittedLineSequence, incremental.CommittedLineSequence)
	}
	for i := range full.Shots {
		fs, is := full.Shots[i], incremental.Shots[i]
		if fs.ShotID != is.ShotID {
			return diffError(is.ShotID, 0, "shot_id", fs.ShotID, is.ShotID)
		}
		if len(fs.Lines) != len(is.Lines) {
			return diffError(fs.ShotID, 0, "line_count", len(fs.Lines), len(is.Lines))
		}
		for j := range fs.Lines {
			fl, il := fs.Lines[j], is.Lines[j]
			lineNo := il.LineNo
			if fl.LineNo != il.LineNo {
				return diffError(fs.ShotID, lineNo, "line_no", fl.LineNo, il.LineNo)
			}
			if fl.Box != il.Box {
				return diffError(fs.ShotID, lineNo, "box", fl.Box, il.Box)
			}
			if (fl.Recognition == nil) != (il.Recognition == nil) {
				return diffError(fs.ShotID, lineNo, "recognition_presence", fl.Recognition != nil, il.Recognition != nil)
			}
			if fl.Recognition == nil {
				continue
			}
			if fl.Recognition.Text != il.Recognition.Text {
				return diffError(fs.ShotID, lineNo, "text", fl.Recognition.Text, il.Recognition.Text)
			}
			if len(fl.Recognition.Chars) != len(il.Recognition.Chars) {
				return diffError(fs.ShotID, lineNo, "char_count", len(fl.Recognition.Chars), len(il.Recognition.Chars))
			}
			for k := range fl.Recognition.Chars {
				fc, ic := fl.Recognition.Chars[k], il.Recognition.Chars[k]
				if fc.Char != ic.Char {
					return diffError(fs.ShotID, lineNo, fmt.Sprintf("chars[%d].char", k), fc.Char, ic.Char)
				}
				if fc.ConfidenceBPS != ic.ConfidenceBPS {
					return diffError(fs.ShotID, lineNo, fmt.Sprintf("chars[%d].confidence_bps", k), fc.ConfidenceBPS, ic.ConfidenceBPS)
				}
			}
		}
	}
	return nil
}

func stringsJoin(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "|"
		}
		out += x
	}
	return out
}
