// Package loader 负责本地确定性样本的加载、元数据校验与输入侧异常注入。
// 加载过程不访问网络、真实声卡或清单之外的外部音频资源。
package loader

import (
	"math"
	"os"
	"path/filepath"

	"spatialir/internal/generator"
	"spatialir/internal/model"
)

// Dataset 是加载后的样本：清单、逐通道原信号与干声片段。
type Dataset struct {
	Manifest *model.Manifest
	Signals  [][]float64
	Dry      [][]float64
}

// Load 从目录读取清单并校验/加载全部信号文件。
func Load(dir, manifestName string) (*Dataset, error) {
	manifest, err := generator.ReadManifest(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	if err := validateMeta(manifest); err != nil {
		return nil, err
	}
	signals := make([][]float64, manifest.Layout.Channels)
	dry := make([][]float64, manifest.Layout.Channels)
	for ch, name := range manifest.Layout.Names {
		// E_CHAN：清单声明了声道但信号文件缺失（可由生成器故障注入产生）。
		if _, err := os.Stat(filepath.Join(dir, manifest.SignalFiles[ch])); os.IsNotExist(err) {
			return nil, model.NewError(model.CodeChan, "signal file missing for declared channel").
				WithChannel(name)
		} else if err != nil {
			return nil, err
		}
		sr, sig, err := generator.ReadSignalFile(filepath.Join(dir, manifest.SignalFiles[ch]))
		if err != nil {
			return nil, err
		}
		if sr != manifest.SampleRate {
			return nil, model.NewError(model.CodeMeta, "signal sample_rate conflicts with manifest").
				WithField("sample_rate").WithChannel(name)
		}
		if len(sig[0]) != manifest.LengthSamples {
			return nil, model.NewError(model.CodeFrame, "signal length conflicts with manifest").
				WithChannel(name)
		}
		signals[ch] = sig[0]
		_, drySig, err := generator.ReadSignalFile(filepath.Join(dir, manifest.DryFiles[ch]))
		if err != nil {
			return nil, err
		}
		dry[ch] = drySig[0]
	}
	if err := validateInputFaults(manifest); err != nil {
		return nil, err
	}
	return &Dataset{Manifest: manifest, Signals: signals, Dry: dry}, nil
}

func validateMeta(m *model.Manifest) error {
	if m.SampleRate <= 0 {
		return model.NewError(model.CodeMeta, "sample_rate missing or non-positive").WithField("sample_rate")
	}
	if m.Layout.Channels <= 0 {
		return model.NewError(model.CodeMeta, "layout.channels missing").WithField("layout.channels")
	}
	if len(m.Layout.Names) == 0 {
		return model.NewError(model.CodeMeta, "layout.names missing").WithField("layout.names")
	}
	if m.Layout.Channels != len(m.Layout.Names) {
		return model.NewError(model.CodeMeta, "layout channels/names mismatch").WithField("layout.names")
	}
	if m.LengthSamples <= 0 {
		return model.NewError(model.CodeMeta, "length_samples missing").WithField("length_samples")
	}
	if len(m.Truth) != m.Layout.Channels {
		return model.NewError(model.CodeMeta, "per-channel truth missing").WithField("truth")
	}
	return nil
}

// validateInputFaults 在加载阶段处理与输入整体相关的异常声明。
// 帧/通道相关的倒置、溢出与坏帧在分析阶段按帧触发，保证帧级隔离。
func validateInputFaults(m *model.Manifest) error {
	f := m.Faults
	if f.MissingChannel != "" {
		for _, name := range m.Layout.Names {
			if name == f.MissingChannel {
				return model.NewError(model.CodeChan, "injected missing channel").WithChannel(name)
			}
		}
		return model.NewError(model.CodeChan, "missing channel not in layout").WithChannel(f.MissingChannel)
	}
	return nil
}

// ValidateParams 校验分帧与渲染参数，越界返回 E_PARA 并指出字段名。
func ValidateParams(p model.FrameParams, length int) error {
	check := func(ok bool, field, reason string) error {
		if !ok {
			return model.NewError(model.CodePara, reason).WithField(field)
		}
		return nil
	}
	if err := check(p.FrameLength > 0, "frame_length", "frame_length must be positive"); err != nil {
		return err
	}
	if err := check(p.FrameHop > 0 && p.FrameHop <= p.FrameLength, "frame_hop", "frame_hop out of range"); err != nil {
		return err
	}
	if err := check(p.WindowLen > 0 && p.WindowLen <= p.FrameLength, "window_len", "window_len out of range"); err != nil {
		return err
	}
	if err := check(p.ConvWindow > 0 && p.ConvWindow <= length, "conv_window", "conv_window out of range"); err != nil {
		return err
	}
	if err := check(p.RenderBlock > 0 && p.RenderBlock <= length, "render_block", "render_block out of range"); err != nil {
		return err
	}
	if err := check(p.InterpOrder == 0 || p.InterpOrder == 1 || p.InterpOrder == 3,
		"interp_order", "interp_order must be 0, 1 or 3"); err != nil {
		return err
	}
	if err := check(p.BoundScale > 0 && !math.IsNaN(p.BoundScale), "bound_scale", "bound_scale must be positive"); err != nil {
		return err
	}
	if err := check(p.MaxWorkers > 0, "max_workers", "max_workers must be positive"); err != nil {
		return err
	}
	if err := check(p.BufferCap > 0, "buffer_cap", "buffer_cap must be positive"); err != nil {
		return err
	}
	if p.FrameLength > length {
		return model.NewError(model.CodePara, "frame_length exceeds signal length").WithField("frame_length")
	}
	return nil
}
