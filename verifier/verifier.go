package verifier

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	"iconcut"

	_ "modernc.org/sqlite"
)

type Violation struct {
	TileIndex int    `json:"tile_index"`
	TX        int    `json:"tx"`
	TY        int    `json:"ty"`
	Stage     string `json:"stage"`
	Field     string `json:"field"`
	Message   string `json:"message"`
	Expected  string `json:"expected,omitempty"`
	Actual    string `json:"actual,omitempty"`
}

type Report struct {
	OK              bool        `json:"ok"`
	DBPath          string      `json:"db_path"`
	ImageID         string      `json:"image_id"`
	CommitsChecked  int         `json:"commits_checked"`
	TilesChecked    int         `json:"tiles_checked"`
	RegionsComputed int         `json:"regions_computed"`
	FirstViolation  *Violation  `json:"first_violation,omitempty"`
	Violations      []Violation `json:"violations,omitempty"`
}

type commitRow struct {
	seq              int64
	tileIndex        int
	tx, ty           int
	stage            string
	digest           string
	mappingVersion   string
	schemeVersion    string
	paramVersion     string
	contentHash      string
	algorithmVersion string
	summary          string
	artifact         []byte
}

func VerifyDB(ctx context.Context, dbPath string) (*Report, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rep := &Report{DBPath: dbPath, OK: true}

	var payload, truthMeta, imageIDMeta string
	_ = db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='snapshot_json'`).Scan(&payload)
	_ = db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='truth_fingerprint'`).Scan(&truthMeta)
	_ = db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='image_id'`).Scan(&imageIDMeta)
	if payload == "" {
		rep.OK = false
		rep.FirstViolation = &Violation{Stage: "snapshot", Field: "meta.snapshot_json", Message: "missing snapshot"}
		return rep, nil
	}
	var snap iconcut.Snapshot
	if err := json.Unmarshal([]byte(payload), &snap); err != nil {
		rep.OK = false
		rep.FirstViolation = &Violation{Stage: "snapshot", Field: "meta.snapshot_json", Message: err.Error()}
		return rep, nil
	}
	rep.ImageID = snap.ImageID
	if truthMeta != snap.TruthFingerprint {
		rep.add(Violation{Stage: "snapshot", Field: "meta.truth_fingerprint", Message: "truth fingerprint mismatch", Expected: snap.TruthFingerprint, Actual: truthMeta})
	}
	if imageIDMeta != snap.ImageID {
		rep.add(Violation{Stage: "snapshot", Field: "meta.image_id", Expected: snap.ImageID, Actual: imageIDMeta})
	}

	rows, err := db.QueryContext(ctx, `SELECT commit_seq,tile_index,tx,ty,stage,digest,mapping_version,scheme_version,param_version,content_hash,algorithm_version,summary_json,artifact
		FROM commits WHERE image_id=? ORDER BY commit_seq`, snap.ImageID)
	if err != nil {
		return nil, err
	}
	var commits []commitRow
	for rows.Next() {
		var c commitRow
		if err := rows.Scan(&c.seq, &c.tileIndex, &c.tx, &c.ty, &c.stage, &c.digest, &c.mappingVersion, &c.schemeVersion, &c.paramVersion, &c.contentHash, &c.algorithmVersion, &c.summary, &c.artifact); err != nil {
			rows.Close()
			return nil, err
		}
		commits = append(commits, c)
	}
	rows.Close()
	rep.CommitsChecked = len(commits)

	sourceRGB, err := loadAsset(ctx, db, snap.ImageID, "source.rgb")
	if err != nil {
		rep.add(Violation{Stage: "asset", Field: "assets.source.rgb", Message: err.Error()})
	}

	expectedSeq := expectedKeys(snap.Tiles)
	for i, c := range commits {
		sum := sha256.Sum256(c.artifact)
		if got := hex.EncodeToString(sum[:]); got != c.contentHash {
			rep.add(Violation{TileIndex: c.tileIndex, TX: c.tx, TY: c.ty, Stage: c.stage, Field: "content_hash", Message: "artifact hash mismatch", Expected: c.contentHash, Actual: got})
		}
		if c.algorithmVersion != snap.Versions.Algorithm || c.mappingVersion != snap.Versions.Mapping || c.schemeVersion != snap.Versions.Scheme || c.paramVersion != snap.Versions.Param {
			rep.add(Violation{TileIndex: c.tileIndex, TX: c.tx, TY: c.ty, Stage: c.stage, Field: "version", Message: "stage version does not match snapshot"})
		}
		if i < len(expectedSeq) {
			ek := expectedSeq[i]
			if ek.tile != c.tileIndex || ek.stage != c.stage {
				rep.add(Violation{TileIndex: c.tileIndex, TX: c.tx, TY: c.ty, Stage: c.stage, Field: "commit_seq", Message: "unexpected commit order", Expected: fmt.Sprintf("%d:%s", ek.tile, ek.stage), Actual: fmt.Sprintf("%d:%s", c.tileIndex, c.stage)})
			}
		}
	}

	arts := map[string]*iconcut.StageArtifact{}
	for _, c := range commits {
		a, err := iconcut.DecodeArtifact(c.artifact)
		if err != nil {
			rep.add(Violation{TileIndex: c.tileIndex, TX: c.tx, TY: c.ty, Stage: c.stage, Field: "artifact", Message: err.Error()})
			continue
		}
		arts[fmt.Sprintf("%d:%s", c.tileIndex, c.stage)] = a
		if c.stage == iconcut.StageMapping && len(sourceRGB) > 0 && snap.Manifest != nil {
			if v := verifyMapping(snap.Manifest, a, sourceRGB); v != nil {
				v.TileIndex = c.tileIndex
				v.TX = c.tx
				v.TY = c.ty
				rep.add(*v)
			}
		}
	}

	m := snap.Manifest
	if m == nil {
		rep.add(Violation{Stage: "snapshot", Field: "manifest", Message: "missing manifest"})
		rep.OK = len(rep.Violations) == 0
		return rep, nil
	}
	mask := iconcut.NewLabelMask(m.Corrected.W, m.Corrected.H)
	for _, t := range snap.Tiles {
		a := arts[fmt.Sprintf("%d:%s", t.Index, iconcut.StageClean)]
		if a == nil || len(a.Labels) != t.Core.W*t.Core.H {
			rep.add(Violation{TileIndex: t.Index, TX: t.TX, TY: t.TY, Stage: iconcut.StageClean, Field: "labels", Message: "missing clean tile artifact"})
			continue
		}
		for y := 0; y < t.Core.H; y++ {
			for x := 0; x < t.Core.W; x++ {
				mask.Set(t.Core.X+x, t.Core.Y+y, a.Labels[y*t.Core.W+x])
			}
		}
		rep.TilesChecked++
	}

	stats := recomputeStats(mask)
	rep.RegionsComputed = len(stats)
	for _, truth := range m.Regions {
		var got *regionStat
		for i := range stats {
			if stats[i].id == truth.ID {
				got = &stats[i]
				break
			}
		}
		if got == nil {
			tx, ty := seedTile(m, truth.Seed)
			rep.add(Violation{TileIndex: tx*m.Declared.TileWidth + 0, TX: tx, TY: ty, Stage: iconcut.StageStats, Field: "regions", Message: "missing region", Expected: fmt.Sprint(truth.ID)})
			continue
		}
		cx, cy := float64(got.centroidX)/float64(got.area), float64(got.centroidY)/float64(got.area)
		cd := math.Hypot(cx-truth.CentroidX, cy-truth.CentroidY)
		ar := math.Abs(float64(got.area-truth.Area)) / math.Max(1, float64(truth.Area))
		if cd > m.Declared.Tolerance.RegionCentroidPX || ar > m.Declared.Tolerance.RegionAreaRel || abs(got.holes-truth.Holes) > m.Declared.Tolerance.HoleCountDelta {
			tx, ty := seedTile(m, truth.Seed)
			rep.add(Violation{TX: tx, TY: ty, TileIndex: ty*(m.Corrected.W/m.Declared.TileWidth) + tx, Stage: iconcut.StageStats, Field: fmt.Sprintf("regions[%d]", truth.ID),
				Message: "recomputed region statistic outside tolerance", Expected: fmt.Sprintf("centroid<=%v area<=%v holes<=%d", m.Declared.Tolerance.RegionCentroidPX, m.Declared.Tolerance.RegionAreaRel, m.Declared.Tolerance.HoleCountDelta),
				Actual: fmt.Sprintf("centroid=%.4f area_rel=%.4f holes=%d", cd, ar, got.holes)})
		}
	}
	rep.OK = len(rep.Violations) == 0
	if !rep.OK && rep.FirstViolation == nil {
		rep.FirstViolation = &rep.Violations[0]
	}
	return rep, nil
}

func (r *Report) add(v Violation) {
	if r.FirstViolation == nil {
		r.FirstViolation = &v
	}
	r.Violations = append(r.Violations, v)
	r.OK = false
}

func loadAsset(ctx context.Context, db *sql.DB, imageID, name string) ([]byte, error) {
	var b []byte
	err := db.QueryRowContext(ctx, `SELECT content FROM assets WHERE image_id=? AND name=?`, imageID, name).Scan(&b)
	return b, err
}

type key struct {
	tile  int
	stage string
}

func expectedKeys(tiles []iconcut.Tile) []key {
	var out []key
	for _, stage := range iconcut.StageOrder {
		for _, t := range tiles {
			out = append(out, key{tile: t.Index, stage: stage})
		}
	}
	return out
}

type vMapper struct {
	h     [3][3]float64
	r     iconcut.RadialParams
	scale float64
	src   []byte
	w, hh int
}

func verifyMapping(m *iconcut.Manifest, a *iconcut.StageArtifact, src []byte) *Violation {
	g := m.Geometry
	vm := vMapper{h: g.Homography, r: g.Radial, scale: float64(g.CoordScale), src: src, w: m.Source.W, hh: m.Source.H}
	if g.CoordScale <= 0 {
		vm.scale = 16
	}
	for y := a.Core.Y; y < a.Core.MaxY(); y++ {
		for x := a.Core.X; x < a.Core.MaxX(); x++ {
			sx, sy := vm.mapQ(x, y)
			c := vm.bilinear(sx, sy)
			li := ((y-a.Core.Y)*a.Core.W + (x - a.Core.X)) * 3
			if li+2 >= len(a.RGB) {
				return &Violation{Stage: iconcut.StageMapping, Field: "rgb.length", Message: "artifact RGB is shorter than core"}
			}
			if c != [3]byte{a.RGB[li], a.RGB[li+1], a.RGB[li+2]} {
				return &Violation{Stage: iconcut.StageMapping, Field: "rgb", Message: "reverse-mapped pixel differs from independent verifier", Actual: fmt.Sprintf("at(%d,%d) got=%v artifact=%v", x, y, c, a.RGB[li:li+3])}
			}
		}
	}
	return nil
}

func (v *vMapper) mapQ(x, y int) (float64, float64) {
	p := iconcut.Point{X: float64(x), Y: float64(y)}
	w := v.h[2][0]*p.X + v.h[2][1]*p.Y + v.h[2][2]
	u := iconcut.Point{X: (v.h[0][0]*p.X + v.h[0][1]*p.Y + v.h[0][2]) / w, Y: (v.h[1][0]*p.X + v.h[1][1]*p.Y + v.h[1][2]) / w}
	dx := u.X - v.r.CenterX
	dy := u.Y - v.r.CenterY
	r2 := (dx*dx + dy*dy) / (v.r.Radius * v.r.Radius)
	f := 1 + v.r.K1*r2 + v.r.K2*r2*r2
	exactX := v.r.CenterX + dx*f
	exactY := v.r.CenterY + dy*f
	qx := math.Round(exactX * v.scale)
	qy := math.Round(exactY * v.scale)
	return qx / v.scale, qy / v.scale
}

func (v *vMapper) color(x, y int) [3]float64 {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= v.w {
		x = v.w - 1
	}
	if y >= v.hh {
		y = v.hh - 1
	}
	i := (y*v.w + x) * 3
	return [3]float64{float64(v.src[i]), float64(v.src[i+1]), float64(v.src[i+2])}
}

func (v *vMapper) bilinear(sx, sy float64) [3]byte {
	x0 := int(math.Floor(sx))
	y0 := int(math.Floor(sy))
	tx := sx - float64(x0)
	ty := sy - float64(y0)
	c00 := v.color(x0, y0)
	c10 := v.color(x0+1, y0)
	c01 := v.color(x0, y0+1)
	c11 := v.color(x0+1, y0+1)
	var out [3]byte
	for i := 0; i < 3; i++ {
		a := c00[i]*(1-tx) + c10[i]*tx
		b := c01[i]*(1-tx) + c11[i]*tx
		out[i] = byte(math.Round(clamp(a*(1-ty)+b*ty, 0, 255)))
	}
	return out
}

type regionStat struct {
	id                     int
	area                   int
	centroidX, centroidY   int
	minX, minY, maxX, maxY int
	holes                  int
}

func recomputeStats(mask *iconcut.LabelMask) []regionStat {
	byID := map[int]*regionStat{}
	for y := 0; y < mask.H; y++ {
		for x := 0; x < mask.W; x++ {
			id := int(mask.At(x, y))
			if id == 0 {
				continue
			}
			s := byID[id]
			if s == nil {
				s = &regionStat{id: id, minX: x, maxX: x, minY: y, maxY: y}
				byID[id] = s
			}
			s.area++
			s.centroidX += x
			s.centroidY += y
			s.minX = min(s.minX, x)
			s.maxX = max(s.maxX, x)
			s.minY = min(s.minY, y)
			s.maxY = max(s.maxY, y)
		}
	}
	out := make([]regionStat, 0, len(byID))
	for id, s := range byID {
		s.holes = countHoles(mask, uint8(id), s.minX, s.minY, s.maxX, s.maxY)
		out = append(out, *s)
	}
	return out
}

func countHoles(mask *iconcut.LabelMask, id uint8, x0, y0, x1, y1 int) int {
	seen := make([]bool, mask.W*mask.H)
	holes := 0
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			idx := y*mask.W + x
			if seen[idx] || mask.At(x, y) == id {
				continue
			}
			queue := []iconcut.IPoint{{X: x, Y: y}}
			seen[idx] = true
			enclosed := true
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				if p.X == x0 || p.Y == y0 || p.X == x1 || p.Y == y1 {
					enclosed = false
				}
				for _, d := range []iconcut.IPoint{{X: 1, Y: 0}, {X: -1, Y: 0}, {X: 0, Y: 1}, {X: 0, Y: -1}} {
					nx, ny := p.X+d.X, p.Y+d.Y
					if nx < x0 || ny < y0 || nx > x1 || ny > y1 {
						enclosed = false
						continue
					}
					nidx := ny*mask.W + nx
					if seen[nidx] || mask.At(nx, ny) == id {
						continue
					}
					seen[nidx] = true
					queue = append(queue, iconcut.IPoint{X: nx, Y: ny})
				}
			}
			if enclosed {
				holes++
			}
		}
	}
	return holes
}

func seedTile(m *iconcut.Manifest, p iconcut.IPoint) (int, int) {
	tx := p.X / m.Declared.TileWidth
	ty := p.Y / m.Declared.TileHeight
	nx := m.Corrected.W / m.Declared.TileWidth
	if tx >= nx {
		tx = nx - 1
	}
	return tx, ty
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
