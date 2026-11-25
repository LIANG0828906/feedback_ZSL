package acoustic

import (
	"context"
	"sync"
	"sync/atomic"
)

const (
	DefaultMaxWorkers       = 4
	AbsoluteMaxWorkers      = 8
	DefaultPerWorkerBuffer  = 2
	AbsolutePerWorkerBuffer = 32
)

type WorkerOptions struct {
	MaxWorkers      int `json:"maxWorkers"`
	PerWorkerBuffer int `json:"perWorkerBuffer"`
}

type WorkerStats struct {
	ConfiguredWorkers  int `json:"configuredWorkers"`
	PerWorkerBuffer    int `json:"perWorkerBuffer"`
	ObservedMaxWorkers int `json:"observedMaxWorkers"`
	Tasks              int `json:"tasks"`
}

type sourceBase struct {
	source Source
	chain  ChainSummary
}

type listenerBase struct {
	listener Listener
	sources  []sourceBase
}

type reflectionTask struct {
	index          int
	listenerID     int
	sourceIndex    int
	reflectorID    int
	reflectorIndex int
	listener       Listener
	source         Source
	reflector      Reflector
	chain          ChainSummary
}

type reflectionTaskResult struct {
	index          int
	listenerID     int
	sourceIndex    int
	reflectorID    int
	reflectorIndex int
	path           ReflectionPath
	rejection      string
	err            error
}

func normalizedWorkerOptions(options WorkerOptions) WorkerOptions {
	if options.MaxWorkers <= 0 {
		options.MaxWorkers = DefaultMaxWorkers
	}
	if options.MaxWorkers > AbsoluteMaxWorkers {
		options.MaxWorkers = AbsoluteMaxWorkers
	}
	if options.PerWorkerBuffer <= 0 {
		options.PerWorkerBuffer = DefaultPerWorkerBuffer
	}
	if options.PerWorkerBuffer > AbsolutePerWorkerBuffer {
		options.PerWorkerBuffer = AbsolutePerWorkerBuffer
	}
	return options
}

func SolveRoomListenersConcurrent(ctx context.Context, project *Project, roomID int, options WorkerOptions) ([]ListenerSolution, WorkerStats, error) {
	chains, err := roomChainMap(project, roomID)
	if err != nil {
		return nil, WorkerStats{}, err
	}
	return SolveRoomListenersConcurrentWithChains(ctx, project, roomID, options, chains)
}

func SolveRoomListenersConcurrentWithChains(ctx context.Context, project *Project, roomID int, options WorkerOptions, chains map[[2]int]ChainSummary) ([]ListenerSolution, WorkerStats, error) {
	options = normalizedWorkerOptions(options)
	listeners := listenersForRoom(project, roomID)
	reflectors := reflectorsForRoom(project, roomID)
	bases := make([]listenerBase, len(listeners))
	var tasks []reflectionTask
	for listenerIndex, listener := range listeners {
		base := listenerBase{listener: listener}
		for _, source := range sortedProjectSources(project) {
			chain, ok := chains[[2]int{source.ID, listener.ID}]
			if !ok {
				return nil, WorkerStats{}, &Error{Code: CodeInternal, Op: "worker_pool", RoomIDs: []int{roomID}, SourceIDs: []int{source.ID}, ListenerIDs: []int{listener.ID}, Message: "precomputed room chain is missing"}
			}
			sourceIndex := len(base.sources)
			base.sources = append(base.sources, sourceBase{source: source, chain: chain})
			for reflectorIndex, reflector := range reflectors {
				tasks = append(tasks, reflectionTask{
					index:          len(tasks),
					listenerID:     listener.ID,
					sourceIndex:    sourceIndex,
					reflectorID:    reflector.ID,
					reflectorIndex: reflectorIndex,
					listener:       listener,
					source:         source,
					reflector:      reflector,
					chain:          chain,
				})
			}
		}
		bases[listenerIndex] = base
	}
	stats := WorkerStats{
		ConfiguredWorkers: options.MaxWorkers,
		PerWorkerBuffer:   options.PerWorkerBuffer,
		Tasks:             len(tasks),
	}
	if len(tasks) == 0 {
		return assembleListenerSolutions(bases, nil), stats, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, stats, err
	}

	workerCount := options.MaxWorkers
	if len(tasks) < workerCount {
		workerCount = len(tasks)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan reflectionTask)
	results := make(chan reflectionTaskResult, workerCount*(options.PerWorkerBuffer+1))
	var waitGroup sync.WaitGroup
	var active int64
	var observed int64

	for workerIndex := 0; workerIndex < workerCount; workerIndex++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			now := atomic.AddInt64(&active, 1)
			for {
				old := atomic.LoadInt64(&observed)
				if now <= old || atomic.CompareAndSwapInt64(&observed, old, now) {
					break
				}
			}
			defer atomic.AddInt64(&active, -1)
			buffer := make([]reflectionTaskResult, 0, options.PerWorkerBuffer)
			flush := func() bool {
				for _, item := range buffer {
					select {
					case results <- item:
					case <-workerCtx.Done():
						return false
					}
				}
				buffer = buffer[:0]
				return true
			}
			for {
				select {
				case <-workerCtx.Done():
					flush()
					return
				case task, ok := <-jobs:
					if !ok {
						flush()
						return
					}
					path, rejection, err := solveReflection(project, task.source, task.listener, task.reflector, task.chain)
					buffer = append(buffer, reflectionTaskResult{
						index:          task.index,
						listenerID:     task.listenerID,
						sourceIndex:    task.sourceIndex,
						reflectorID:    task.reflectorID,
						reflectorIndex: task.reflectorIndex,
						path:           path,
						rejection:      rejection,
						err:            err,
					})
					if err != nil {
						cancel()
					}
					if len(buffer) == cap(buffer) {
						if !flush() {
							return
						}
					}
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, task := range tasks {
			select {
			case jobs <- task:
			case <-workerCtx.Done():
				return
			}
		}
	}()
	go func() {
		waitGroup.Wait()
		close(results)
	}()

	collected := make([]reflectionTaskResult, 0, len(tasks))
	for result := range results {
		collected = append(collected, result)
	}
	stats.ObservedMaxWorkers = int(atomic.LoadInt64(&observed))

	var firstErr error
	firstIndex := len(tasks)
	for _, result := range collected {
		if result.err != nil && result.index < firstIndex {
			firstErr = result.err
			firstIndex = result.index
		}
	}
	if firstErr != nil {
		if ctx.Err() != nil {
			return nil, stats, ctx.Err()
		}
		return nil, stats, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, stats, err
	}
	if len(collected) != len(tasks) {
		return nil, stats, &Error{Code: CodeInternal, Op: "worker_pool", RoomIDs: []int{roomID}, Message: "worker pool returned an incomplete reflection set"}
	}
	sortReflectionTaskResults(collected)
	return assembleListenerSolutions(bases, collected), stats, nil
}

func roomChainMap(project *Project, roomID int) (map[[2]int]ChainSummary, error) {
	chains := make(map[[2]int]ChainSummary)
	for _, listener := range listenersForRoom(project, roomID) {
		for _, source := range sortedProjectSources(project) {
			chain, err := FindChain(project, source.ID, listener.ID)
			if err != nil {
				return nil, noChainError(source.ID, listener.ID, err)
			}
			chains[[2]int{source.ID, listener.ID}] = chain
		}
	}
	return chains, nil
}

func assembleListenerSolutions(bases []listenerBase, results []reflectionTaskResult) []ListenerSolution {
	solutions := make([]ListenerSolution, len(bases))
	for listenerIndex, base := range bases {
		solution := ListenerSolution{
			ListenerID: base.listener.ID,
			RoomID:     base.listener.RoomID,
			Sources:    make([]SourceReflectionResult, len(base.sources)),
		}
		for sourceIndex, source := range base.sources {
			solution.Sources[sourceIndex] = SourceReflectionResult{
				SourceID: source.source.ID,
				Chain:    source.chain,
			}
		}
		solutions[listenerIndex] = solution
	}
	listenerIndexByID := make(map[int]int, len(bases))
	for index, base := range bases {
		listenerIndexByID[base.listener.ID] = index
	}
	for _, result := range results {
		listenerIndex, ok := listenerIndexByID[result.listenerID]
		if !ok || result.sourceIndex < 0 || result.sourceIndex >= len(solutions[listenerIndex].Sources) {
			continue
		}
		sourceResult := &solutions[listenerIndex].Sources[result.sourceIndex]
		if result.rejection != "" {
			sourceResult.Rejections = append(sourceResult.Rejections, ReflectionRejection{
				SourceID:    sourceResult.SourceID,
				ReflectorID: result.reflectorID,
				Reason:      result.rejection,
			})
			continue
		}
		sourceResult.Paths = append(sourceResult.Paths, result.path)
	}
	for listenerIndex := range solutions {
		solution := &solutions[listenerIndex]
		for sourceIndex := range solution.Sources {
			sourceResult := &solution.Sources[sourceIndex]
			sortReflectionPaths(sourceResult.Paths)
			for _, path := range sourceResult.Paths {
				solution.Paths = append(solution.Paths, path)
				solution.ValidPathCount++
			}
		}
		sortReflectionPaths(solution.Paths)
	}
	return solutions
}

func sortedProjectSources(project *Project) []Source {
	sources := append([]Source(nil), project.Sources...)
	sortSourcesByID(sources)
	return sources
}

func listenersForRoom(project *Project, roomID int) []Listener {
	var listeners []Listener
	for _, listener := range project.Listeners {
		if listener.RoomID == roomID {
			listeners = append(listeners, listener)
		}
	}
	sortListeners(listeners)
	return listeners
}

func sortReflectionTaskResults(results []reflectionTaskResult) {
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].index < results[j-1].index; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
}
