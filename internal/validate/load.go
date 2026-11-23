package validate

import (
	"encoding/json"
	"fmt"
	"os"

	"haptickernel/internal/synth"
)

// 指针字段用于区分“缺失/未声明”与“显式零值”，缺失即报错而不是取零值。

type manifestDTO struct {
	ManifestVersion  *string         `json:"manifest_version"`
	ChannelCount     *int            `json:"channel_count"`
	SampleRateHz     *int            `json:"sample_rate_hz"`
	QuantUnit        *int            `json:"quant_unit"`
	SamplesPerPacket *int            `json:"samples_per_packet"`
	FrameGrid        *int            `json:"frame_grid"`
	Channels         []channelCapDTO `json:"channels"`
}

type channelCapDTO struct {
	Channel          *int             `json:"channel"`
	Templates        []templateDTO    `json:"templates"`
	MinEventInterval *int             `json:"min_event_interval"`
	TimingWindow     *timingWindowDTO `json:"timing_window"`
}

type templateDTO struct {
	Name     *string            `json:"name"`
	Duration *int               `json:"duration"`
	Points   []envelopePointDTO `json:"points"`
}

type envelopePointDTO struct {
	At    *int `json:"at"`
	Level *int `json:"level"`
}

type timingWindowDTO struct {
	MinOffsetUS *int64 `json:"min_offset_us"`
	MaxOffsetUS *int64 `json:"max_offset_us"`
}

type eventDTO struct {
	ID          *string          `json:"id"`
	Frame       *int             `json:"frame"`
	FrameIndex  *int             `json:"frame_index"`
	Type        *synth.EventType `json:"type"`
	Channel     *int             `json:"channel"`
	Strength    *int             `json:"strength"`
	Priority    *int             `json:"priority"`
	NoInterrupt *bool            `json:"no_interrupt"`
	Template    *string          `json:"template"`
}

// LoadManifest 加载并校验设备能力清单。
func LoadManifest(path string) (*synth.Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &BizError{Code: EStorage, Message: "读取能力清单失败", Detail: err.Error()}
	}
	var dto manifestDTO
	if err := json.Unmarshal(raw, &dto); err != nil {
		return nil, &BizError{Code: EPara, Message: "能力清单 JSON 非法", Field: "manifest", Detail: err.Error()}
	}
	if err := requireStr(dto.ManifestVersion, "manifest_version", -1, -1); err != nil {
		return nil, err
	}
	if err := requireInt(dto.ChannelCount, "channel_count", -1, -1); err != nil {
		return nil, err
	}
	if err := requirePositive(dto.SampleRateHz, "sample_rate_hz", -1); err != nil {
		return nil, err
	}
	if err := requirePositive(dto.QuantUnit, "quant_unit", -1); err != nil {
		return nil, err
	}
	if err := requirePositive(dto.SamplesPerPacket, "samples_per_packet", -1); err != nil {
		return nil, err
	}
	if err := requirePositive(dto.FrameGrid, "frame_grid", -1); err != nil {
		return nil, err
	}
	if len(dto.Channels) == 0 {
		return nil, NewBiz(EPara, "通道能力列表为空", "channels", -1, -1)
	}
	if len(dto.Channels) != *dto.ChannelCount {
		return nil, NewBiz(EPara,
			fmt.Sprintf("通道数声明 %d 与实际 %d 不符", *dto.ChannelCount, len(dto.Channels)),
			"channel_count", -1, -1)
	}

	m := &synth.Manifest{
		ManifestVersion:  *dto.ManifestVersion,
		ChannelCount:     *dto.ChannelCount,
		SampleRateHz:     *dto.SampleRateHz,
		QuantUnit:        *dto.QuantUnit,
		SamplesPerPacket: *dto.SamplesPerPacket,
		FrameGrid:        *dto.FrameGrid,
		Channels:         make([]synth.ChannelCap, len(dto.Channels)),
	}
	for ci, cdto := range dto.Channels {
		if err := requireInt(cdto.Channel, "channel", ci, -1); err != nil {
			return nil, err
		}
		if *cdto.Channel != ci {
			return nil, NewBiz(EPara, fmt.Sprintf("通道下标 %d 与声明 channel=%d 不符", ci, *cdto.Channel),
				"channel", ci, -1)
		}
		if err := requireNonNegative(cdto.MinEventInterval, "min_event_interval", ci); err != nil {
			return nil, err
		}
		if cdto.TimingWindow == nil {
			return nil, NewBiz(EPara, "缺少包时序窗口", "timing_window", ci, -1)
		}
		if cdto.TimingWindow.MinOffsetUS == nil || cdto.TimingWindow.MaxOffsetUS == nil {
			return nil, NewBiz(EPara, "时序窗口边界缺失", "timing_window", ci, -1)
		}
		if *cdto.TimingWindow.MinOffsetUS < 0 || *cdto.TimingWindow.MaxOffsetUS < *cdto.TimingWindow.MinOffsetUS {
			return nil, NewBiz(EPara, "时序窗口边界非法", "timing_window", ci, -1)
		}
		if len(cdto.Templates) == 0 {
			return nil, NewBiz(ETpl, "通道未声明任何包络模板", "templates", ci, -1)
		}
		seen := map[string]bool{}
		capCh := synth.ChannelCap{
			Channel:     ci,
			MinEventGap: *cdto.MinEventInterval,
			TimingWindow: synth.TimingWindow{
				MinOffsetUS: *cdto.TimingWindow.MinOffsetUS,
				MaxOffsetUS: *cdto.TimingWindow.MaxOffsetUS,
			},
		}
		for _, tdto := range cdto.Templates {
			if err := requireStr(tdto.Name, "template.name", ci, -1); err != nil {
				return nil, err
			}
			if seen[*tdto.Name] {
				return nil, NewBiz(ETpl, "模板重名: "+*tdto.Name, "template.name", ci, -1)
			}
			seen[*tdto.Name] = true
			if err := requirePositive(tdto.Duration, "template.duration", ci); err != nil {
				return nil, err
			}
			// 量化单位必须为正且整除模板时长。
			if *tdto.Duration%m.QuantUnit != 0 {
				return nil, NewBiz(EPara,
					fmt.Sprintf("量化单位 %d 不能整除模板 %s 时长 %d", m.QuantUnit, *tdto.Name, *tdto.Duration),
					"quant_unit", ci, -1)
			}
			if len(tdto.Points) < 2 {
				return nil, NewBiz(ETpl, "模板折点至少需要两个", "template.points", ci, -1)
			}
			tpl := synth.Template{Name: *tdto.Name, Duration: *tdto.Duration}
			lastAt := -1
			for _, pdto := range tdto.Points {
				if pdto.At == nil || pdto.Level == nil {
					return nil, NewBiz(EPara, "模板折点字段缺失", "template.points", ci, -1)
				}
				if *pdto.At < lastAt || *pdto.At < 0 || *pdto.At > *tdto.Duration {
					return nil, NewBiz(EPara, "模板折点时间非法", "template.points.at", ci, -1)
				}
				if *pdto.Level < 0 || *pdto.Level > 1000 {
					return nil, NewBiz(EPara, "模板电平必须在 0..1000", "template.points.level", ci, -1)
				}
				tpl.Points = append(tpl.Points, synth.EnvelopePoint{At: *pdto.At, Level: *pdto.Level})
				lastAt = *pdto.At
			}
			capCh.Templates = append(capCh.Templates, tpl)
		}
		m.Channels[ci] = capCh
	}
	return m, nil
}

// LoadEvents 加载并校验事件流（含跨字段引用校验）。
func LoadEvents(path string, m *synth.Manifest) ([]synth.Event, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &BizError{Code: EStorage, Message: "读取事件流失败", Detail: err.Error()}
	}
	var dtos []eventDTO
	if err := json.Unmarshal(raw, &dtos); err != nil {
		return nil, &BizError{Code: EPara, Message: "事件流 JSON 非法", Field: "events", Detail: err.Error()}
	}
	if len(dtos) == 0 {
		return nil, NewBiz(EPara, "事件流为空", "events", -1, -1)
	}
	events := make([]synth.Event, 0, len(dtos))
	prevFrame, prevIndex := -1, -1
	lastStartByChan := map[int]int{}
	for i, d := range dtos {
		loc := fmt.Sprintf("events[%d]", i)
		if err := requireStr(d.ID, loc+".id", -1, -1); err != nil {
			return nil, err
		}
		if err := requireNonNegative(d.Frame, loc+".frame", -1); err != nil {
			return nil, err
		}
		if err := requireNonNegative(d.FrameIndex, loc+".frame_index", -1); err != nil {
			return nil, err
		}
		if d.Type == nil {
			return nil, NewBiz(EPara, "缺少事件类型", loc+".type", -1, *d.Frame)
		}
		switch *d.Type {
		case synth.TypeHit, synth.TypeShoot, synth.TypeBlast, synth.TypeStep, synth.TypeUIFeed:
		default:
			return nil, NewBiz(EPara, "未知事件类型: "+string(*d.Type), loc+".type", -1, *d.Frame)
		}
		if err := requireNonNegative(d.Channel, loc+".channel", -1); err != nil {
			return nil, err
		}
		ch := *d.Channel
		if ch < 0 || ch >= m.ChannelCount {
			return nil, NewBiz(EChan, fmt.Sprintf("通道 %d 越界（上限 %d）", ch, m.ChannelCount-1),
				loc+".channel", ch, *d.Frame)
		}
		if err := requireStr(d.Template, loc+".template", ch, *d.Frame); err != nil {
			return nil, err
		}
		if m.Template(ch, *d.Template) == nil {
			return nil, NewBiz(ETpl, "模板引用不存在: "+*d.Template,
				loc+".template", ch, *d.Frame)
		}
		if d.Strength == nil {
			return nil, NewBiz(EPara, "缺少强度字段", loc+".strength", ch, *d.Frame)
		}
		if *d.Strength <= 0 || *d.Strength > 1000 {
			return nil, NewBiz(EPara, "强度必须在 1..1000", loc+".strength", ch, *d.Frame)
		}
		if d.Priority == nil {
			return nil, NewBiz(EPara, "缺少优先级字段", loc+".priority", ch, *d.Frame)
		}
		if *d.Priority <= 0 {
			return nil, NewBiz(EPara, "优先级必须为正", loc+".priority", ch, *d.Frame)
		}
		if d.NoInterrupt == nil {
			return nil, NewBiz(EPara, "缺少禁止打断标记", loc+".no_interrupt", ch, *d.Frame)
		}
		// 帧序与帧内序号必须单调非递减。
		if *d.Frame < prevFrame || (*d.Frame == prevFrame && *d.FrameIndex < prevIndex) {
			return nil, NewBiz(EOrder,
				fmt.Sprintf("帧序非单调: (%d,%d) 出现在 (%d,%d) 之后", *d.Frame, *d.FrameIndex, prevFrame, prevIndex),
				loc+".frame", ch, *d.Frame)
		}
		prevFrame, prevIndex = *d.Frame, *d.FrameIndex

		ev := synth.Event{
			ID: *d.ID, Frame: *d.Frame, FrameIndex: *d.FrameIndex, Type: *d.Type,
			Channel: ch, Strength: *d.Strength, Priority: *d.Priority,
			NoInterrupt: *d.NoInterrupt, Template: *d.Template,
		}
		// 最小事件间隔校验（同通道网格起始点间隔）。
		start := ev.StartGrid(m.FrameGrid)
		if last, ok := lastStartByChan[ch]; ok && start-last < m.Channels[ch].MinEventGap {
			return nil, NewBiz(EOrder,
				fmt.Sprintf("同通道事件间隔 %d 小于最小间隔 %d", start-last, m.Channels[ch].MinEventGap),
				loc+".frame", ch, *d.Frame)
		}
		lastStartByChan[ch] = start
		events = append(events, ev)
	}
	return events, nil
}

func requireStr(p *string, field string, ch, frame int) error {
	if p == nil || *p == "" {
		return NewBiz(EPara, "缺少必填字段", field, ch, frame)
	}
	return nil
}

func requireInt(p *int, field string, ch, frame int) error {
	if p == nil {
		return NewBiz(EPara, "缺少必填字段", field, ch, frame)
	}
	return nil
}

func requirePositive(p *int, field string, ch int) error {
	if p == nil {
		return NewBiz(EPara, "缺少必填字段", field, ch, -1)
	}
	if *p <= 0 {
		return NewBiz(EPara, "字段必须为正数", field, ch, -1)
	}
	return nil
}

func requireNonNegative(p *int, field string, ch int) error {
	if p == nil {
		return NewBiz(EPara, "缺少必填字段", field, ch, -1)
	}
	if *p < 0 {
		return NewBiz(EPara, "字段不能为负", field, ch, -1)
	}
	return nil
}
