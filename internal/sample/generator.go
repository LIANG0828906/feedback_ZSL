package sample

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"haptickernel/internal/synth"
)

// Seed 是固定样本种子，保证任意机器、任意路径产出相同文件内容。
const Seed int64 = 20261124

const (
	FrameCount       = 48
	GridPerFrame     = 10
	ChannelCount     = 4
	SampleRateHz     = 8000
	QuantUnit        = 2
	SamplesPerPacket = 16
	EventCount       = 64
)

// genEvent 是落盘格式，字段名与加载校验的 DTO 对齐。
type genEvent struct {
	ID          string          `json:"id"`
	Frame       int             `json:"frame"`
	FrameIndex  int             `json:"frame_index"`
	Type        synth.EventType `json:"type"`
	Channel     int             `json:"channel"`
	Strength    int             `json:"strength"`
	Priority    int             `json:"priority"`
	NoInterrupt bool            `json:"no_interrupt"`
	Template    string          `json:"template"`
}

func templates() [][]synth.Template {
	return [][]synth.Template{
		{
			{Name: "pulse", Duration: 8, Points: []synth.EnvelopePoint{
				{At: 0, Level: 1000}, {At: 2, Level: 600}, {At: 5, Level: 200}, {At: 8, Level: 0},
			}},
			{Name: "rumble", Duration: 24, Points: []synth.EnvelopePoint{
				{At: 0, Level: 200}, {At: 4, Level: 900}, {At: 12, Level: 700}, {At: 20, Level: 300}, {At: 24, Level: 0},
			}},
			{Name: "click", Duration: 4, Points: []synth.EnvelopePoint{
				{At: 0, Level: 800}, {At: 1, Level: 400}, {At: 4, Level: 0},
			}},
		},
		{
			{Name: "pulse", Duration: 8, Points: []synth.EnvelopePoint{
				{At: 0, Level: 900}, {At: 3, Level: 500}, {At: 8, Level: 0},
			}},
			{Name: "rumble", Duration: 24, Points: []synth.EnvelopePoint{
				{At: 0, Level: 300}, {At: 6, Level: 1000}, {At: 16, Level: 500}, {At: 24, Level: 0},
			}},
		},
		{
			{Name: "pulse", Duration: 8, Points: []synth.EnvelopePoint{
				{At: 0, Level: 700}, {At: 4, Level: 350}, {At: 8, Level: 0},
			}},
			{Name: "click", Duration: 4, Points: []synth.EnvelopePoint{
				{At: 0, Level: 1000}, {At: 2, Level: 300}, {At: 4, Level: 0},
			}},
		},
		{
			{Name: "rumble", Duration: 24, Points: []synth.EnvelopePoint{
				{At: 0, Level: 100}, {At: 5, Level: 600}, {At: 18, Level: 250}, {At: 24, Level: 0},
			}},
			{Name: "click", Duration: 4, Points: []synth.EnvelopePoint{
				{At: 0, Level: 600}, {At: 1, Level: 300}, {At: 4, Level: 0},
			}},
		},
	}
}

// Generate 生成确定性样本并写入 dir 下的 manifest.json 与 events.json。
func Generate(dir string) (manifestPath, eventsPath string, err error) {
	rng := rand.New(rand.NewSource(Seed))

	tpls := templates()
	chans := make([]synth.ChannelCap, ChannelCount)
	for c := 0; c < ChannelCount; c++ {
		chans[c] = synth.ChannelCap{
			Channel:      c,
			Templates:    tpls[c],
			MinEventGap:  0,
			TimingWindow: synth.TimingWindow{MinOffsetUS: 0, MaxOffsetUS: 100000},
		}
	}
	manifest := synth.Manifest{
		ManifestVersion:  "cap-v1",
		ChannelCount:     ChannelCount,
		SampleRateHz:     SampleRateHz,
		QuantUnit:        QuantUnit,
		SamplesPerPacket: SamplesPerPacket,
		FrameGrid:        GridPerFrame,
		Channels:         chans,
	}

	eventTypes := []synth.EventType{synth.TypeHit, synth.TypeShoot, synth.TypeBlast, synth.TypeStep, synth.TypeUIFeed}
	events := make([]genEvent, 0, EventCount)
	for i := 0; i < EventCount; i++ {
		ch := rng.Intn(ChannelCount)
		tpl := tpls[ch][rng.Intn(len(tpls[ch]))]
		ev := genEvent{
			ID:          "ev-" + itoa(i),
			Frame:       rng.Intn(FrameCount),
			FrameIndex:  rng.Intn(GridPerFrame),
			Type:        eventTypes[rng.Intn(len(eventTypes))],
			Channel:     ch,
			Strength:    50 + rng.Intn(951),
			Priority:    1 + rng.Intn(10),
			NoInterrupt: rng.Intn(100) < 25,
			Template:    tpl.Name,
		}
		events = append(events, ev)
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Frame != events[j].Frame {
			return events[i].Frame < events[j].Frame
		}
		return events[i].FrameIndex < events[j].FrameIndex
	})

	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	manifestPath = filepath.Join(dir, "manifest.json")
	eventsPath = filepath.Join(dir, "events.json")
	if err = writeJSON(manifestPath, manifest); err != nil {
		return "", "", err
	}
	if err = writeJSON(eventsPath, events); err != nil {
		return "", "", err
	}
	return manifestPath, eventsPath, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func itoa(i int) string {
	if i == 0 {
		return "000"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
