package scenario

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"obsproj/internal/kernel"
	"obsproj/internal/model"
	"obsproj/internal/schema"
)

const DefaultSeed int64 = 20261126

type Bundle struct {
	Session *model.Session
	V1      []byte
	V2      []byte
	V3      []byte
	Truth   TruthManifest
}

type TruthWindow struct {
	Window          int              `json:"window"`
	At              int64            `json:"at_tick"`
	SourceRegion    model.Rect       `json:"source_region"`
	ActiveScreens   []string         `json:"active_screens"`
	TopologyVersion int64            `json:"topology_version"`
	Decisions       []model.Decision `json:"decisions"`
	Screens         []TruthScreen    `json:"screens"`
}

type TruthScreen struct {
	ScreenID      string              `json:"screen_id"`
	RawPixels     model.PixelRect     `json:"raw_pixels"`
	Pixels        model.PixelRect     `json:"pixels"`
	Params        model.MappingParams `json:"params"`
	Metrics       model.ErrorMetrics  `json:"metrics"`
	Packets       []model.Packet      `json:"packets"`
	ReplayWindows []int               `json:"replay_windows,omitempty"`
	ContentHash   string              `json:"content_hash"`
}

type TruthManifest struct {
	Seed              int64                 `json:"seed"`
	SessionID         string                `json:"session_id"`
	SemanticHash      string                `json:"semantic_hash"`
	SchemaVersions    []string              `json:"schema_versions"`
	SchemaHashes      map[string]string     `json:"schema_hashes"`
	UpgradeTracePaths []string              `json:"upgrade_trace_paths"`
	Decisions         []model.Decision      `json:"decisions"`
	Windows           []TruthWindow         `json:"windows"`
	Final             model.RuntimeSnapshot `json:"final"`
}

type LossyBundle struct {
	Session *model.Session
	V1      schema.ExportResult
	V1Err   *model.KernelError
	V2      []byte
	V3      []byte
}

func Generate(seed int64) (*Bundle, error) {
	session := buildCompatibleSession(seed)
	v1Export, err := schema.Export(session, schema.Version1)
	if err != nil {
		return nil, fmt.Errorf("v1 export unexpectedly lossy: %w", err)
	}
	v2Export, err := schema.Export(session, schema.Version2)
	if err != nil {
		return nil, fmt.Errorf("v2 export failed: %w", err)
	}
	v3Export, err := schema.Export(session, schema.Version3)
	if err != nil {
		return nil, fmt.Errorf("v3 export failed: %w", err)
	}
	loaded := map[string]*model.Session{}
	for version, data := range map[string][]byte{
		schema.Version1: v1Export.Data,
		schema.Version2: v2Export.Data,
		schema.Version3: v3Export.Data,
	} {
		decoded, decodeErr := schema.Decode(data)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s decode failed: %w", version, decodeErr)
		}
		loaded[version] = decoded
	}
	hash := session.Fingerprint()
	for version, decoded := range loaded {
		if decoded.Fingerprint() != hash {
			return nil, fmt.Errorf("%s semantic fingerprint mismatch", version)
		}
	}
	truth, err := buildTruth(session)
	if err != nil {
		return nil, err
	}
	truth.SchemaHashes = map[string]string{
		schema.Version1: hashBytes(v1Export.Data),
		schema.Version2: hashBytes(v2Export.Data),
		schema.Version3: hashBytes(v3Export.Data),
	}
	for _, trace := range loaded[schema.Version1].UpgradeTraces {
		truth.UpgradeTracePaths = append(truth.UpgradeTracePaths, trace.Path)
	}
	sort.Strings(truth.UpgradeTracePaths)
	return &Bundle{
		Session: session,
		V1:      v1Export.Data,
		V2:      v2Export.Data,
		V3:      v3Export.Data,
		Truth:   truth,
	}, nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func GenerateLossy(seed int64) (*LossyBundle, error) {
	session := buildLossySession(seed)
	v1Export, v1Err := schema.Export(session, schema.Version1)
	var kernelErr *model.KernelError
	if v1Err != nil {
		asErr, ok := model.AsKernelError(v1Err)
		if !ok || asErr.Code != model.CodeLossy {
			return nil, fmt.Errorf("unexpected v1 lossy export failure: %w", v1Err)
		}
		kernelErr = asErr
	}
	if v1Export.Data == nil {
		return nil, fmt.Errorf("v1 lossy export produced no loadable artifact")
	}
	v2Export, err := schema.Export(session, schema.Version2)
	if err != nil {
		return nil, fmt.Errorf("v2 export failed: %w", err)
	}
	v3Export, err := schema.Export(session, schema.Version3)
	if err != nil {
		return nil, fmt.Errorf("v3 export failed: %w", err)
	}
	return &LossyBundle{
		Session: session,
		V1:      v1Export,
		V1Err:   kernelErr,
		V2:      v2Export.Data,
		V3:      v3Export.Data,
	}, nil
}

func buildCompatibleSession(seed int64) *model.Session {
	if seed == 0 {
		seed = DefaultSeed
	}
	rng := rand.New(rand.NewSource(seed))
	tickRate := int64(1200)
	internalRate, _ := model.InternalTickRate(tickRate)
	screens := []model.Screen{
		newScreen("A", "客厅主屏", 0, 0, 1280, 720, model.Scale150, model.Chroma420,
			[]model.Scale{model.Scale100, model.Scale150, model.Scale200},
			[]model.Chroma{model.Chroma420, model.Chroma422}),
		newScreen("B", "书房副屏", -640, -360, 1920, 1080, model.Scale100, model.Chroma422,
			[]model.Scale{model.Scale100, model.Scale150},
			[]model.Chroma{model.Chroma420, model.Chroma422}),
		newScreen("C", "餐厅屏", 320, 180, 1024, 768, model.Scale200, model.Chroma420,
			[]model.Scale{model.Scale150, model.Scale200},
			[]model.Chroma{model.Chroma420, model.Chroma422}),
		newScreen("D", "移动屏", -200, -100, 800, 600, model.Scale150, model.Chroma422,
			[]model.Scale{model.Scale100, model.Scale150, model.Scale200},
			[]model.Chroma{model.Chroma420, model.Chroma422}),
		newScreen("E", "观战屏", 50, 50, 1366, 768, model.Scale150, model.Chroma420,
			[]model.Scale{model.Scale100, model.Scale150, model.Scale200},
			[]model.Chroma{model.Chroma420, model.Chroma422}),
	}
	baseX := int64(-120 + rng.Intn(41))
	baseY := int64(60 + rng.Intn(21))
	baseW := int64(1180 + rng.Intn(31))
	baseH := int64(680 + rng.Intn(31))
	events := []model.Event{
		{ID: "ev-001", Window: 1, At: 0, Kind: model.EventJoin, ScreenID: "A"},
		{ID: "ev-002", Window: 1, At: 0, Kind: model.EventJoin, ScreenID: "B"},
		{ID: "ev-003", Window: 1, At: 0, Kind: model.EventJoin, ScreenID: "C"},
		{ID: "ev-010", Window: 2, At: 750, Kind: model.EventJoin, ScreenID: "D"},
		{ID: "ev-011", Window: 2, At: 750, Kind: model.EventExit, ScreenID: "B"},
		{ID: "ev-012", Window: 2, At: 750, Kind: model.EventTopology, ScreenID: "B", Topology: &model.TopologyPatch{
			Resolution: &model.Size{W: 1600, H: 900},
		}},
		{ID: "ev-020", Window: 3, At: 1500, Kind: model.EventTopology, ScreenID: "A", Topology: &model.TopologyPatch{
			Resolution: &model.Size{W: 1920, H: 1080},
		}},
		{ID: "ev-021", Window: 3, At: 1500, Kind: model.EventTopology, ScreenID: "C", Topology: &model.TopologyPatch{
			Resolution: &model.Size{W: 900, H: 700},
		}},
		{ID: "ev-022", Window: 3, At: 1500, Kind: model.EventTopology, ScreenID: "D", Topology: &model.TopologyPatch{
			Origin:     &model.Point{X: -150 * 1000, Y: -80 * 1000},
			Resolution: &model.Size{W: 1000, H: 700},
		}},
		{ID: "ev-030", Window: 4, At: 2250, Kind: model.EventExit, ScreenID: "C"},
		{ID: "ev-031", Window: 4, At: 2250, Kind: model.EventJoin, ScreenID: "C"},
		{ID: "ev-040", Window: 5, At: 3000, Kind: model.EventJoin, ScreenID: "B"},
		{ID: "ev-041", Window: 5, At: 3000, Kind: model.EventTopology, ScreenID: "B", Topology: &model.TopologyPatch{
			Resolution: &model.Size{W: 1600, H: 900},
		}},
		{ID: "ev-050", Window: 6, At: 3750, Kind: model.EventJoin, ScreenID: "E"},
		{ID: "ev-051", Window: 6, At: 3750, Kind: model.EventTopology, ScreenID: "D", Topology: &model.TopologyPatch{
			Resolution: &model.Size{W: 1000, H: 700},
		}},
		{ID: "ev-060", Window: 7, At: 4500, Kind: model.EventExit, ScreenID: "A"},
		{ID: "ev-061", Window: 7, At: 4500, Kind: model.EventJoin, ScreenID: "A"},
		{ID: "ev-070", Window: 8, At: 5250, Kind: model.EventExit, ScreenID: "D"},
	}
	commands := make([]model.Command, 0, 8*len(screens))
	for window := 1; window <= 8; window++ {
		internalAt := int64(window-1) * 750
		region := model.Rect{
			X: (baseX + int64((window%3)*11)) * model.InternalMilliPerUnit,
			Y: (baseY + int64((window%2)*7)) * model.InternalMilliPerUnit,
			W: (baseW + int64(window%4)) * model.InternalMilliPerUnit,
			H: (baseH + int64(window%3)) * model.InternalMilliPerUnit,
		}
		for _, screen := range screens {
			commands = append(commands, model.Command{
				Window:   window,
				At:       internalAt,
				ScreenID: screen.ID,
				Region:   region,
			})
		}
	}
	session := &model.Session{
		ID:            fmt.Sprintf("obs-session-%d", seed),
		Seed:          seed,
		SourceVersion: "internal",
		TickRate:      tickRate,
		InternalRate:  internalRate,
		Screens:       screens,
		Events:        events,
		Commands:      commands,
	}
	session.Normalize()
	return session
}

func buildLossySession(seed int64) *model.Session {
	if seed == 0 {
		seed = DefaultSeed
	}
	tickRate := int64(1200)
	internalRate, _ := model.InternalTickRate(tickRate)
	screens := []model.Screen{
		newScreen("L", "旧协议测试屏", 0, 0, 1280, 720, model.Scale125, model.Chroma444,
			[]model.Scale{model.Scale100, model.Scale125, model.Scale150},
			[]model.Chroma{model.Chroma420, model.Chroma422, model.Chroma444}),
	}
	events := []model.Event{
		{ID: "lossy-join", Window: 1, At: 0, Kind: model.EventJoin, ScreenID: "L"},
		{ID: "lossy-topology", Window: 2, At: 750, Kind: model.EventTopology, ScreenID: "L", Topology: &model.TopologyPatch{
			Scale:  scalePtr(model.Scale125),
			Chroma: chromaPtr(model.Chroma444),
		}},
	}
	commands := []model.Command{
		{Window: 1, At: 0, ScreenID: "L", Region: model.Rect{X: -50 * 1000, Y: 20 * 1000, W: 1500 * 1000, H: 800 * 1000}},
		{Window: 2, At: 750, ScreenID: "L", Region: model.Rect{X: -40 * 1000, Y: 30 * 1000, W: 1490 * 1000, H: 790 * 1000}},
		{Window: 3, At: 1500, ScreenID: "L", Region: model.Rect{X: -30 * 1000, Y: 40 * 1000, W: 1480 * 1000, H: 780 * 1000}},
	}
	session := &model.Session{
		ID:            fmt.Sprintf("obs-lossy-%d", seed),
		Seed:          seed,
		SourceVersion: "internal",
		TickRate:      tickRate,
		InternalRate:  internalRate,
		Screens:       screens,
		Events:        events,
		Commands:      commands,
	}
	session.Normalize()
	return session
}

func newScreen(id, name string, ox, oy, w, h int64, scale model.Scale, chroma model.Chroma, scales []model.Scale, chromas []model.Chroma) model.Screen {
	return model.Screen{
		ID:         id,
		Name:       name,
		Origin:     model.Point{X: ox * 1000, Y: oy * 1000},
		Resolution: model.Size{W: w, H: h},
		Scale:      scale,
		Chroma:     chroma,
		Capability: model.Capability{
			Scales:        scales,
			Chromas:       chromas,
			MaxPacketSize: model.DefaultMaxPacketSize,
		},
	}
}

func scalePtr(v model.Scale) *model.Scale    { return &v }
func chromaPtr(v model.Chroma) *model.Chroma { return &v }

func buildTruth(session *model.Session) (TruthManifest, error) {
	engine, err := kernel.NewEngine(session, kernel.DefaultEngineOptions())
	if err != nil {
		return TruthManifest{}, err
	}
	truth := TruthManifest{
		Seed:           session.Seed,
		SessionID:      session.ID,
		SemanticHash:   session.Fingerprint(),
		SchemaVersions: []string{schema.Version1, schema.Version2, schema.Version3},
	}
	for _, window := range engine.WindowNumbers() {
		commit, _, processErr := engine.ProcessWindow(window)
		if processErr != nil {
			return TruthManifest{}, processErr
		}
		truth.Decisions = append(truth.Decisions, commit.Decisions...)
		tw := TruthWindow{
			Window:          commit.Window,
			At:              commit.At,
			SourceRegion:    commit.SourceRegion,
			ActiveScreens:   append([]string(nil), commit.ActiveScreens...),
			TopologyVersion: commit.TopologyVersion,
			Decisions:       append([]model.Decision(nil), commit.Decisions...),
		}
		for _, screen := range commit.Screens {
			tw.Screens = append(tw.Screens, TruthScreen{
				ScreenID:      screen.ScreenID,
				RawPixels:     screen.Mapped.RawPixels,
				Pixels:        screen.Mapped.Pixels,
				Params:        screen.Mapped.Params,
				Metrics:       screen.Metrics,
				Packets:       append([]model.Packet(nil), screen.Packets...),
				ReplayWindows: append([]int(nil), screen.ReplayWindows...),
				ContentHash:   screen.ContentHash,
			})
		}
		truth.Windows = append(truth.Windows, tw)
	}
	truth.Final = engine.FinalSnapshot()
	return truth, nil
}

func WriteBundle(dir string, bundle *Bundle) error {
	if bundle == nil {
		return fmt.Errorf("nil bundle")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"session_v1.json": bundle.V1,
		"session_v2.json": bundle.V2,
		"session_v3.json": bundle.V3,
	}
	truthData, err := json.MarshalIndent(bundle.Truth, "", "  ")
	if err != nil {
		return err
	}
	files["truth.json"] = append(truthData, '\n')
	manifest := map[string]any{
		"seed":          bundle.Session.Seed,
		"session_id":    bundle.Session.ID,
		"semantic_hash": bundle.Session.Fingerprint(),
		"schema_hashes": bundle.Truth.SchemaHashes,
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	files["manifest.json"] = append(manifestData, '\n')
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, files[name], 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}
