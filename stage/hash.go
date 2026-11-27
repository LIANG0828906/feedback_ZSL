package stage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

func canonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func HashJSON(v any) (string, error) {
	data, err := canonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func FixtureContentHash(f Fixture) string {
	h, err := HashJSON(f)
	if err != nil {
		panic(err)
	}
	return h
}

func CueContentHash(c Cue) string {
	h, err := HashJSON(c)
	if err != nil {
		panic(err)
	}
	return h
}

// FixtureCacheKey includes the fixture content, sample time, parameter version
// and every upstream cue/content key. Upstream keys are sorted so insertion
// order cannot affect the cache identity.
func FixtureCacheKey(fixtureHash string, timeMS int64, parameterVersion string, upstream ...string) string {
	keys := append([]string(nil), upstream...)
	sort.Strings(keys)
	payload := struct {
		FixtureHash      string   `json:"fixture_hash"`
		TimeMS           int64    `json:"time_ms"`
		ParameterVersion string   `json:"parameter_version"`
		Upstream         []string `json:"upstream"`
	}{fixtureHash, timeMS, parameterVersion, keys}
	h, err := HashJSON(payload)
	if err != nil {
		panic(err)
	}
	return h
}

func WindowCacheKey(cacheKeys []string, samples []int64, parameterVersion string) string {
	keys := append([]string(nil), cacheKeys...)
	sort.Strings(keys)
	times := append([]int64(nil), samples...)
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	h, err := HashJSON(struct {
		CacheKeys        []string `json:"cache_keys"`
		SampleTimes      []int64  `json:"sample_times_ms"`
		ParameterVersion string   `json:"parameter_version"`
	}{keys, times, parameterVersion})
	if err != nil {
		panic(err)
	}
	return h
}

func windowContentHash(w WindowResult) (string, error) {
	copy := w
	copy.ContentHash = ""
	return HashJSON(copy)
}

func FinalizeWindow(w WindowResult) (WindowResult, error) {
	h, err := windowContentHash(w)
	if err != nil {
		return w, err
	}
	w.ContentHash = h
	return w, nil
}

func dependencyNodeID(fixtureID string, window int, time int64) string {
	return fmt.Sprintf("fixture:%s:window:%d:time:%d", fixtureID, window, time)
}
