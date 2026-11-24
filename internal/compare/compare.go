// Package compare locates the first route/frame/field difference between two
// persisted snapshots without invoking the conversion chain.
package compare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"hdrproof/internal/apperr"
	"hdrproof/internal/snapshot"
)

type Result struct {
	Left           string `json:"left"`
	Right          string `json:"right"`
	Equal          bool   `json:"equal"`
	FramesCompared int    `json:"frames"`
	RoutesCompared int    `json:"routes"`
}

func Snapshots(ctx context.Context, leftPath, rightPath string) (*Result, error) {
	left, right, err := readPair(ctx, leftPath, rightPath)
	if err != nil {
		return nil, err
	}
	return compareLoaded(ctx, leftPath, rightPath, left, right)
}

type loadedDB struct {
	routes []string
	frames map[snapshot.FrameKey]snapshot.StoredFrame
}

func readPair(ctx context.Context, leftPath, rightPath string) (*loadedDB, *loadedDB, error) {
	left, err := readOne(ctx, leftPath)
	if err != nil {
		return nil, nil, err
	}
	right, err := readOne(ctx, rightPath)
	if err != nil {
		return nil, nil, err
	}
	return left, right, nil
}

func readOne(ctx context.Context, path string) (*loadedDB, error) {
	store, err := snapshot.OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	routes, err := store.RouteJSON(ctx)
	if err != nil {
		return nil, err
	}
	_, frames, err := store.ReadAll(ctx)
	if err != nil {
		return nil, err
	}
	out := &loadedDB{frames: frames}
	for _, route := range routes {
		out.routes = append(out.routes, route.ID)
	}
	return out, nil
}

func compareLoaded(_ context.Context, leftPath, rightPath string, left, right *loadedDB) (*Result, error) {
	result := &Result{Left: leftPath, Right: rightPath}
	if !reflect.DeepEqual(left.routes, right.routes) {
		return result, apperr.New(apperr.CodeDiff, "", -1, -1, "route_order",
			fmt.Sprint(left.routes), fmt.Sprint(right.routes), nil)
	}
	result.RoutesCompared = len(left.routes)
	for _, routeID := range left.routes {
		indices := frameIndicesByRoute(left.frames, routeID)
		rightIndices := frameIndicesByRoute(right.frames, routeID)
		if !reflect.DeepEqual(indices, rightIndices) {
			frame, segment := firstMissing(routeID, indices, rightIndices, left.frames, right.frames)
			return result, apperr.New(apperr.CodeDiff, routeID, segment, frame, "presence",
				"matching complete frame sequence", "frame exists in only one snapshot", nil)
		}
		for _, frameIndex := range indices {
			key := snapshot.FrameKey{RouteID: routeID, Frame: frameIndex}
			lf, rf := left.frames[key], right.frames[key]
			if !bytes.Equal(lf.Pixels, rf.Pixels) {
				field := firstByteDifference(lf.Pixels, rf.Pixels)
				return result, apperr.New(apperr.CodeDiff, routeID, lf.Segment, frameIndex, field,
					"identical SDR pixels", "pixel bytes differ", nil)
			}
			if !bytes.Equal(lf.ProofJSON, rf.ProofJSON) {
				field := firstProofDifference(lf.ProofJSON, rf.ProofJSON)
				return result, apperr.New(apperr.CodeDiff, routeID, lf.Segment, frameIndex, field,
					"byte-identical proof record", "proof records differ", nil)
			}
			result.FramesCompared++
		}
	}
	result.Equal = true
	return result, nil
}

func frameIndicesByRoute(frames map[snapshot.FrameKey]snapshot.StoredFrame, routeID string) []int {
	var out []int
	for key := range frames {
		if key.RouteID == routeID {
			out = append(out, key.Frame)
		}
	}
	sort.Ints(out)
	return out
}

func firstMissing(routeID string, left, right []int, lf, rf map[snapshot.FrameKey]snapshot.StoredFrame) (int, int) {
	lset := make(map[int]bool, len(left))
	rset := make(map[int]bool, len(right))
	for _, i := range left {
		lset[i] = true
	}
	for _, i := range right {
		rset[i] = true
	}
	all := map[int]bool{}
	for _, i := range left {
		all[i] = true
	}
	for _, i := range right {
		all[i] = true
	}
	for _, i := range sortedSet(all) {
		if !lset[i] || !rset[i] {
			if lset[i] {
				return i, lf[snapshot.FrameKey{RouteID: routeID, Frame: i}].Segment
			}
			f := rf[snapshot.FrameKey{RouteID: routeID, Frame: i}]
			return i, f.Segment
		}
	}
	return -1, -1
}

func sortedSet(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func firstByteDifference(a, b []byte) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("pixels[%d]", i)
		}
	}
	return fmt.Sprintf("pixels.length(%d,%d)", len(a), len(b))
}

func firstProofDifference(a, b []byte) string {
	var av, bv interface{}
	if err := json.Unmarshal(a, &av); err != nil {
		return "proof_json.parse_left"
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return "proof_json.parse_right"
	}
	path, ok := firstValueDifference("proof", av, bv)
	if !ok {
		return "proof_json.bytes"
	}
	return path
}

func firstValueDifference(path string, a, b interface{}) (string, bool) {
	switch av := a.(type) {
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok {
			return path, true
		}
		keys := make(map[string]bool)
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		ordered := make([]string, 0, len(keys))
		for k := range keys {
			ordered = append(ordered, k)
		}
		sort.Strings(ordered)
		for _, k := range ordered {
			x, xok := av[k]
			y, yok := bv[k]
			if !xok || !yok {
				return path + "." + k, true
			}
			if p, diff := firstValueDifference(path+"."+k, x, y); diff {
				return p, true
			}
		}
		return "", false
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return path + ".length", true
		}
		for i := range av {
			if p, diff := firstValueDifference(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i]); diff {
				return p, true
			}
		}
		return "", false
	default:
		if !reflect.DeepEqual(a, b) {
			return path, true
		}
		return "", false
	}
}
