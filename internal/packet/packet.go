package packet

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"audiobus/domain"
	"audiobus/errs"
)

func BuildWindow(win int, frames []int64, events []domain.Event, templateHash string, m *domain.Manifest, branchVersion string) ([]domain.Packet, error) {
	windowStart := win * m.WindowSamples
	windowEnd := windowStart + m.WindowSamples
	if windowEnd > len(frames) {
		windowEnd = len(frames)
	}
	segment := frames[windowStart:windowEnd]
	packets := []domain.Packet{}
	capacity := m.MaxSamplesPerPack
	for offset := 0; offset < len(segment); offset += capacity {
		seq := offset / capacity
		end := offset + capacity
		if end > len(segment) {
			end = len(segment)
		}
		actual := end - offset
		start := windowStart + offset
		mark := int64(start) * 1000 / m.SampleRate
		if mark < 0 || mark > m.TimingWindowMs {
			return nil, &errs.Error{Code: errs.EPack, Window: win, Channel: channelOf(events, start), Packet: seq, Reason: "timing mark outside packet timing window", Frame: int64(start)}
		}

		validFrames := append([]int64(nil), segment[offset:end]...)
		padded := make([]int64, capacity)
		copy(padded, validFrames)
		disposition := domain.FullDisposition
		if actual < capacity {
			switch m.TailRule {
			case "pad", domain.PadDisposition:
				disposition = domain.PadDisposition
			case "truncate", domain.TruncDisposition:
				trimmed := trimTrailingZeros(validFrames)
				if len(trimmed) == 0 {
					trimmed = validFrames[:1]
				}
				validFrames = trimmed
				actual = len(validFrames)
				disposition = domain.TruncDisposition
			default:
				disposition = domain.PadDisposition
			}
		}
		hash := HashFrames(padded)
		deps := dependencies(events, start, start+actual)
		p := domain.Packet{
			Window: win, Channel: channelOf(events, start), Seq: seq, Start: start,
			Samples: actual, CapacitySamples: capacity, Disposition: disposition,
			Mark: mark, PrevOffset: int64(seq * capacity), Frames: validFrames,
			PaddedFrames: padded, EventIDs: deps, TemplateHash: templateHash,
			QuantUnit: m.QuantUnit, ContentHash: hash, ConsumedOffset: consumedOffset(events, start+actual),
			BranchVersion: branchVersion, AlgoVersion: domain.AlgoVersion,
		}
		if disposition == domain.TruncDisposition {
			p.ContentHash = HashFrames(validFrames)
		}
		if err := ValidatePacket(p, m); err != nil {
			return nil, err
		}
		packets = append(packets, p)
	}
	return packets, nil
}

func ValidatePacket(p domain.Packet, m *domain.Manifest) error {
	if p.Samples <= 0 || p.Samples > m.MaxSamplesPerPack {
		return &errs.Error{Code: errs.EPack, Window: p.Window, Channel: p.Channel, Packet: p.Seq, Reason: "packet sample count exceeds cap"}
	}
	if p.Mark < 0 || p.Mark > m.TimingWindowMs {
		return &errs.Error{Code: errs.EPack, Window: p.Window, Channel: p.Channel, Packet: p.Seq, Reason: "timing mark outside window", Frame: int64(p.Start)}
	}
	windowStart := p.Window * m.WindowSamples
	relativeStart := p.Start - windowStart
	if p.Start < 0 || relativeStart < 0 || relativeStart/p.CapacitySamples != p.Seq || relativeStart%p.CapacitySamples != 0 {
		return &errs.Error{Code: errs.EPack, Window: p.Window, Channel: p.Channel, Packet: p.Seq, Reason: "packet start/seq is not aligned to window and cap"}
	}
	return nil
}

func HashFrames(frames []int64) string {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, frames)
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

func TemplateHash(t domain.Template) string {
	b, _ := json.Marshal(t)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func dependencies(events []domain.Event, start, end int) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	for _, e := range events {
		if e.Start < int64(end) && e.Start+e.Duration > int64(start) {
			key := e.ID
			if !seen[key] {
				seen[key] = true
				out = append(out, e.ID)
			}
		}
	}
	return out
}

func consumedOffset(events []domain.Event, before int) int64 {
	var max int64
	for _, e := range events {
		if e.Start < int64(before) {
			if int64(e.SourceOrdinal+1) > max {
				max = int64(e.SourceOrdinal + 1)
			}
		}
	}
	return max
}

func channelOf(events []domain.Event, start int) int {
	for _, e := range events {
		if e.Start <= int64(start) && e.Start+e.Duration > int64(start) {
			return e.Channel
		}
	}
	if len(events) > 0 {
		return events[0].Channel
	}
	return -1
}

func trimTrailingZeros(v []int64) []int64 {
	end := len(v)
	for end > 1 && v[end-1] == 0 {
		end--
	}
	return v[:end]
}

func EncodedBytes(p domain.Packet) ([]byte, string, error) {
	frames := p.PaddedFrames
	if p.Disposition == domain.TruncDisposition {
		frames = p.Frames
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, frames); err != nil {
		return nil, "", fmt.Errorf("encode packet %d/%d: %w", p.Channel, p.Seq, err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}
