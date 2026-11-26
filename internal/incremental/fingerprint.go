package incremental

import (
	"sort"
	"strconv"

	"schedlab/internal/domain"
)

// fnv is a deterministic 64-bit FNV-1a hash used only for cache fingerprints.
type fnv struct {
	state uint64
}

func newFNV() *fnv { return &fnv{state: 14695981039346656037} }

func (f *fnv) write(b []byte) {
	for _, c := range b {
		f.state ^= uint64(c)
		f.state *= 1099511628211
	}
}

func (f *fnv) writeString(s string) {
	f.write([]byte{0x02})
	f.write([]byte(s))
}

func (f *fnv) writeInt(v int64) {
	f.writeString(strconv.FormatInt(v, 10))
}

func (f *fnv) writeStringMap(m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f.writeString(k)
		f.writeString(m[k])
	}
}

func (f *fnv) writeRule(r domain.AffinityRule) {
	f.writeString(r.ID)
	f.writeString(string(r.Phase))
	f.writeString(string(r.Mode))
	f.writeString(r.TopologyKey)
	f.writeInt(r.WeightBps)
	f.writeInt(r.MaxCoLocated)
	f.writeStringMap(r.Selector.MatchLabels)
}

func (f *fnv) sum() string { return strconv.FormatUint(f.state, 16) }
