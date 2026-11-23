package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"textureworkbench/internal/tex"
)

type RunOptions struct {
	MaxBlocks int `json:"max_blocks"`
}

type blockResult struct {
	index      int
	converted  *tex.Converted
	err        error
	durationMS float64
}

type Session struct {
	id      string
	image   *tex.Image
	desc    tex.Descriptor
	cfg     Config
	mapping tex.Mapping
	store   *Store

	mu           sync.RWMutex
	blocks       []*Block
	running      bool
	paused       bool
	cancelled    bool
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	stats        PathStats
	restore      RestoreInfo
	generations  []GenerationInfo
	nextSequence int
	faults       map[int]int
	runStarted   time.Time
}

func OpenSession(image *tex.Image, desc tex.Descriptor, cfg Config) (*Session, error) {
	if image == nil {
		return nil, tex.ErrPara("image", "source image is required")
	}
	if err := image.Validate(); err != nil {
		return nil, err
	}
	image.EnsureHash()
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	desc = desc.Normalize()
	if err := desc.ValidateForImage(image); err != nil {
		return nil, err
	}
	mapping, err := tex.BuildMapping(desc)
	if err != nil {
		return nil, err
	}
	key := cfg.SessionKey
	if strings.TrimSpace(key) == "" {
		key = image.ID
	}
	cfg.SessionKey = key
	header := checkpointHeader{
		Schema:            checkpointSchema,
		SessionKey:        key,
		ImageID:           image.ID,
		ContentHash:       image.ContentHash,
		DescriptorVersion: desc.Version(),
		ParameterVersion:  cfg.ParameterVersion,
		AlgorithmVersion:  tex.AlgorithmVersion,
	}
	store, err := OpenStore(cfg.CheckpointRoot, key, header)
	if err != nil {
		return nil, err
	}
	session := &Session{
		id:      key,
		image:   image,
		desc:    desc,
		cfg:     cfg,
		mapping: mapping,
		store:   store,
		faults:  make(map[int]int),
	}
	session.blocks = buildBlocks(image, cfg, desc.Version(), cfg.ParameterVersion)
	load, err := store.Load()
	if err != nil {
		return nil, err
	}
	session.nextSequence = store.Sequence()
	session.restore.Attempted = true
	session.restore.Invalidated = load.Invalidated
	session.restore.Reasons = append([]string(nil), load.Reasons...)
	if !load.Invalidated && len(load.Records) > 0 {
		if err := session.hydrate(load.Records); err != nil {
			if invalidateErr := store.Invalidate("checkpoint_semantic_validation_failed"); invalidateErr != nil {
				return nil, invalidateErr
			}
			session.restore.Invalidated = true
			session.restore.Reasons = append(session.restore.Reasons, err.Error())
			session.blocks = buildBlocks(image, cfg, desc.Version(), cfg.ParameterVersion)
			session.nextSequence = 0
		} else {
			session.restore.Used = true
		}
	}
	if session.restore.Invalidated {
		for _, block := range session.blocks {
			block.Invalidated = true
			block.InvalidationReason = "persisted_snapshot_invalidated"
		}
	}
	return session, nil
}

func buildBlocks(image *tex.Image, cfg Config, descriptorVersion, parameterVersion string) []*Block {
	var blocks []*Block
	row := 0
	for y := 0; y < image.Height; y += cfg.BlockHeight {
		col := 0
		for x := 0; x < image.Width; x += cfg.BlockWidth {
			width := cfg.BlockWidth
			if x+width > image.Width {
				width = image.Width - x
			}
			height := cfg.BlockHeight
			if y+height > image.Height {
				height = image.Height - y
			}
			block := &Block{
				Index:             len(blocks),
				Row:               row,
				Col:               col,
				Rect:              tex.Rect{X: x, Y: y, W: width, H: height},
				State:             BlockPending,
				DescriptorVersion: descriptorVersion,
				ParameterVersion:  parameterVersion,
				AlgorithmVersion:  tex.AlgorithmVersion,
			}
			blocks = append(blocks, block)
			col++
		}
		row++
	}
	return blocks
}

func (s *Session) hydrate(records map[int]blockRecord) error {
	targetChannels := s.desc.TargetLayout.ChannelCount()
	maxValue := uint16(255)
	if s.desc.TargetBitDepth == 16 {
		maxValue = 65535
	}
	for index, record := range records {
		if index < 0 || index >= len(s.blocks) {
			return fmt.Errorf("checkpoint references unknown block %d", index)
		}
		block := s.blocks[index]
		if record.Rect != block.Rect || record.Row != block.Row || record.Col != block.Col {
			return fmt.Errorf("checkpoint block %d geometry mismatch", index)
		}
		count := record.Rect.Pixels() * targetChannels
		output, err := decodeUint16(record.Output, count)
		if err != nil {
			return fmt.Errorf("checkpoint block %d output: %w", index, err)
		}
		errors, err := decodeInt32(record.Errors, count)
		if err != nil {
			return fmt.Errorf("checkpoint block %d errors: %w", index, err)
		}
		for _, value := range output {
			if value > maxValue {
				return fmt.Errorf("checkpoint block %d output value %d exceeds target bit depth", index, value)
			}
		}
		block.State = BlockCommitted
		block.Output = output
		block.Errors = errors
		block.OutputHash = record.OutputHash
		block.ErrorHash = record.ErrorHash
		block.DescriptorVersion = record.DescriptorVersion
		block.ParameterVersion = record.ParameterVersion
		block.AlgorithmVersion = record.AlgorithmVersion
		block.CacheHit = true
		block.Attempts = 1
		s.restore.RestoredBlocks++
	}
	s.restore.PersistedSequence = s.nextSequence - 1
	s.stats.CacheHits = int64(s.restore.RestoredBlocks)
	return nil
}

func (s *Session) ID() string {
	return s.id
}

func (s *Session) Image() *tex.Image {
	return s.image
}

func (s *Session) Descriptor() tex.Descriptor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.desc
}

func (s *Session) InverseMapping() ([]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mapping.SourceFromTarget()
}

func (s *Session) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Session) Snapshot() SessionSnapshot {
	s.mu.RLock()
	blocks := make([]Block, len(s.blocks))
	progress := Progress{Total: len(s.blocks)}
	for i, block := range s.blocks {
		blocks[i] = *block
		blocks[i].Output = nil
		blocks[i].Errors = nil
		switch block.State {
		case BlockCommitted:
			progress.Committed++
		case BlockFailed:
			progress.Failed++
		case BlockProcessing:
			progress.Processing++
		default:
			progress.Pending++
		}
	}
	snapshot := SessionSnapshot{
		SessionID:         s.id,
		ImageID:           s.image.ID,
		ImagePath:         s.image.Path,
		Width:             s.image.Width,
		Height:            s.image.Height,
		SourceBitDepth:    s.image.BitDepth,
		SourceLayout:      s.image.Layout,
		Descriptor:        s.desc,
		DescriptorVersion: s.desc.Version(),
		ParameterVersion:  s.cfg.ParameterVersion,
		AlgorithmVersion:  tex.AlgorithmVersion,
		ContentHash:       s.image.ContentHash,
		Config:            s.cfg,
		Blocks:            blocks,
		Progress:          progress,
		Running:           s.running,
		Paused:            s.paused,
		Cancelled:         s.cancelled,
		Stats:             s.stats,
		Restore:           s.restore,
		Generations:       append([]GenerationInfo(nil), s.generations...),
		Mapping:           s.mapping,
	}
	snapshot.Complete = progress.Committed == progress.Total
	snapshot.ExportReady = snapshot.Complete
	s.mu.RUnlock()
	if snapshot.Complete {
		if result, err := s.Result(); err == nil {
			hist := result.Histogram
			stats := result.QuantStats
			snapshot.Histogram = &hist
			snapshot.QuantStats = &stats
		}
	}
	return snapshot
}

func (s *Session) Result() (*tex.Converted, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.blocks) == 0 {
		return nil, tex.ErrState("session has no blocks")
	}
	for _, block := range s.blocks {
		if block.State != BlockCommitted {
			return nil, &tex.Error{
				Code:    tex.CodeState,
				Message: "all blocks must be committed before assembling a result",
				Details: map[string]any{"block_index": block.Index, "state": block.State},
			}
		}
	}
	targetChannels := s.desc.TargetLayout.ChannelCount()
	totalSamples := s.image.PixelCount() * targetChannels
	pixels := make([]uint16, totalSamples)
	errors := make([]int32, totalSamples)
	for _, block := range s.blocks {
		for row := 0; row < block.Rect.H; row++ {
			srcStart := row * block.Rect.W * targetChannels
			dstStart := ((block.Rect.Y+row)*s.image.Width + block.Rect.X) * targetChannels
			copy(pixels[dstStart:dstStart+block.Rect.W*targetChannels], block.Output[srcStart:srcStart+block.Rect.W*targetChannels])
			copy(errors[dstStart:dstStart+block.Rect.W*targetChannels], block.Errors[srcStart:srcStart+block.Rect.W*targetChannels])
		}
	}
	return &tex.Converted{
		Width:             s.image.Width,
		Height:            s.image.Height,
		BitDepth:          s.desc.TargetBitDepth,
		Layout:            s.desc.TargetLayout,
		Pixels:            pixels,
		Errors:            errors,
		QuantStats:        tex.ComputeQuantStats(errors, targetChannels, s.image.BitDepth, s.desc.TargetBitDepth),
		Histogram:         tex.BuildHistogram(pixels, targetChannels, s.desc.TargetLayout, s.desc.TargetBitDepth),
		Mapping:           s.mapping,
		DescriptorVersion: s.desc.Version(),
		AlgorithmVersion:  tex.AlgorithmVersion,
	}, nil
}

func (s *Session) StartSegmented(opts RunOptions) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return tex.NewError(tex.CodeBusy, "", "a conversion run is already active")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.running = true
	s.paused = false
	s.cancelled = false
	s.cancel = cancel
	s.runStarted = time.Now()
	s.stats = PathStats{
		Path:      "realtime_segmented",
		CacheHits: int64(s.restore.RestoredBlocks),
	}
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.executeRun(ctx, opts)
	}()
	return nil
}

func (s *Session) RunSegmented(opts RunOptions) error {
	if err := s.StartSegmented(opts); err != nil {
		return err
	}
	s.wg.Wait()
	return nil
}

func (s *Session) Pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.paused = true
		if s.cancel != nil {
			s.cancel()
		}
	}
}

func (s *Session) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.cancelled = true
		if s.cancel != nil {
			s.cancel()
		}
	}
}

func (s *Session) Wait() {
	s.wg.Wait()
}

func (s *Session) Close() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Session) executeRun(ctx context.Context, opts RunOptions) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		if s.paused || s.cancelled {
			// Preserve the user-requested terminal intent.
		} else {
			s.paused = false
		}
		s.stats.TotalTimeMS = float64(time.Since(s.runStarted).Microseconds()) / 1000
		s.mu.Unlock()
	}()
	s.mu.RLock()
	candidates := make([]int, 0, len(s.blocks))
	for _, block := range s.blocks {
		if block.State == BlockPending {
			candidates = append(candidates, block.Index)
			if opts.MaxBlocks > 0 && len(candidates) >= opts.MaxBlocks {
				break
			}
		}
	}
	s.mu.RUnlock()
	if len(candidates) == 0 {
		return
	}
	jobs := make(chan int, s.cfg.QueueDepth)
	results := make(chan blockResult, len(candidates))
	var workers sync.WaitGroup
	workerCount := s.cfg.MaxWorkers
	if workerCount > len(candidates) {
		workerCount = len(candidates)
	}
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				results <- s.convertBlock(index)
			}
		}()
	}
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		s.collectResults(results, candidates)
	}()
	for _, index := range candidates {
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- index:
		default:
			s.mu.Lock()
			s.stats.BufferQueueWaits++
			if s.stats.BufferWaitReason == "" {
				s.stats.BufferWaitReason = "jobs queue full while per-worker pixel buffer limit is saturated; block blocked and queued, never dropped"
			}
			s.mu.Unlock()
			select {
			case jobs <- index:
			case <-ctx.Done():
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	close(results)
	<-collectorDone
}

func (s *Session) convertBlock(index int) (result blockResult) {
	start := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			result = blockResult{
				index:      index,
				err:        fmt.Errorf("recovered panic in block %d: %v", index, recovered),
				durationMS: float64(time.Since(start).Microseconds()) / 1000,
			}
		}
	}()
	s.mu.Lock()
	block := s.blocks[index]
	if err := Transition(block.State, BlockProcessing); err != nil {
		s.mu.Unlock()
		return blockResult{index: index, err: err, durationMS: float64(time.Since(start).Microseconds()) / 1000}
	}
	block.State = BlockProcessing
	block.Invalidated = false
	block.InvalidationReason = ""
	block.Attempts++
	if block.Attempts > 1 {
		block.Retries++
		s.stats.Retries++
	}
	s.stats.BlockRecomputes++
	desc := s.desc
	image := s.image
	fault := false
	if remaining := s.faults[index]; remaining > 0 {
		fault = true
		s.faults[index] = remaining - 1
		if s.faults[index] == 0 {
			delete(s.faults, index)
		}
	}
	rect := block.Rect
	s.mu.Unlock()

	var converted *tex.Converted
	var err error
	if fault {
		err = fmt.Errorf("injected conversion failure in block %d at pixel_stage", index)
	} else {
		converted, err = tex.ConvertBlock(image, desc, rect, tex.ConvertOptions{})
	}
	duration := float64(time.Since(start).Microseconds()) / 1000
	return blockResult{index: index, converted: converted, err: err, durationMS: duration}
}

func (s *Session) collectResults(results <-chan blockResult, order []int) {
	pending := make(map[int]blockResult)
	next := 0
	drain := func() {
		for next < len(order) {
			result, ok := pending[order[next]]
			if !ok {
				return
			}
			delete(pending, order[next])
			s.finishBlock(result)
			next++
		}
	}
	for result := range results {
		pending[result.index] = result
		drain()
	}
	drain()
}

func (s *Session) finishBlock(result blockResult) {
	s.mu.Lock()
	block := s.blocks[result.index]
	block.DurationMS = result.durationMS
	s.stats.ConversionTimeMS += result.durationMS
	if result.err != nil {
		if transitionErr := Transition(block.State, BlockFailed); transitionErr == nil {
			block.State = BlockFailed
		}
		block.Failure = result.err.Error()
		s.stats.FailedAttempts++
		s.mu.Unlock()
		return
	}
	descVersion := s.desc.Version()
	paramVersion := s.cfg.ParameterVersion
	rect := block.Rect
	seq := s.nextSequence
	s.mu.Unlock()

	checkpointStart := time.Now()
	meta, err := s.store.Commit(seq, result.index, block.Row, block.Col, rect, result.converted)
	checkpointDuration := float64(time.Since(checkpointStart).Microseconds()) / 1000

	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.CheckpointTimeMS += checkpointDuration
	if err != nil {
		if transitionErr := Transition(block.State, BlockFailed); transitionErr == nil {
			block.State = BlockFailed
		}
		block.Failure = "checkpoint commit failed: " + err.Error()
		s.stats.FailedAttempts++
		return
	}
	if transitionErr := Transition(block.State, BlockCommitted); transitionErr != nil {
		block.Failure = transitionErr.Error()
		s.stats.FailedAttempts++
		return
	}
	block.State = BlockCommitted
	block.Output = append([]uint16(nil), result.converted.Pixels...)
	block.Errors = append([]int32(nil), result.converted.Errors...)
	block.OutputHash = meta.OutputHash
	block.ErrorHash = meta.ErrorHash
	block.DescriptorVersion = descVersion
	block.ParameterVersion = paramVersion
	block.AlgorithmVersion = tex.AlgorithmVersion
	block.Failure = ""
	block.CacheHit = false
	s.nextSequence++
	s.stats.SuccessfulCommits++
}

func (s *Session) RetryBlock(index int) (Block, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return Block{}, tex.NewError(tex.CodeBusy, "", "cannot retry while a conversion run is active")
	}
	if index < 0 || index >= len(s.blocks) {
		s.mu.Unlock()
		return Block{}, tex.ErrPara("block_index", "block index is out of range")
	}
	block := s.blocks[index]
	if block.State != BlockFailed {
		state := block.State
		s.mu.Unlock()
		return Block{}, &tex.Error{
			Code:    tex.CodeState,
			Message: "only failed blocks can be retried",
			Details: map[string]any{"block_index": index, "state": state},
		}
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.running = true
	s.paused = false
	s.cancelled = false
	s.cancel = cancel
	s.runStarted = time.Now()
	s.stats.Path = "block_retry"
	s.mu.Unlock()

	result := s.convertBlock(index)
	s.finishBlock(result)
	s.mu.Lock()
	s.running = false
	s.cancel = nil
	s.stats.TotalTimeMS += float64(time.Since(s.runStarted).Microseconds()) / 1000
	snapshot := *s.blocks[index]
	snapshot.Output = nil
	snapshot.Errors = nil
	s.mu.Unlock()
	s.wg.Wait()
	if result.err != nil {
		return snapshot, result.err
	}
	if snapshot.State != BlockCommitted {
		return snapshot, &tex.Error{Code: tex.CodeCommit, Message: snapshot.Failure}
	}
	return snapshot, nil
}

func (s *Session) InjectFault(index, times int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.blocks) {
		return tex.ErrPara("block_index", "block index is out of range")
	}
	if times <= 0 {
		times = 1
	}
	s.faults[index] = times
	return nil
}

func (s *Session) Reconfigure(desc tex.Descriptor, parameterVersion string) ([]InvalidatedBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil, tex.NewError(tex.CodeBusy, "", "cannot reconfigure while a conversion run is active")
	}
	desc = desc.Normalize()
	if err := desc.ValidateForImage(s.image); err != nil {
		return nil, err
	}
	if strings.TrimSpace(parameterVersion) == "" {
		parameterVersion = s.cfg.ParameterVersion
	}
	if desc.Version() == s.desc.Version() && parameterVersion == s.cfg.ParameterVersion {
		return nil, nil
	}
	reason := "target_descriptor_changed"
	if desc.Version() == s.desc.Version() {
		reason = "parameter_version_changed"
	}
	invalidated := make([]InvalidatedBlock, 0, len(s.blocks))
	for _, block := range s.blocks {
		if block.State != BlockPending || block.OutputHash != "" {
			invalidated = append(invalidated, InvalidatedBlock{
				Index:             block.Index,
				Rect:              block.Rect,
				State:             block.State,
				Reason:            reason,
				DescriptorVersion: block.DescriptorVersion,
				ParameterVersion:  block.ParameterVersion,
				OutputHash:        block.OutputHash,
			})
		}
	}
	if err := s.store.Invalidate(reason); err != nil {
		return nil, err
	}
	generation := GenerationInfo{
		DescriptorVersion: s.desc.Version(),
		ParameterVersion:  s.cfg.ParameterVersion,
		AlgorithmVersion:  tex.AlgorithmVersion,
		InvalidatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Reason:            reason,
		Blocks:            invalidated,
	}
	s.generations = append(s.generations, generation)
	s.desc = desc
	s.cfg.ParameterVersion = parameterVersion
	mapping, err := tex.BuildMapping(desc)
	if err != nil {
		return nil, err
	}
	s.mapping = mapping
	s.blocks = buildBlocks(s.image, s.cfg, desc.Version(), parameterVersion)
	for _, block := range s.blocks {
		block.Invalidated = true
		block.InvalidationReason = reason
	}
	s.nextSequence = 0
	s.restore = RestoreInfo{}
	s.stats = PathStats{Path: "realtime_segmented", HistoryReset: true}
	return invalidated, nil
}
