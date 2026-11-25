package engine

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"graphlab/internal/model"
)

func esc(s string) string {
	if s == "" {
		return "_empty"
	}
	return url.QueryEscape(s)
}

func VertexEntity(id string) string { return "v:" + id }
func EdgeEntity(id string) string   { return "e:" + id }
func ShardDependency(id string) string {
	return "s:" + id
}

func VertexIdentityKey(id string) string { return "v:vertex:" + esc(id) }
func EdgeIdentityKey(id string) string   { return "e:edge:" + esc(id) }
func LabelKey(label string) string       { return "v:label:" + esc(label) }
func OutKey(from string) string          { return "e:out:" + esc(from) }
func LevelKey(from, level string) string { return "e:out:" + esc(from) + ":lvl:" + esc(level) }
func TargetLabelKey(from, label string) string {
	return "e:out:" + esc(from) + ":dst:" + esc(label)
}

func floorDiv(value, divisor int64) int64 {
	if divisor <= 0 {
		return 0
	}
	if value >= 0 {
		return value / divisor
	}
	return int64(math.Floor(float64(value) / float64(divisor)))
}

func intBucketKey(attr string, value, width int64) string {
	return fmt.Sprintf("v:int:%s:b%d", esc(attr), floorDiv(value, width))
}

func fixedBucketKey(attr string, micros, width int64) string {
	return fmt.Sprintf("v:fixed:%s:b%d", esc(attr), floorDiv(micros, width))
}

func stringPrefixKey(attr, value string, n int) string {
	runes := []rune(value)
	if n > len(runes) {
		n = len(runes)
	}
	prefix := string(runes[:n])
	if prefix == "" {
		prefix = "_empty"
	}
	return "v:string:" + esc(attr) + ":p:" + esc(prefix)
}

func VertexKeys(v model.Vertex, rules []model.IndexRule) []string {
	keys := []string{VertexIdentityKey(v.ID)}
	for _, label := range v.Labels {
		keys = append(keys, LabelKey(label))
	}
	for _, rule := range rules {
		value, ok := v.Attrs[rule.Attribute]
		if !ok || value.Kind != rule.Kind {
			continue
		}
		switch value.Kind {
		case model.AttrInt:
			keys = append(keys, intBucketKey(rule.Attribute, value.Int, rule.Bucket))
		case model.AttrFixed:
			keys = append(keys, fixedBucketKey(rule.Attribute, value.FixedMicros, rule.Bucket))
		case model.AttrString:
			keys = append(keys, stringPrefixKey(rule.Attribute, value.String, rule.PrefixLen))
		}
	}
	return sortedUnique(keys)
}

func EdgeKeys(e model.Edge, p *model.Project) []string {
	keys := []string{EdgeIdentityKey(e.ID), OutKey(e.From)}
	if p.HierarchyEnabled {
		for _, level := range e.Levels {
			keys = append(keys, LevelKey(e.From, level))
		}
	}
	if !e.Directed {
		// Undirected edges are also reachable from their target side.
		keys = append(keys, OutKey(e.To))
	}
	return sortedUnique(keys)
}

func sortedUnique(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

func AllEntityKeys(v model.Vertex, e model.Edge, p *model.Project) []string {
	keys := VertexKeys(v, p.IndexRules)
	keys = append(keys, EdgeKeys(e, p)...)
	return sortedUnique(keys)
}

// Every record carries an identity key. Updates that do not cross a range
// boundary still bump this key, making propagation observable and cache
// invalidation exact.
func EntityIdentityKey(entity string) string {
	if strings.HasPrefix(entity, "v:") {
		return VertexIdentityKey(strings.TrimPrefix(entity, "v:"))
	}
	if strings.HasPrefix(entity, "e:") {
		return EdgeIdentityKey(strings.TrimPrefix(entity, "e:"))
	}
	return ""
}

func ParseBucketSuffix(key string) int64 {
	i := strings.LastIndex(key, ":b")
	if i < 0 {
		return 0
	}
	v, _ := strconv.ParseInt(key[i+2:], 10, 64)
	return v
}

func RuneLen(s string) int { return utf8.RuneCountInString(s) }
