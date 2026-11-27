package grid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// SemanticVersions keeps independent geometric semantic components.
type SemanticVersions struct {
	Endpoint    int `json:"endpoint"`
	Normal      int `json:"normal"`
	Restitution int `json:"restitution"`
	Gravity     int `json:"gravity"`
	TimeStep    int `json:"timeStep"`
	Structure   int `json:"structure"`
}

func DefaultSemanticVersions() SemanticVersions {
	return SemanticVersions{Endpoint: 1, Normal: 1, Restitution: 1, Gravity: 1, TimeStep: 1, Structure: 1}
}

// CurrentVersions is retained for simple zero-configuration callers. Service
// code should keep its own mutable SemanticVersions value.
var CurrentVersions = DefaultSemanticVersions()

func CacheKeyForTile(contentHash string, versions SemanticVersions, cameraVersion int) string {
	payload := fmt.Sprintf("tile-content=%s|structure=%d|camera=%d|algorithm=%s",
		contentHash, versions.Structure, cameraVersion, AlgorithmVersion)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

type InvalidationItem struct {
	Tile           TileID           `json:"tile"`
	OldVersions    SemanticVersions `json:"oldVersions"`
	Reasons        []string         `json:"reasons"`
	ObstacleIDs    []string         `json:"obstacleIds,omitempty"`
	Downstream     bool             `json:"downstream"`
	OldContentHash string           `json:"oldContentHash,omitempty"`
}

type InvalidationPlan struct {
	ChangedComponent string             `json:"changedComponent"`
	OldVersions      SemanticVersions   `json:"oldVersions"`
	NewVersions      SemanticVersions   `json:"newVersions"`
	Items            []InvalidationItem `json:"items"`
	Tiles            []TileID           `json:"tiles"`
}

// EditObstacle applies mutate, bumps the requested semantic component, and
// computes exact direct + downstream tile invalidation.
func EditObstacle(level *Level, cam Camera, versions *SemanticVersions, obstacleID, component string, mutate func(*Obstacle)) (*InvalidationPlan, error) {
	oldVersions := *versions
	before, err := ProjectScene(level, cam)
	if err != nil {
		return nil, err
	}
	oldReasons := reasonsForObstacle(before, obstacleID, component)

	o := level.ObstacleByID(obstacleID)
	if o == nil {
		return nil, decodeError(obstacleID, "obstacles", "unknown obstacle id")
	}
	if mutate != nil {
		mutate(o)
	}
	switch component {
	case "endpoint":
		versions.Endpoint++
		o.EndpointVer++
	case "normal":
		versions.Normal++
		o.NormalVer++
	case "restitution":
		versions.Restitution++
		o.RestitutionVer++
	default:
		return nil, decodeError(obstacleID, "component", "unknown semantic component %q", component)
	}

	after, err := ProjectScene(level, cam)
	if err != nil {
		return nil, err
	}
	newReasons := reasonsForObstacle(after, obstacleID, component)
	merged := mergeReasonSets(oldReasons, newReasons)
	plan := &InvalidationPlan{ChangedComponent: component, OldVersions: oldVersions, NewVersions: *versions}

	ids := make([]TileID, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i].Row < ids[j].Row || (ids[i].Row == ids[j].Row && ids[i].Col < ids[j].Col)
	})
	oldHash := map[string]string{}
	for _, t := range before.Tiles {
		oldHash[t.ID.String()] = t.ContentHash
	}
	for _, id := range ids {
		reasonSet := merged[id]
		reasons := make([]string, 0, len(reasonSet))
		obstacleSet := map[string]bool{}
		downstream := false
		for r := range reasonSet {
			reasons = append(reasons, r)
			if strings.HasPrefix(r, "downstream-") {
				downstream = true
			}
			for _, oid := range reasonObstacleIDs(r) {
				obstacleSet[oid] = true
			}
		}
		sort.Strings(reasons)
		obs := sortedStringSet(obstacleSet)
		item := InvalidationItem{
			Tile: id, OldVersions: oldVersions, Reasons: reasons, ObstacleIDs: obs,
			Downstream: downstream, OldContentHash: oldHash[id.String()],
		}
		plan.Items = append(plan.Items, item)
		plan.Tiles = append(plan.Tiles, id)
	}
	return plan, nil
}

func reasonObstacleIDs(reason string) []string {
	// Reasons use stable forms such as direct-endpoint:OB01 and
	// downstream-bounce:REQ-01:OB01.
	parts := splitColon(reason)
	if len(parts) == 0 {
		return nil
	}
	var out []string
	for _, p := range parts[1:] {
		if len(p) == 4 && p[:2] == "OB" {
			out = append(out, p)
		}
	}
	return out
}

func splitColon(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func mergeReasonSets(a, b map[TileID]map[string]bool) map[TileID]map[string]bool {
	out := map[TileID]map[string]bool{}
	for _, src := range []map[TileID]map[string]bool{a, b} {
		for id, reasons := range src {
			if out[id] == nil {
				out[id] = map[string]bool{}
			}
			for r := range reasons {
				out[id][r] = true
			}
		}
	}
	return out
}

func reasonsForObstacle(scene *SceneTiles, obstacleID, component string) map[TileID]map[string]bool {
	out := map[TileID]map[string]bool{}
	add := func(id TileID, reason string) {
		if out[id] == nil {
			out[id] = map[string]bool{}
		}
		out[id][reason] = true
	}
	directReason := "direct-" + component + ":" + obstacleID
	for _, t := range scene.Tiles {
		for _, id := range t.ObstacleIDs {
			if id == obstacleID {
				add(t.ID, directReason)
			}
		}
	}
	for _, result := range scene.Results {
		for _, b := range result.Bounces {
			if b.ObstacleID != obstacleID {
				continue
			}
			reason := fmt.Sprintf("downstream-bounce:%s:%s", result.RequestID, obstacleID)
			for _, id := range TilesAlongPath(scene.Camera, result.Path, b.Point) {
				add(id, reason)
			}
		}
	}
	return out
}

// TilesAlongPath returns visible tiles from the first matching anchor onward.
func TilesAlongPath(cam Camera, path []Vec2, anchor Vec2) []TileID {
	start := 0
	for i, p := range path {
		if p == anchor {
			start = i
			break
		}
	}
	set := map[TileID]bool{}
	var last TileID
	hasLast := false
	addPoint := func(p Vec2) {
		if id, ok := cam.TileAt(p); ok {
			set[id] = true
		}
	}
	for i := start; i < len(path); i++ {
		addPoint(path[i])
		if i == start {
			continue
		}
		a := path[i-1]
		b := path[i]
		dx := b.X - a.X
		dy := b.Y - a.Y
		steps := int(max64(abs64(dx), abs64(dy)) / max64(1, cam.UnitsPerPixel*int64(cam.TilePixels)/2))
		if steps < 1 {
			steps = 1
		}
		if steps > 200 {
			steps = 200
		}
		for s := 1; s <= steps; s++ {
			p := Vec2{X: a.X + dx*int64(s)/int64(steps), Y: a.Y + dy*int64(s)/int64(steps)}
			addPoint(p)
		}
	}
	ids := make([]TileID, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i].Row < ids[j].Row || (ids[i].Row == ids[j].Row && ids[i].Col < ids[j].Col)
	})
	_ = last
	_ = hasLast
	return ids
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (c Camera) TileAt(p Vec2) (TileID, bool) {
	px := (p.X - c.Offset.X) / c.UnitsPerPixel
	py := (p.Y - c.Offset.Y) / c.UnitsPerPixel
	if px < 0 || py < 0 {
		return TileID{}, false
	}
	pixels := int64(c.TilePixels)
	col := int(px / pixels)
	row := int(py / pixels)
	if col < 0 || row < 0 || col >= c.Cols() || row >= c.Rows() {
		return TileID{Col: col, Row: row}, false
	}
	return TileID{Col: col, Row: row}, true
}

// InvalidatePhysicsConfig marks tiles containing trajectories after a gravity
// or time-step component edit.
func InvalidatePhysicsConfig(level *Level, cam Camera, versions *SemanticVersions, component string) (*InvalidationPlan, *SceneTiles, error) {
	old := *versions
	scene, err := ProjectScene(level, cam)
	if err != nil {
		return nil, nil, err
	}
	switch component {
	case "gravity":
		versions.Gravity++
	case "timeStep":
		versions.TimeStep++
	default:
		return nil, nil, decodeError("", "component", "unknown physics component %q", component)
	}
	plan := &InvalidationPlan{ChangedComponent: component, OldVersions: old, NewVersions: *versions}
	for _, t := range scene.Tiles {
		if len(t.RequestIDs) == 0 {
			continue
		}
		plan.Tiles = append(plan.Tiles, t.ID)
		plan.Items = append(plan.Items, InvalidationItem{
			Tile: t.ID, OldVersions: old, Reasons: []string{"physics-" + component + ":trajectory"},
		})
	}
	return plan, scene, nil
}
