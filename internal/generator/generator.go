// Package generator 按固定种子在本地确定性产出多通道脉冲响应样本、
// 干声片段、清单与逐通道解析真值。真值只来源于解析构造，不读取任何分析输出。
//
// 信号文件格式（本地私有二进制，无外部依赖）：
//
//	magic(4)="SIR1" | sampleRate int32 LE | channels int32 LE | length int32 LE
//	其后为 channels*length 个 float64 LE 采样，通道交织存放。
package generator

import (
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"spatialir/internal/model"
)

const (
	SchemaVersion    = "sir-manifest/1"
	GeneratorVersion = "sir-gen/1.0.0"
	magic            = "SIR1"
)

// Spec 描述一组待生成样本。
type Spec struct {
	Name        string
	Seed        int64
	SampleRate  int
	Layout      model.ChannelLayout
	Length      int
	Reflections int // 每通道早期反射条数
	Faults      model.FaultSpec
}

// BuiltinSpecs 是工作台自带的三组确定性样本规格。
func BuiltinSpecs() []Spec {
	return []Spec{
		{
			Name: "stereo_48k", Seed: 2026112401, SampleRate: 48000,
			Layout: model.ChannelLayout{ID: "stereo", Names: []string{"L", "R"}, Channels: 2},
			Length: 4800, Reflections: 4,
		},
		{
			Name: "quad_44k1", Seed: 2026112402, SampleRate: 44100,
			Layout: model.ChannelLayout{ID: "quad", Names: []string{"FL", "FR", "RL", "RR"}, Channels: 4},
			Length: 4410, Reflections: 3,
		},
		{
			Name: "surround_96k", Seed: 2026112403, SampleRate: 96000,
			Layout: model.ChannelLayout{ID: "5.1", Names: []string{"FL", "FR", "C", "LFE", "SL", "SR"}, Channels: 6},
			Length: 9600, Reflections: 5,
		},
	}
}

// Generate 在 outDir 下产出一个样本集，返回清单与多通道信号（便于内存模式复用）。
func Generate(spec Spec, outDir string) (*model.Manifest, [][]float64, [][]float64, error) {
	if spec.SampleRate <= 0 {
		return nil, nil, nil, model.NewError(model.CodeMeta, "sample_rate missing").WithField("sample_rate")
	}
	if spec.Layout.Channels == 0 || len(spec.Layout.Names) == 0 {
		return nil, nil, nil, model.NewError(model.CodeMeta, "channel layout missing").WithField("layout")
	}
	rng := rand.New(rand.NewSource(spec.Seed))
	truth := make([]model.ChannelTruth, 0, spec.Layout.Channels)
	signals := make([][]float64, spec.Layout.Channels)
	dry := make([][]float64, spec.Layout.Channels)

	for ch := 0; ch < spec.Layout.Channels; ch++ {
		ct, sig, drySig := buildChannel(rng, spec, ch)
		truth = append(truth, ct)
		signals[ch] = sig
		dry[ch] = drySig
	}

	// 通道缺失异常：清单仍声明布局，但对应信号文件不写出，加载链路触发 E_CHAN。
	signalFiles := make([]string, spec.Layout.Channels)
	dryFiles := make([]string, spec.Layout.Channels)
	for ch, name := range spec.Layout.Names {
		base := spec.Name + "_" + name
		signalFiles[ch] = base + ".sir"
		dryFiles[ch] = base + "_dry.sir"
	}

	manifest := &model.Manifest{
		SchemaVersion: SchemaVersion,
		Seed:          spec.Seed,
		SampleRate:    spec.SampleRate,
		Layout:        spec.Layout,
		LengthSamples: spec.Length,
		SignalFiles:   signalFiles,
		DryFiles:      dryFiles,
		Reference:     spec.Layout.Names[0],
		Truth:         truth,
		Faults:        spec.Faults,
		GenVersion:    GeneratorVersion,
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, nil, nil, err
	}
	for ch, name := range spec.Layout.Names {
		if spec.Faults.MissingChannel == name {
			continue
		}
		if err := writeSignalFile(filepath.Join(outDir, signalFiles[ch]), spec.SampleRate, signals[ch:ch+1]); err != nil {
			return nil, nil, nil, err
		}
		if err := writeSignalFile(filepath.Join(outDir, dryFiles[ch]), spec.SampleRate, dry[ch:ch+1]); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := writeJSON(filepath.Join(outDir, spec.Name+"_manifest.json"), manifest); err != nil {
		return nil, nil, nil, err
	}
	return manifest, signals, dry, nil
}

func buildChannel(rng *rand.Rand, spec Spec, ch int) (model.ChannelTruth, []float64, []float64) {
	n := spec.Length
	sig := make([]float64, n)
	drySig := make([]float64, n)

	// 直达声：到达点落在前 1/8 区间，声道间带确定性微时差。
	direct := 64 + (ch*17+int(rng.Int63n(48)))%(n/8)
	directAmp := 0.72 + rng.Float64()*0.18
	sig[direct] += directAmp
	drySig[direct] = directAmp

	ct := model.ChannelTruth{
		Channel:      spec.Layout.Names[ch],
		DirectSample: direct,
		DirectAmp:    directAmp,
	}

	// 离散早期反射：到达点严格递增，振幅确定性衰减。
	last := direct
	refs := make([]model.ReflectionTruth, 0, spec.Reflections)
	for k := 0; k < spec.Reflections; k++ {
		gap := n/16 + int(rng.Int63n(int64(n/12)))
		arrival := last + gap
		if arrival >= n-2 {
			break
		}
		amp := (0.34 - float64(k)*0.045) * (0.85 + rng.Float64()*0.3)
		sig[arrival] += amp
		refs = append(refs, model.ReflectionTruth{
			ArrivalSample: arrival,
			Amplitude:     amp,
			Energy:        amp * amp,
		})
		last = arrival
	}
	ct.Reflections = refs

	// 指数衰减尾音：从最后一个反射后开始，整段确定性叠加。
	tailOnset := last + n/32
	if tailOnset >= n {
		tailOnset = n - 1
	}
	tailAmp := 0.16 + rng.Float64()*0.06
	tailRate := 2.0/float64(n) + rng.Float64()*2.0/float64(n)
	for i := tailOnset; i < n; i++ {
		sig[i] += tailAmp * math.Exp(-tailRate*float64(i-tailOnset))
	}
	ct.Tail = model.TailTruth{OnsetSample: tailOnset, Amplitude: tailAmp, Rate: tailRate}
	return ct, sig, drySig
}

func writeSignalFile(path string, sampleRate int, channels [][]float64) error {
	ch := len(channels)
	n := len(channels[0])
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := make([]byte, 16)
	copy(hdr[0:4], magic)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(ch))
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(n))
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	buf := make([]byte, 8)
	for i := 0; i < n; i++ {
		for c := 0; c < ch; c++ {
			binary.LittleEndian.PutUint64(buf, math.Float64bits(channels[c][i]))
			if _, err := f.Write(buf); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadSignalFile 读取本地 .sir 信号文件，返回采样率与逐通道采样。
func ReadSignalFile(path string) (int, [][]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, err
	}
	if len(data) < 16 || string(data[0:4]) != magic {
		return 0, nil, model.NewError(model.CodeFrame, "bad signal file header").WithField("magic")
	}
	sr := int(binary.LittleEndian.Uint32(data[4:8]))
	ch := int(binary.LittleEndian.Uint32(data[8:12]))
	n := int(binary.LittleEndian.Uint32(data[12:16]))
	if len(data) != 16+ch*n*8 {
		return 0, nil, model.NewError(model.CodeFrame, "signal file length mismatch")
	}
	out := make([][]float64, ch)
	for c := 0; c < ch; c++ {
		out[c] = make([]float64, n)
	}
	off := 16
	for i := 0; i < n; i++ {
		for c := 0; c < ch; c++ {
			out[c][i] = math.Float64frombits(binary.LittleEndian.Uint64(data[off:]))
			off += 8
		}
	}
	return sr, out, nil
}
