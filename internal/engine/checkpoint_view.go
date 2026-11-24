package engine

func (e *Engine) CheckpointWriteOrder() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.WriteOrder()
}

func (e *Engine) CheckpointWriteCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.WriteCount()
}
