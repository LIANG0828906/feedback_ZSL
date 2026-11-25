package textureconv

import (
	"fmt"
	"strconv"
	"strings"
)

type VerifyOptions struct {
	ExpectedParamsVersion    string
	ExpectedAlgorithmVersion string
}

type VerifyReport struct {
	ImageID          string         `json:"imageId"`
	PlanFingerprint  string         `json:"planFingerprint"`
	TruthFingerprint string         `json:"truthFingerprint"`
	StageSummaries   []ImageSummary `json:"stageSummaries"`
	SamplesChecked   int            `json:"samplesChecked"`
	MaxDeviation     uint16         `json:"maxDeviation"`
}

type expectedStageMeta struct {
	entry    int
	depth    BitDepth
	channels int
	alpha    int
}

// VerifyPersisted independently reconstructs all stage images from SQLite
// chunk rows, recomputes summaries, and checks truth samples. It deliberately
// does not call ConvertEntry or the scheduler.
func VerifyPersisted(bundle *SampleBundle, imageID string, store *Store, opts VerifyOptions) (*VerifyReport, error) {
	if bundle == nil || bundle.Plan == nil || bundle.Truth == nil {
		return nil, schemaErr(0, "bundle", "incomplete sample bundle")
	}
	rec, ok := bundle.Source(imageID)
	if !ok {
		return nil, schemaErr(0, "imageId", "unknown image %q", imageID)
	}
	it, ok := bundle.ImageTruth(imageID)
	if !ok {
		return nil, schemaErr(0, "truth", "missing truth for image %q", imageID)
	}
	if got := truthFingerprint(bundle.Truth); got != bundle.Truth.Fingerprint {
		return nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "fingerprint",
			Msg: fmt.Sprintf("truth fingerprint mismatch: stored=%s calculated=%s", bundle.Truth.Fingerprint, got)}
	}
	version, commits, err := store.ReadAll()
	if err != nil {
		return nil, err
	}
	if version.ImageID != imageID {
		return nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "imageId",
			Msg: fmt.Sprintf("snapshot image %q, expected %q", version.ImageID, imageID)}
	}
	if version.PlanFingerprint != bundle.Plan.Fingerprint() {
		return nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "planFingerprint",
			Msg: "snapshot plan fingerprint differs from plan file"}
	}
	if version.SourceHash != rec.SHA256 {
		return nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "sourceHash",
			Msg: "snapshot source hash differs from truth source fingerprint"}
	}
	if opts.ExpectedAlgorithmVersion != "" && version.AlgorithmVersion != opts.ExpectedAlgorithmVersion {
		return nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "algorithmVersion",
			Msg: fmt.Sprintf("snapshot algorithm %q, expected %q", version.AlgorithmVersion, opts.ExpectedAlgorithmVersion)}
	}
	if opts.ExpectedParamsVersion != "" && version.ParamsVersion != opts.ExpectedParamsVersion {
		return nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "paramsVersion",
			Msg: fmt.Sprintf("snapshot params %q, expected %q", version.ParamsVersion, opts.ExpectedParamsVersion)}
	}
	tileSize, err := parseGeometryTile(version.Geometry)
	if err != nil {
		return nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "geometry", Msg: err.Error()}
	}
	tiles, err := MakeTiles(rec.Width, rec.Height, tileSize)
	if err != nil {
		return nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "geometry", Msg: err.Error()}
	}
	rows := map[stageTileID]StageRecord{}
	for _, c := range commits {
		if c.Tile < 0 || c.Tile >= len(tiles) {
			return nil, &Error{Code: CodeDiff, Entry: 0, Tile: c.Tile, Field: "tile",
				Msg: "persisted tile is outside image geometry"}
		}
		for _, r := range c.Stages {
			rows[stageTileID{Entry: r.Entry, Tile: c.Tile}] = r
		}
	}
	metas := expectedStageMetas(bundle.Plan)
	report := &VerifyReport{
		ImageID: imageID, PlanFingerprint: version.PlanFingerprint,
		TruthFingerprint: bundle.Truth.Fingerprint,
	}
	var final *StageImage
	for _, meta := range metas {
		out := &StageImage{
			Width: rec.Width, Height: rec.Height, BitDepth: meta.depth,
			Channels: meta.channels, AlphaIndex: meta.alpha,
			Pixels: make([]uint16, rec.Width*rec.Height*meta.channels),
		}
		var dropped []DroppedChannel
		for _, t := range tiles {
			r, ok := rows[stageTileID{Entry: meta.entry, Tile: t.Index}]
			if !ok {
				return nil, &Error{Code: CodeDiff, Entry: meta.entry, Tile: t.Index,
					Field: "stageResult", Pixel: Point{X: t.X, Y: t.Y}, HasPixel: true,
					Msg: "persisted stage result is missing for tile"}
			}
			if r.X != t.X || r.Y != t.Y || r.W != t.W || r.H != t.H {
				return nil, &Error{Code: CodeDiff, Entry: meta.entry, Tile: t.Index,
					Field: "geometry", Msg: "persisted tile geometry does not match grid"}
			}
			if r.BitDepth != meta.depth || r.Channels != meta.channels || r.AlphaIndex != meta.alpha {
				return nil, &Error{Code: CodeDiff, Entry: meta.entry, Tile: t.Index,
					Field: "bitDepth", Msg: fmt.Sprintf("got depth=%d channels=%d alpha=%d", r.BitDepth, r.Channels, r.AlphaIndex)}
			}
			st := r.StageImage()
			if err := st.Validate(); err != nil {
				return nil, &Error{Code: CodeDiff, Entry: meta.entry, Tile: t.Index,
					Field: "pixels", Msg: err.Error()}
			}
			if len(dropped) == 0 {
				dropped = append([]DroppedChannel(nil), st.Dropped...)
			} else if !equalDropped(dropped, st.Dropped) {
				return nil, &Error{Code: CodeDiff, Entry: meta.entry, Tile: t.Index,
					Field: "dropped", Msg: "dropped-channel metadata differs across tiles"}
			}
			if err := placeStageTile(out, t, st); err != nil {
				return nil, &Error{Code: CodeDiff, Entry: meta.entry, Tile: t.Index,
					Field: "assemble", Msg: err.Error()}
			}
		}
		out.Dropped = dropped
		if err := out.Validate(); err != nil {
			return nil, &Error{Code: CodeDiff, Entry: meta.entry, Tile: -1, Field: "stage", Msg: err.Error()}
		}
		report.StageSummaries = append(report.StageSummaries, summarizeStage(out))
		final = out
	}
	if final == nil {
		return nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "stages", Msg: "plan produced no final image"}
	}
	for _, sample := range it.Samples {
		meta, ok := metaByEntry(metas, sample.Entry)
		if !ok {
			return nil, schemaErr(sample.Entry, "entry", "truth sample references unknown entry")
		}
		if sample.BitDepth != meta.depth {
			return nil, &Error{Code: CodeDiff, Entry: sample.Entry, Tile: tileAt(tiles, sample.X, sample.Y),
				Field: "bitDepth", Pixel: Point{X: sample.X, Y: sample.Y}, HasPixel: true,
				Msg: fmt.Sprintf("sample says depth %d, persisted stage is %d", sample.BitDepth, meta.depth)}
		}
		if len(sample.Channels) != meta.channels {
			return nil, schemaErr(sample.Entry, "channels", "truth sample has %d channels, expected %d", len(sample.Channels), meta.channels)
		}
		tile, ok := tileContaining(tiles, sample.X, sample.Y)
		if !ok {
			return nil, schemaErr(sample.Entry, "sample", "sample is outside image")
		}
		r, ok := rows[stageTileID{Entry: sample.Entry, Tile: tile.Index}]
		if !ok {
			return nil, &Error{Code: CodeDiff, Entry: sample.Entry, Tile: tile.Index,
				Field: "stageResult", Pixel: Point{X: sample.X, Y: sample.Y}, HasPixel: true,
				Msg: "persisted stage result is missing"}
		}
		st := r.StageImage()
		lx, ly := sample.X-tile.X, sample.Y-tile.Y
		off := (ly*tile.W + lx) * st.Channels
		actual := append([]uint16(nil), st.Pixels[off:off+st.Channels]...)
		for c, want := range sample.Channels {
			diff := absDiff(actual[c], want)
			if diff > report.MaxDeviation {
				report.MaxDeviation = diff
			}
			if diff > sample.Tolerance {
				return nil, &Error{Code: CodeDiff, Entry: sample.Entry, Tile: tile.Index,
					Field: fmt.Sprintf("channels[%d]", c),
					Pixel: Point{X: sample.X, Y: sample.Y}, HasPixel: true,
					Left: actual, Right: append([]uint16(nil), sample.Channels...),
					Msg: fmt.Sprintf("sample difference %d exceeds tolerance %d (persisted=%v truth=%v)",
						diff, sample.Tolerance, actual, sample.Channels)}
			}
		}
		report.SamplesChecked++
	}
	return report, nil
}

func expectedStageMetas(p *Plan) []expectedStageMeta {
	alpha := 3
	out := make([]expectedStageMeta, 0, len(p.Entries))
	for _, e := range p.Entries {
		targetAlpha := -1
		for dst, src := range e.Permutation {
			if alpha >= 0 && src == alpha {
				targetAlpha = dst
				break
			}
		}
		out = append(out, expectedStageMeta{
			entry: e.Seq, depth: e.TargetDepth, channels: e.TargetChannels, alpha: targetAlpha,
		})
		alpha = targetAlpha
	}
	return out
}

func parseGeometryTile(geometry string) (int, error) {
	for _, part := range strings.Split(geometry, "/") {
		if strings.HasPrefix(part, "tile=") {
			n, err := strconv.Atoi(strings.TrimPrefix(part, "tile="))
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("invalid tile size in geometry %q", geometry)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("geometry %q has no tile size", geometry)
}

func metaByEntry(metas []expectedStageMeta, entry int) (expectedStageMeta, bool) {
	for _, m := range metas {
		if m.entry == entry {
			return m, true
		}
	}
	return expectedStageMeta{}, false
}

func tileContaining(tiles []Tile, x, y int) (Tile, bool) {
	for _, t := range tiles {
		if x >= t.X && y >= t.Y && x < t.X+t.W && y < t.Y+t.H {
			return t, true
		}
	}
	return Tile{}, false
}

func equalDropped(a, b []DroppedChannel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func absDiff(a, b uint16) uint16 {
	if a > b {
		return a - b
	}
	return b - a
}
