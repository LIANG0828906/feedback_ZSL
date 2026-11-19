package gateway

import (
	"hash/fnv"
	"sync"
)

type keyedLocks struct {
	shards [64]sync.Mutex
}

func (k *keyedLocks) Lock(key string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	idx := int(h.Sum32() % uint32(len(k.shards)))
	k.shards[idx].Lock()
	return k.shards[idx].Unlock
}
