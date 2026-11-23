package synth

import "sort"

// activeEvent 是生效期内的事件运行态。
type activeEvent struct {
	ev      Event
	start   int
	end     int
	dropped bool
}

// Synthesize 对单通道事件流做仲裁、叠加与定点量化，输出连续包络曲线。
// horizon 是网格总长度；结果只依赖事件内容与逻辑时间，与线程/遍历顺序无关。
func Synthesize(channel int, events []Event, m *Manifest, horizon int) *ChannelCurve {
	curve := &ChannelCurve{Channel: channel}
	actives := make([]activeEvent, 0)
	decisions := make([]EventDecision, 0)

	// 预排序：按到达（帧序、帧内序号、ID）稳定排序，保证合并结论确定。
	ordered := make([]Event, len(events))
	copy(ordered, events)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i].StartGrid(m.FrameGrid), ordered[j].StartGrid(m.FrameGrid)
		if a != b {
			return a < b
		}
		return ordered[i].ID < ordered[j].ID
	})
	eventAt := map[int][]Event{}
	for _, ev := range ordered {
		g := ev.StartGrid(m.FrameGrid)
		eventAt[g] = append(eventAt[g], ev)
	}

	for tick := 0; tick < horizon; tick++ {
		// 1) 到达事件仲裁。
		for _, ev := range eventAt[tick] {
			tpl := m.Template(channel, ev.Template)
			incoming := activeEvent{ev: ev, start: tick, end: tick + tpl.Duration}
			blocked := false
			for idx := range actives {
				owner := &actives[idx]
				if owner.dropped {
					continue
				}
				if owner.ev.NoInterrupt && ev.Priority <= owner.ev.Priority {
					// 禁止打断且优先级不高于当前生效事件：丢弃新来者。
					blocked = true
					decisions = append(decisions, EventDecision{
						EventID: ev.ID, Frame: ev.Frame, Channel: channel,
						Decision: DecisionDropped, AtGrid: tick,
						Reason: "blocked-by-nointerrupt:" + owner.ev.ID,
					})
					break
				}
			}
			if blocked {
				continue
			}
			// 高优先级（含同优先级后到）事件接管：替换仍在生效的低优先级事件。
			for idx := range actives {
				owner := &actives[idx]
				if owner.dropped {
					continue
				}
				if ev.Priority >= owner.ev.Priority {
					owner.dropped = true
					decisions = append(decisions, EventDecision{
						EventID: owner.ev.ID, Frame: owner.ev.Frame, Channel: channel,
						Decision: DecisionReplaced, AtGrid: tick,
						Reason: "replaced-by:" + ev.ID,
					})
				}
			}
			actives = append(actives, incoming)
			decisions = append(decisions, EventDecision{
				EventID: ev.ID, Frame: ev.Frame, Channel: channel,
				Decision: DecisionAccepted, AtGrid: tick, Reason: "accepted",
			})
		}

		// 2) 清理已过生效期的事件。
		kept := make([]activeEvent, 0, len(actives))
		for _, a := range actives {
			if tick < a.end && !a.dropped {
				kept = append(kept, a)
			}
		}
		actives = kept

		// 3) 逐采样点叠加与定点量化（纯整数）。
		sd := SampleDecision{Channel: channel, GridOffset: tick}
		var accum int64
		for _, a := range actives {
			tpl := m.Template(channel, a.ev.Template)
			level := templateLevelAt(tpl, tick-a.start)
			weight := a.ev.Strength
			sd.Contributors = append(sd.Contributors, Contributor{
				EventID: a.ev.ID, Template: a.ev.Template, WeightQ: weight, LevelQ: level,
			})
			accum += int64(level) * int64(weight) * int64(FixedOne) / 1_000_000
		}
		// 贡献者按事件 ID 排序，记录与遍历顺序解耦。
		sort.SliceStable(sd.Contributors, func(i, j int) bool {
			return sd.Contributors[i].EventID < sd.Contributors[j].EventID
		})
		sd.AccumFixed = accum

		// 24 位定点 -> 16 位整型输出（量化残差在定点域记录）。
		scaled := accum * int64(OutputScale) / int64(FixedOne)
		out := int(scaled)
		saturated := false
		if out > OutputMax {
			out = OutputMax
			saturated = true
		} else if out < OutputMin {
			out = OutputMin
			saturated = true
		}
		sd.Output = out
		sd.Saturated = saturated
		sd.Residual = accum - int64(out)*int64(FixedOne)/int64(OutputScale)

		curve.Frames = append(curve.Frames, Frame{Channel: channel, GridOffset: tick, Value: out})
		curve.Decisions = append(curve.Decisions, sd)
	}

	// 决策按 (网格, 事件ID) 稳定排序，输出不受 map 遍历影响。
	sort.SliceStable(decisions, func(i, j int) bool {
		if decisions[i].AtGrid != decisions[j].AtGrid {
			return decisions[i].AtGrid < decisions[j].AtGrid
		}
		return decisions[i].EventID < decisions[j].EventID
	})
	curve.Events = decisions
	return curve
}

// templateLevelAt 返回模板在相对偏移 off 处的千分比电平，整型线性插值。
func templateLevelAt(tpl *Template, off int) int {
	if off < 0 || off >= tpl.Duration {
		return 0
	}
	pts := tpl.Points
	for i := 0; i < len(pts)-1; i++ {
		p0, p1 := pts[i], pts[i+1]
		if off >= p0.At && off <= p1.At {
			if p1.At == p0.At {
				return p1.Level
			}
			d := off - p0.At
			span := p1.At - p0.At
			return p0.Level + (p1.Level-p0.Level)*d/span
		}
	}
	last := pts[len(pts)-1]
	if off >= last.At {
		return last.Level
	}
	return pts[0].Level
}

// SynthesizeAll 对全部通道做合成，返回按通道下标有序的曲线集合。
func SynthesizeAll(events []Event, m *Manifest, horizon int) []*ChannelCurve {
	byChan := make([][]Event, m.ChannelCount)
	for _, ev := range events {
		byChan[ev.Channel] = append(byChan[ev.Channel], ev)
	}
	curves := make([]*ChannelCurve, m.ChannelCount)
	for c := 0; c < m.ChannelCount; c++ {
		curves[c] = Synthesize(c, byChan[c], m, horizon)
	}
	return curves
}
