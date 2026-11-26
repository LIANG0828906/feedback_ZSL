package mirror

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

type GeneratedSample struct {
	Seed      int64
	Session   *Session
	Documents map[string][]byte
	Hashes    map[string]string
	Truth     *Truth
}

type deterministicRand struct {
	state uint64
}

func newDeterministicRand(seed int64) *deterministicRand {
	s := uint64(seed)
	if s == 0 {
		s = 0x9e3779b97f4a7c15
	}
	return &deterministicRand{state: s}
}

func (r *deterministicRand) next() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 2685821657736338717
}

func (r *deterministicRand) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func (r *deterministicRand) choiceInt64(values []int64) int64 {
	return values[r.intn(len(values))]
}

// Generate creates a fixed-seed canonical session, three schema documents,
// and the authoritative parsing truth. No device, network, or clock input is
// consulted.
func Generate(seed int64) (*GeneratedSample, error) {
	rng := newDeterministicRand(seed)
	domains := []Domain{
		{ID: "root", Parent: "", OriginX: 0, OriginY: 0},
		{ID: "west", Parent: "root", OriginX: 0, OriginY: 0},
		{ID: "east", Parent: "root", OriginX: 320, OriginY: 200},
	}
	widths := []int64{1280, 1536, 1920, 2560}
	heights := []int64{720, 864, 1080, 1440}
	packets := []int64{65536, 98304, 131072, 196608, 262144}
	screens := []Screen{
		{
			ID: "screen-alpha", Name: "Stage Alpha", Domain: "west", Master: true,
			OriginX: 0, OriginY: 0, Width: 1920, Height: 1080,
			ScaleCapabilities: []Scale{Scale100, Scale125}, DefaultScale: Scale100,
			ChromaCapabilities: []Chroma{Chroma444, Chroma420}, DefaultChroma: Chroma444,
			MaxPacketBytes: packets[rng.intn(len(packets))],
		},
		{
			ID: "screen-bravo", Name: "Stage Bravo", Domain: "west",
			OriginX: 64 + int64(rng.intn(48)), OriginY: 32 + int64(rng.intn(32)),
			Width: widths[1+rng.intn(2)], Height: heights[1+rng.intn(2)],
			ScaleCapabilities: []Scale{Scale100, Scale125}, DefaultScale: Scale125,
			ChromaCapabilities: []Chroma{Chroma444, Chroma420}, DefaultChroma: Chroma420,
			MaxPacketBytes: packets[1+rng.intn(len(packets)-1)],
		},
		{
			ID: "screen-charlie", Name: "Stage Charlie", Domain: "east",
			OriginX: 128 + int64(rng.intn(64)), OriginY: 64 + int64(rng.intn(32)),
			Width: widths[2+rng.intn(2)], Height: heights[2+rng.intn(2)],
			ScaleCapabilities: []Scale{Scale100, Scale125}, DefaultScale: Scale100,
			ChromaCapabilities: []Chroma{Chroma444, Chroma420}, DefaultChroma: Chroma444,
			MaxPacketBytes: packets[2+rng.intn(len(packets)-2)],
		},
		{
			ID: "screen-delta", Name: "Stage Delta", Domain: "east",
			OriginX: 192 + int64(rng.intn(64)), OriginY: 96 + int64(rng.intn(32)),
			Width: widths[rng.intn(2)], Height: heights[rng.intn(2)],
			ScaleCapabilities: []Scale{Scale100, Scale125}, DefaultScale: Scale125,
			ChromaCapabilities: []Chroma{Chroma444, Chroma420}, DefaultChroma: Chroma420,
			MaxPacketBytes: packets[rng.intn(3)],
		},
	}
	events := []Event{
		{ID: "ev-0000", Kind: EventJoin, ScreenID: "screen-alpha", Domain: "west", AtTick: 0},
		{ID: "ev-0001", Kind: EventJoin, ScreenID: "screen-bravo", Domain: "west", AtTick: 0},
		{ID: "ev-0002", Kind: EventJoin, ScreenID: "screen-charlie", Domain: "east", AtTick: 0},
		{ID: "ev-0003", Kind: EventJoin, ScreenID: "screen-delta", Domain: "east", AtTick: 0},
		{ID: "ev-0010", Kind: EventNegotiate, ScreenID: "screen-alpha", AtTick: 6},
		{ID: "ev-0011", Kind: EventNegotiate, ScreenID: "screen-bravo", AtTick: 6},
		{ID: "ev-0012", Kind: EventNegotiate, ScreenID: "screen-charlie", AtTick: 6},
		{ID: "ev-0013", Kind: EventNegotiate, ScreenID: "screen-delta", AtTick: 6},
		{ID: "ev-0020", Kind: EventTopology, ScreenID: "screen-bravo", FromDomain: "west", ToDomain: "east", AtTick: 10},
		{ID: "ev-0021", Kind: EventJoin, ScreenID: "screen-bravo", Domain: "east", AtTick: 10},
		{ID: "ev-0030", Kind: EventLeave, ScreenID: "screen-charlie", AtTick: 70},
		{ID: "ev-0031", Kind: EventJoin, ScreenID: "screen-charlie", Domain: "east", AtTick: 85},
		{ID: "ev-0032", Kind: EventNegotiate, ScreenID: "screen-charlie", AtTick: 90},
		{ID: "ev-0040", Kind: EventLeave, ScreenID: "screen-delta", AtTick: 95},
		{ID: "ev-0041", Kind: EventJoin, ScreenID: "screen-delta", Domain: "east", AtTick: 95},
		{ID: "ev-0042", Kind: EventJoin, ScreenID: "screen-delta", Domain: "east", AtTick: 105},
		{ID: "ev-0043", Kind: EventNegotiate, ScreenID: "screen-delta", AtTick: 110},
	}
	windowTimes := [][2]int64{{20, 30}, {40, 50}, {60, 80}, {90, 100}, {110, 125}, {130, 140}}
	windows := make([]Window, 0, len(windowTimes))
	for i, times := range windowTimes {
		x := int64(rng.intn(120))
		y := int64(rng.intn(80))
		w := int64(900 + rng.intn(520))
		h := int64(500 + rng.intn(320))
		if i == len(windowTimes)-1 {
			x, y, w, h = 0, 0, 1920, 1080
		}
		scale := []Scale{Scale100, Scale125}[rng.intn(2)]
		chroma := []Chroma{Chroma444, Chroma420}[rng.intn(2)]
		maxPacket := packets[2+rng.intn(len(packets)-2)]
		windows = append(windows, Window{
			ID: "window-" + pad2(i), Index: i,
			StartTick: times[0], EndTick: times[1],
			Region: Region{X: x, Y: y, W: w, H: h},
			Scale:  scale, Chroma: chroma, MaxPacketBytes: maxPacket,
		})
	}
	session := &Session{
		ID: "mirror-session-" + padSeed(seed), Seed: seed,
		SourceVersion: SchemaV3, AlgorithmVersion: AlgorithmVersion,
		TickMillis:              DefaultTickMillis,
		NegotiationTimeoutTicks: 80, ReconnectTimeoutTicks: 250,
		MasterScreenID: "screen-alpha",
		Domains:        domains, Screens: screens, Events: events, Windows: windows,
	}
	if err := NormalizeSession(session); err != nil {
		return nil, err
	}
	docs := map[string][]byte{}
	hashes := map[string]string{}
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		data, _, err := Export(session, version)
		if err != nil {
			return nil, err
		}
		docs[version] = data
		hashes[version] = HashBytes(data)
	}
	truth, err := BuildGeneratorTruth(session)
	if err != nil {
		return nil, err
	}
	truth.Seed = seed
	return &GeneratedSample{
		Seed: seed, Session: session, Documents: docs, Hashes: hashes, Truth: truth,
	}, nil
}

func pad2(v int) string {
	if v < 10 {
		return "0" + itoa(v)
	}
	return itoa(v)
}

func padSeed(seed int64) string {
	s := itoa64(seed)
	for len(s) < 10 {
		s = "0" + s
	}
	return s
}

func itoa(v int) string { return itoa64(int64(v)) }

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := [24]byte{}
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// WriteSample persists the generated documents and truth list.
func WriteSample(dir string, sample *GeneratedSample) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := map[string]string{
		SchemaV1: "session.v1.json",
		SchemaV2: "session.v2.json",
		SchemaV3: "session.v3.json",
	}
	versions := make([]string, 0, len(names))
	for version := range names {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	for _, version := range versions {
		if err := os.WriteFile(filepath.Join(dir, names[version]), sample.Documents[version], 0o644); err != nil {
			return err
		}
	}
	truth, err := json.MarshalIndent(sample.Truth, "", "  ")
	if err != nil {
		return err
	}
	truth = append(truth, '\n')
	return os.WriteFile(filepath.Join(dir, "truth.json"), truth, 0o644)
}
