// Package verify is an independent checker. It deliberately does not import
// internal/render; it reconstructs the expected accumulation and display image
// from persisted scene/config/tile data.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
	"spectralforge/internal/store"
)

type Mismatch struct {
	Tile     int     `json:"tile"`
	X        int     `json:"x,omitempty"`
	Y        int     `json:"y,omitempty"`
	Field    string  `json:"field"`
	Expected float64 `json:"expected,omitempty"`
	Actual   float64 `json:"actual,omitempty"`
	Detail   string  `json:"detail,omitempty"`
}

type Report struct {
	OK              bool            `json:"ok"`
	TilesChecked    int             `json:"tiles_checked"`
	PixelsChecked   int             `json:"pixels_checked"`
	FirstMismatch   *Mismatch       `json:"first_mismatch,omitempty"`
	CommitSequence  []int           `json:"commit_sequence"`
	CommitOrderOK   bool            `json:"commit_order_ok"`
	WaitStats       store.WaitStats `json:"wait_stats"`
	MainSeconds     float64         `json:"main_render_seconds"`
	VerifierSeconds float64         `json:"verifier_seconds"`
	SpeedRatio      float64         `json:"main_to_verifier_ratio"`
}

type diskFrame struct {
	Width           int       `json:"width"`
	Height          int       `json:"height"`
	ContentHash     string    `json:"content_hash"`
	SamplingVersion string    `json:"sampling_version"`
	ParamVersion    string    `json:"param_version"`
	LinearRGB       []float64 `json:"linear_rgb"`
	DisplayRGB      []uint8   `json:"display_rgb"`
	LinearDigest    string    `json:"linear_digest"`
	DisplayDigest   string    `json:"display_digest"`
	Blocks          []struct {
		Tile   int    `json:"tile"`
		Digest string `json:"digest"`
	} `json:"blocks"`
}

type xyz struct{ x, y, z float64 }

func (a xyz) add(b xyz) xyz     { return xyz{a.x + b.x, a.y + b.y, a.z + b.z} }
func (a xyz) mul(v float64) xyz { return xyz{a.x * v, a.y * v, a.z * v} }

type samplePoint struct {
	wavelength float64
	weight     float64
}

type lightKernel struct {
	direction [3]float64
	white     xyz
	basis     []xyz
}

type kernel struct {
	points       []samplePoint
	lights       []lightKernel
	average      lightKernel
	wavelengths  []float64
	basisValues  [][]float64
	lightValues  [][]float64
	avgIntensity []float64
	whitePoint   [3]float64
	gamut        domain.Gamut
}

type vertexMaterial struct {
	albedo    [3]float64
	roughness float64
	metallic  float64
	normal    [3]float64
}

type material struct {
	vertices []vertexMaterial
	coeff    []float64
}

func Run(bundle *domain.SampleBundle, checkpointDir, outputDir string) (*Report, error) {
	start := time.Now()
	report := &Report{OK: true}
	manifest, err := store.ReadManifest(checkpointDir)
	if err != nil {
		return nil, err
	}
	if manifest.ContentHash != bundle.ContentHash {
		return nil, errs.Version(bundle.ContentHash, manifest.ContentHash)
	}
	stats, err := store.ReadStats(checkpointDir)
	if err != nil {
		return nil, err
	}
	report.WaitStats = stats.WaitStats
	report.MainSeconds = stats.RenderSeconds
	tiles, err := store.ReadTiles(checkpointDir, manifest)
	if err != nil {
		return nil, err
	}
	k, err := buildKernel(bundle.Spectral, manifest.Config)
	if err != nil {
		return nil, err
	}
	materials, err := buildMaterials(bundle.Scene, manifest.Config)
	if err != nil {
		return nil, err
	}
	recomputed := make([]float64, manifest.Width*manifest.Height*3)
	for i := range tiles {
		tile := &tiles[i]
		report.TilesChecked++
		if err := checkTile(bundle.Scene, k, materials, manifest, tile, recomputed, report); err != nil {
			return nil, err
		}
		report.PixelsChecked += tile.TileW * tile.TileH
	}

	framePath := filepath.Join(outputDir, "frame.json")
	frameData, err := os.ReadFile(framePath)
	if err != nil {
		return nil, errs.Decode("frame.json", "cannot read: %v", err)
	}
	var frame diskFrame
	if err := json.Unmarshal(frameData, &frame); err != nil {
		return nil, errs.Decode("frame.json", "invalid JSON: %v", err)
	}
	checkFrame(manifest, &frame, recomputed, tiles, report)

	commits, err := store.ReadCommits(checkpointDir)
	if err != nil {
		return nil, err
	}
	report.CommitSequence = make([]int, len(commits))
	for i, commit := range commits {
		report.CommitSequence[i] = commit.Tile
	}
	report.CommitOrderOK = true
	if len(commits) != manifest.TileCount {
		report.CommitOrderOK = false
		setMismatch(report, Mismatch{Tile: len(commits), Field: "commit_sequence", Detail: "commit count differs from tile count"})
	} else {
		for i, commit := range commits {
			if commit.Tile != i || commit.Seq != i {
				report.CommitOrderOK = false
				setMismatch(report, Mismatch{Tile: i, Field: "commit_sequence", Expected: float64(i), Actual: float64(commit.Tile), Detail: "tile commit sequence differs from canonical order"})
				break
			}
		}
	}
	report.VerifierSeconds = time.Since(start).Seconds()
	if report.VerifierSeconds > 0 {
		report.SpeedRatio = report.MainSeconds / report.VerifierSeconds
	}
	if outputDir != "" {
		_ = store.WriteJSONAtomic(filepath.Join(outputDir, "verification.json"), report)
	}
	return report, nil
}

func checkTile(scene *domain.Scene, k *kernel, materials map[string]*material, manifest store.Manifest, tile *store.TileSnapshot, target []float64, report *Report) error {
	recomputed := make([]float64, tile.TileW*tile.TileH*3)
	objectIDs := make([]uint32, tile.TileW*tile.TileH)
	peaks := make([]float64, tile.TileW*tile.TileH)
	meshIDs := make(map[string]uint32, len(scene.Meshes))
	for i, mesh := range scene.Meshes {
		meshIDs[mesh.ID] = uint32(i + 1)
	}
	for row := 0; row < tile.TileH; row++ {
		for col := 0; col < tile.TileW; col++ {
			x := tile.TileX + col
			y := tile.TileY + row
			pixel := row*tile.TileW + col
			hit, ok := trace(scene, float64(x)+0.5, float64(y)+0.5)
			var rgb [3]float64
			var peak float64
			var objectID uint32
			if !ok {
				rgb = scene.Background
			} else {
				mat := materials[hit.objectID]
				if mat == nil {
					return fmt.Errorf("verifier: no material for %s", hit.objectID)
				}
				attrs := interpolate(mat, hit.indices, hit.bary)
				value, p := shade(attrs, mat, k, manifest.Config)
				rgb = toLinearRGB(value, k)
				peak = p
				objectID = meshIDs[hit.objectID]
			}
			if manifest.Config.InjectedGamut != nil &&
				manifest.Config.InjectedGamut.X == x && manifest.Config.InjectedGamut.Y == y {
				switch manifest.Config.InjectedGamut.Channel {
				case "r":
					rgb[0] = manifest.Config.InjectedGamut.Value
				case "g":
					rgb[1] = manifest.Config.InjectedGamut.Value
				case "b":
					rgb[2] = manifest.Config.InjectedGamut.Value
				}
			}
			for c := 0; c < 3; c++ {
				if rgb[c] < 0 && rgb[c] > -1e-7 {
					rgb[c] = 0
				}
				recomputed[pixel*3+c] = rgb[c]
				if diff := math.Abs(tile.LinearRGB[pixel*3+c] - rgb[c]); diff > manifest.Config.ColorTolerance {
					field := "linear_rgb." + channelName(c)
					setMismatch(report, Mismatch{
						Tile: tile.TileIndex, X: x, Y: y, Field: field,
						Expected: rgb[c], Actual: tile.LinearRGB[pixel*3+c],
						Detail: fmt.Sprintf("absolute difference %.9g exceeds tolerance %.9g", diff, manifest.Config.ColorTolerance),
					})
					return nil
				}
			}
			objectIDs[pixel] = objectID
			peaks[pixel] = peak
			if tile.ObjectIDs[pixel] != objectID {
				setMismatch(report, Mismatch{
					Tile: tile.TileIndex, X: x, Y: y, Field: "object_id",
					Expected: float64(objectID), Actual: float64(tile.ObjectIDs[pixel]),
				})
				return nil
			}
			if diff := math.Abs(tile.PeakRadiance[pixel] - peak); diff > math.Max(manifest.Config.ColorTolerance, 1e-9)*math.Max(1, peak) {
				setMismatch(report, Mismatch{
					Tile: tile.TileIndex, X: x, Y: y, Field: "peak_radiance",
					Expected: peak, Actual: tile.PeakRadiance[pixel],
					Detail: fmt.Sprintf("absolute difference %.9g", diff),
				})
				return nil
			}
			dst := (y*manifest.Width + x) * 3
			copy(target[dst:dst+3], recomputed[pixel*3:pixel*3+3])
		}
	}
	if digest := store.TileDigest(tile.TileW, tile.TileH, recomputed); digest != tile.Digest {
		setMismatch(report, Mismatch{Tile: tile.TileIndex, Field: "tile_digest", Detail: "persisted block digest differs from recomputed pixels"})
	}
	return nil
}

func checkFrame(manifest store.Manifest, frame *diskFrame, linear []float64, tiles []store.TileSnapshot, report *Report) {
	if frame.Width != manifest.Width || frame.Height != manifest.Height {
		setMismatch(report, Mismatch{Tile: 0, Field: "frame_dimensions", Expected: float64(manifest.Width), Actual: float64(frame.Width)})
		return
	}
	if frame.ContentHash != manifest.ContentHash || frame.SamplingVersion != manifest.SamplingVersion || frame.ParamVersion != manifest.ParamVersion {
		setMismatch(report, Mismatch{Tile: 0, Field: "frame_version", Detail: "frame version hashes do not match checkpoint manifest"})
		return
	}
	if frame.LinearDigest != store.TileDigest(frame.Width, frame.Height, frame.LinearRGB) {
		setMismatch(report, Mismatch{Tile: 0, Field: "frame_linear_digest", Detail: "frame linear digest is inconsistent"})
	}
	for i := range frame.LinearRGB {
		if diff := math.Abs(frame.LinearRGB[i] - linear[i]); diff > manifest.Config.ColorTolerance {
			pix := i / 3
			setMismatch(report, Mismatch{Tile: tileIndexForPixel(pix, manifest), X: pix % frame.Width, Y: pix / frame.Width, Field: "frame_linear_rgb." + channelName(i%3), Expected: linear[i], Actual: frame.LinearRGB[i], Detail: fmt.Sprintf("absolute difference %.9g", diff)})
			return
		}
	}
	if len(frame.DisplayRGB) != frame.Width*frame.Height*3 {
		setMismatch(report, Mismatch{Tile: 0, Field: "display_rgb", Detail: "display buffer length mismatch"})
		return
	}
	display := encodeDisplay(linear, manifest.Config)
	for i := range display {
		if display[i] != frame.DisplayRGB[i] {
			pix := i / 3
			cols := (manifest.Width + manifest.TileSize - 1) / manifest.TileSize
			tile := (pix/frame.Width/manifest.TileSize)*cols + (pix%frame.Width)/manifest.TileSize
			setMismatch(report, Mismatch{Tile: tile, X: pix % frame.Width, Y: pix / frame.Width, Field: "display_rgb." + channelName(i%3), Expected: float64(display[i]), Actual: float64(frame.DisplayRGB[i]), Detail: "display byte differs"})
			return
		}
	}
	if len(frame.Blocks) != manifest.TileCount {
		setMismatch(report, Mismatch{Tile: 0, Field: "block_summary_count", Expected: float64(manifest.TileCount), Actual: float64(len(frame.Blocks))})
		return
	}
	for i := range frame.Blocks {
		if frame.Blocks[i].Tile != i {
			setMismatch(report, Mismatch{Tile: i, Field: "block_order", Expected: float64(i), Actual: float64(frame.Blocks[i].Tile)})
			return
		}
		if frame.Blocks[i].Digest != tiles[i].Digest {
			setMismatch(report, Mismatch{Tile: i, Field: "block_summary_digest", Detail: "frame block summary digest differs from persisted tile digest"})
			return
		}
	}
	if frame.DisplayDigest != digestBytes(display) {
		setMismatch(report, Mismatch{Tile: 0, Field: "frame_display_digest", Detail: "frame display digest is inconsistent"})
	}
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func setMismatch(report *Report, m Mismatch) {
	report.OK = false
	if report.FirstMismatch == nil {
		copy := m
		report.FirstMismatch = &copy
	}
}

func channelName(index int) string {
	return [...]string{"r", "g", "b"}[index]
}

func tileIndexForPixel(pixel int, manifest store.Manifest) int {
	cols := (manifest.Width + manifest.TileSize - 1) / manifest.TileSize
	return (pixel/manifest.Width/manifest.TileSize)*cols + (pixel%manifest.Width)/manifest.TileSize
}

func buildKernel(s *domain.SpectralData, cfg domain.RenderConfig) (*kernel, error) {
	lo, hi := s.Wavelengths[0], s.Wavelengths[len(s.Wavelengths)-1]
	points := make([]samplePoint, cfg.WavelengthSteps)
	switch cfg.Integration {
	case "trapezoid":
		d := (hi - lo) / float64(cfg.WavelengthSteps-1)
		for i := range points {
			points[i] = samplePoint{wavelength: lo + float64(i)*d, weight: d}
			if i == 0 || i == len(points)-1 {
				points[i].weight = d / 2
			}
		}
	case "midpoint":
		d := (hi - lo) / float64(cfg.WavelengthSteps)
		for i := range points {
			points[i] = samplePoint{wavelength: lo + (float64(i)+0.5)*d, weight: d}
		}
	default:
		return nil, errs.Para("render_config.integration", `["trapezoid","midpoint"]`, "unsupported integration")
	}
	k := &kernel{
		points:      points,
		wavelengths: append([]float64(nil), s.Wavelengths...),
		whitePoint:  s.WhitePoint,
		gamut:       s.Gamut,
	}
	k.basisValues = make([][]float64, len(s.Basis))
	for i := range s.Basis {
		k.basisValues[i] = append([]float64(nil), s.Basis[i].Values...)
	}
	for i := range s.Lights {
		lk := makeLightKernel(s, s.Lights[i].Direction, s.Lights[i].Intensity, points)
		k.lights = append(k.lights, lk)
		k.lightValues = append(k.lightValues, append([]float64(nil), s.Lights[i].Intensity...))
	}
	avg := make([]float64, len(s.Wavelengths))
	for i := range avg {
		for j := range s.Lights {
			avg[i] += s.Lights[j].Intensity[i]
		}
		avg[i] /= float64(len(s.Lights))
	}
	k.avgIntensity = avg
	k.average = makeLightKernel(s, [3]float64{0, 0, 1}, avg, points)
	return k, nil
}

func makeLightKernel(s *domain.SpectralData, direction [3]float64, intensity []float64, points []samplePoint) lightKernel {
	lk := lightKernel{direction: direction, basis: make([]xyz, len(s.Basis))}
	for _, point := range points {
		li := interp(s.Wavelengths, intensity, point.wavelength)
		mx := interp(s.Wavelengths, s.CMF.X, point.wavelength)
		my := interp(s.Wavelengths, s.CMF.Y, point.wavelength)
		mz := interp(s.Wavelengths, s.CMF.Z, point.wavelength)
		scale := point.weight * li
		lk.white.x += scale * mx
		lk.white.y += scale * my
		lk.white.z += scale * mz
		for b := range s.Basis {
			bv := interp(s.Wavelengths, s.Basis[b].Values, point.wavelength)
			term := scale * bv
			lk.basis[b].x += term * mx
			lk.basis[b].y += term * my
			lk.basis[b].z += term * mz
		}
	}
	return lk
}

func buildMaterials(scene *domain.Scene, cfg domain.RenderConfig) (map[string]*material, error) {
	out := make(map[string]*material, len(scene.Objects))
	for _, obj := range scene.Objects {
		var mesh *domain.Mesh
		for i := range scene.Meshes {
			if scene.Meshes[i].ID == obj.MeshID {
				mesh = &scene.Meshes[i]
				break
			}
		}
		if mesh == nil {
			return nil, errs.Schema("scene.objects", "missing mesh %s", obj.MeshID)
		}
		mat := &material{vertices: make([]vertexMaterial, len(mesh.Vertices)), coeff: make([]float64, len(obj.Coefficients))}
		for i, v := range mesh.Vertices {
			mat.vertices[i].normal = v.Normal
			for c := 0; c < 3; c++ {
				mat.vertices[i].albedo[c] = qUnit(v.Albedo[c], cfg.Quantization.AlbedoBits)
			}
			mat.vertices[i].roughness = qUnit(v.Roughness, cfg.Quantization.RoughnessBits)
			mat.vertices[i].metallic = qUnit(v.Metallic, cfg.Quantization.MetallicBits)
		}
		declared := coeffScale(obj.BasisBits, obj.BasisSigned)
		target := coeffScale(cfg.Quantization.BasisBits, obj.BasisSigned)
		for i, coeff := range obj.Coefficients {
			before := float64(coeff) / float64(declared)
			mat.coeff[i] = math.Round(before*float64(target)) / float64(target)
		}
		out[obj.MeshID] = mat
	}
	return out, nil
}

func qUnit(v float64, bits int) float64 {
	levels := float64(int64(1) << bits)
	return math.Max(0, math.Min(1, math.Round(v*levels)/levels))
}

func coeffScale(bits int, signed bool) int64 {
	if signed {
		return int64(1) << (bits - 1)
	}
	return int64(1) << bits
}

type hit struct {
	objectID string
	indices  [3]int
	bary     [3]float64
}

func trace(scene *domain.Scene, x, y float64) (hit, bool) {
	bestDepth := math.Inf(1)
	var best hit
	found := false
	for mi := range scene.Meshes {
		mesh := &scene.Meshes[mi]
		for ti := 0; ti+2 < len(mesh.Indices); ti += 3 {
			indices := [3]int{mesh.Indices[ti], mesh.Indices[ti+1], mesh.Indices[ti+2]}
			v0 := mesh.Vertices[indices[0]].Position
			v1 := mesh.Vertices[indices[1]].Position
			v2 := mesh.Vertices[indices[2]].Position
			denom := (v1[1]-v2[1])*(v0[0]-v2[0]) + (v2[0]-v1[0])*(v0[1]-v2[1])
			if math.Abs(denom) < 1e-12 {
				continue
			}
			w0 := ((v1[1]-v2[1])*(x-v2[0]) + (v2[0]-v1[0])*(y-v2[1])) / denom
			w1 := ((v2[1]-v0[1])*(x-v2[0]) + (v0[0]-v2[0])*(y-v2[1])) / denom
			w2 := 1 - w0 - w1
			if w0 < -1e-9 || w1 < -1e-9 || w2 < -1e-9 {
				continue
			}
			depth := w0*v0[2] + w1*v1[2] + w2*v2[2]
			if depth < bestDepth {
				bestDepth = depth
				best = hit{objectID: mesh.ID, indices: indices, bary: [3]float64{w0, w1, w2}}
				found = true
			}
		}
	}
	return best, found
}

type attrs struct {
	albedo    [3]float64
	roughness float64
	metallic  float64
	normal    [3]float64
}

func interpolate(mat *material, indices [3]int, bary [3]float64) attrs {
	var a attrs
	for i := 0; i < 3; i++ {
		v := mat.vertices[indices[i]]
		for c := 0; c < 3; c++ {
			a.albedo[c] += bary[i] * v.albedo[c]
			a.normal[c] += bary[i] * v.normal[c]
		}
		a.roughness += bary[i] * v.roughness
		a.metallic += bary[i] * v.metallic
	}
	return a
}

func shade(a attrs, mat *material, k *kernel, cfg domain.RenderConfig) (xyz, float64) {
	tint := 0.2126*a.albedo[0] + 0.7152*a.albedo[1] + 0.0722*a.albedo[2]
	metal := clamp(a.metallic)
	rough := math.Max(0.02, clamp(a.roughness))
	normal := norm3(a.normal)
	view := [3]float64{0, 0, 1}
	diffuseWeight := (1 - metal) * tint
	f0 := 0.04*(1-metal) + tint*metal
	shininess := math.Max(2, 2/(rough*rough*rough*rough+1e-4)-2)
	var out xyz
	for i := range k.lights {
		l := &k.lights[i]
		dir := norm3(l.direction)
		ndotl := math.Max(0, dot(normal, dir))
		if ndotl == 0 {
			continue
		}
		h := norm3([3]float64{dir[0] + view[0], dir[1] + view[1], dir[2] + view[2]})
		spec := f0 * math.Pow(math.Max(0, dot(normal, h)), shininess)
		contrib := basisXYZ(l.basis, mat.coeff).mul(diffuseWeight).add(l.white.mul(spec))
		out = out.add(contrib.mul(ndotl))
	}
	avgRefl := basisXYZ(k.average.basis, mat.coeff)
	power := 1.0
	for b := 0; b < cfg.IndirectBounces; b++ {
		out = out.add(avgRefl.mul(0.12 * tint * (1 - metal) * power))
		power *= 0.35 * tint * (1 - metal)
	}
	peak := 0.0
	for _, p := range k.points {
		rho := clamp(reflectance(mat, k, p.wavelength))
		radiance := 0.0
		for i := range k.lights {
			l := &k.lights[i]
			dir := norm3(l.direction)
			ndotl := math.Max(0, dot(normal, dir))
			if ndotl == 0 {
				continue
			}
			h := norm3([3]float64{dir[0] + view[0], dir[1] + view[1], dir[2] + view[2]})
			spec := f0 * math.Pow(math.Max(0, dot(normal, h)), shininess)
			intensity := interp(k.wavelengths, k.lightValues[i], p.wavelength)
			radiance += ndotl * intensity * (diffuseWeight*rho + spec)
		}
		power = 1
		for b := 0; b < cfg.IndirectBounces; b++ {
			radiance += interp(k.wavelengths, k.avgIntensity, p.wavelength) * rho * 0.12 * tint * (1 - metal) * power
			power *= 0.35 * tint * (1 - metal)
		}
		if radiance > peak {
			peak = radiance
		}
	}
	return out, peak
}

func reflectance(mat *material, k *kernel, wavelength float64) float64 {
	value := 0.0
	for i := range mat.coeff {
		value += mat.coeff[i] * interp(k.wavelengths, k.basisValues[i], wavelength)
	}
	return value
}

func toLinearRGB(v xyz, k *kernel) [3]float64 {
	n := [3]float64{v.x / k.whitePoint[0], v.y / k.whitePoint[1], v.z / k.whitePoint[2]}
	var rgb [3]float64
	for r := 0; r < 3; r++ {
		rgb[r] = k.gamut.XYZToRGB[r][0]*n[0] + k.gamut.XYZToRGB[r][1]*n[1] + k.gamut.XYZToRGB[r][2]*n[2]
	}
	return rgb
}

func encodeDisplay(linear []float64, cfg domain.RenderConfig) []uint8 {
	out := make([]uint8, len(linear))
	scale := math.Pow(2, cfg.Exposure)
	for i := 0; i < len(linear); i += 3 {
		for c := 0; c < 3; c++ {
			v := linear[i+c] * scale
			switch cfg.ToneCurve {
			case "reinhard":
				v = v / (1 + v)
			case "aces":
				v = (v * (2.51*v + 0.03)) / (v*(2.43*v+0.59) + 0.14)
			}
			v = math.Max(0, math.Min(1, v))
			v = math.Pow(v, 1/cfg.Gamma)
			out[i+c] = uint8(math.Round(v * 255))
		}
	}
	return out
}

func basisXYZ(basis []xyz, coeff []float64) xyz {
	var out xyz
	n := len(basis)
	if len(coeff) < n {
		n = len(coeff)
	}
	for i := 0; i < n; i++ {
		out.x += basis[i].x * coeff[i]
		out.y += basis[i].y * coeff[i]
		out.z += basis[i].z * coeff[i]
	}
	return out
}

func interp(xs, ys []float64, x float64) float64 {
	if x <= xs[0] {
		return ys[0]
	}
	if x >= xs[len(xs)-1] {
		return ys[len(ys)-1]
	}
	lo, hi := 0, len(xs)-1
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if xs[mid] <= x {
			lo = mid
		} else {
			hi = mid
		}
	}
	t := (x - xs[lo]) / (xs[hi] - xs[lo])
	return ys[lo] + t*(ys[hi]-ys[lo])
}

func norm3(v [3]float64) [3]float64 {
	n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if n < 1e-15 {
		return [3]float64{0, 0, 1}
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}
}

func dot(a, b [3]float64) float64 {
	return a[0]*b[0] + a[1]*b[1] + a[2]*b[2]
}

func clamp(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}
