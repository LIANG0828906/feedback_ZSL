package editorpick

import "sync"

func cacheKey(prefix string, q any) string {
	h, err := canonicalHash(q)
	if err != nil {
		return prefix + ":uncacheable"
	}
	return prefix + ":" + h[:16]
}

func (e *Engine) RaycastCached(q RayQuery) (*RayResult, bool, error) {
	key := cacheKey("ray", q)
	e.mu.RLock()
	entry := e.cache[key]
	revision := e.revision
	e.mu.RUnlock()
	if entry != nil && entry.ray != nil {
		return entry.ray, true, nil
	}
	result, err := e.Raycast(q)
	if err != nil {
		return nil, false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revision != revision {
		return result, false, nil
	}
	if existing := e.cache[key]; existing != nil && existing.ray != nil {
		return existing.ray, true, nil
	}
	e.cache[key] = &queryCacheEntry{key: key, ray: result}
	for _, dep := range result.Visited {
		e.graph.AddEdge(entryObject(dep), queryObject(key), "ray-query-dependency")
	}
	return result, false, nil
}

func (e *Engine) BoxSelectCached(q BoxQuery) (*BoxResult, bool, error) {
	key := cacheKey("box", q)
	e.mu.RLock()
	entry := e.cache[key]
	revision := e.revision
	e.mu.RUnlock()
	if entry != nil && entry.box != nil {
		return entry.box, true, nil
	}
	result, err := e.BoxSelect(q)
	if err != nil {
		return nil, false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revision != revision {
		return result, false, nil
	}
	if existing := e.cache[key]; existing != nil && existing.box != nil {
		return existing.box, true, nil
	}
	e.cache[key] = &queryCacheEntry{key: key, box: result}
	for _, dep := range result.Visited {
		e.graph.AddEdge(entryObject(dep), queryObject(key), "box-query-dependency")
	}
	return result, false, nil
}

func (e *Engine) QueryPointCached(q PointQuery) (*PointResult, bool, error) {
	key := cacheKey("point", q)
	e.mu.RLock()
	entry := e.cache[key]
	revision := e.revision
	e.mu.RUnlock()
	if entry != nil && entry.point != nil {
		return entry.point, true, nil
	}
	result, err := e.QueryPoint(q)
	if err != nil {
		return nil, false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revision != revision {
		return result, false, nil
	}
	e.cache[key] = &queryCacheEntry{key: key, point: result}
	for _, dep := range result.Visited {
		e.graph.AddEdge(entryObject(dep), queryObject(key), "point-query-dependency")
	}
	return result, false, nil
}

type RayOutcome struct {
	Result *RayResult
	Err    error
}

// RaycastParallel executes independent queries on worker goroutines. Results
// are returned in request order; query state is never used for ordering.
func RaycastParallel(e *Engine, queries []RayQuery) []RayOutcome {
	out := make([]RayOutcome, len(queries))
	var wg sync.WaitGroup
	for i := range queries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := e.Raycast(queries[i])
			out[i] = RayOutcome{Result: r, Err: err}
		}(i)
	}
	wg.Wait()
	return out
}

type BoxOutcome struct {
	Result *BoxResult
	Err    error
}

func BoxSelectParallel(e *Engine, queries []BoxQuery) []BoxOutcome {
	out := make([]BoxOutcome, len(queries))
	var wg sync.WaitGroup
	for i := range queries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := e.BoxSelect(queries[i])
			out[i] = BoxOutcome{Result: r, Err: err}
		}(i)
	}
	wg.Wait()
	return out
}

func (e *Engine) CacheSize() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.cache)
}
