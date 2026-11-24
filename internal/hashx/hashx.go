package hashx

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"

	"gameintegrity/internal/model"
)

type Digest struct {
	Summary string
	Blocks  []model.BlockMetric
}

// DigestFrame computes deterministic per-block SHA-256 values and a frame summary.
// pixelStep is used only for candidate-priority screening; exact refinement always
// calls this function with pixelStep == 1.
func DigestFrame(pixels []byte, width, height, channels, blockSize, pixelStep int) Digest {
	if pixelStep < 1 {
		pixelStep = 1
	}
	blocks := model.AllBlocks(width, height, blockSize)
	out := Digest{Blocks: make([]model.BlockMetric, len(blocks))}
	for i, block := range blocks {
		h := sha256.New()
		writeString(h, "gameintegrity/block/v1")
		writeInt(h, width)
		writeInt(h, height)
		writeInt(h, channels)
		writeInt(h, blockSize)
		writeInt(h, block.Col)
		writeInt(h, block.Row)
		writeInt(h, pixelStep)
		for y := block.Y; y < block.Y+block.H; y += pixelStep {
			rowOff := y * width * channels
			for x := block.X; x < block.X+block.W; x += pixelStep {
				off := rowOff + x*channels
				h.Write(pixels[off : off+channels])
			}
		}
		sum := h.Sum(nil)
		out.Blocks[i] = model.BlockMetric{
			Col: block.Col, Row: block.Row, X: block.X, Y: block.Y,
			W: block.W, H: block.H,
			Hash: hex.EncodeToString(sum),
		}
	}
	out.Summary = SummarizeBlocks(width, height, channels, blockSize, out.Blocks)
	return out
}

// SummarizeBlocks is independent of caller order and worker identity. It sorts
// by block row/column and hashes the canonical coordinate + hash sequence.
func SummarizeBlocks(width, height, channels, blockSize int, blocks []model.BlockMetric) string {
	sorted := append([]model.BlockMetric(nil), blocks...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Row != sorted[j].Row {
			return sorted[i].Row < sorted[j].Row
		}
		return sorted[i].Col < sorted[j].Col
	})
	frameHash := sha256.New()
	writeString(frameHash, "gameintegrity/block-frame/v1")
	writeInt(frameHash, width)
	writeInt(frameHash, height)
	writeInt(frameHash, channels)
	writeInt(frameHash, blockSize)
	var scratch [8]byte
	for _, block := range sorted {
		binary.BigEndian.PutUint32(scratch[:4], uint32(block.Col))
		binary.BigEndian.PutUint32(scratch[4:], uint32(block.Row))
		frameHash.Write(scratch[:])
		raw, err := hex.DecodeString(block.Hash)
		if err != nil {
			frameHash.Write([]byte(block.Hash))
			continue
		}
		frameHash.Write(raw)
	}
	return hex.EncodeToString(frameHash.Sum(nil))
}

func MarkChanged(current, previous Digest) int {
	if len(current.Blocks) != len(previous.Blocks) {
		return len(current.Blocks)
	}
	count := 0
	for i := range current.Blocks {
		changed := current.Blocks[i].Hash != previous.Blocks[i].Hash
		current.Blocks[i].Changed = changed
		if changed {
			count++
		}
	}
	return count
}

func ChangedRefs(blocks []model.BlockMetric) []model.BlockRef {
	out := make([]model.BlockRef, 0, len(blocks))
	for i := range blocks {
		if blocks[i].Changed {
			out = append(out, model.BlockRef{
				Col: blocks[i].Col, Row: blocks[i].Row,
				X: blocks[i].X, Y: blocks[i].Y,
				W: blocks[i].W, H: blocks[i].H,
			})
		}
	}
	return out
}

func FrameContentHash(summary, timestamp string, index int) string {
	h := sha256.New()
	writeString(h, "gameintegrity/frame-content/v1")
	writeString(h, summary)
	writeString(h, timestamp)
	writeInt(h, index)
	return hex.EncodeToString(h.Sum(nil))
}

func SegmentSummary(frames []model.FrameResult) string {
	h := sha256.New()
	writeString(h, "gameintegrity/segment/v1")
	for i := range frames {
		writeInt(h, frames[i].Frame)
		writeString(h, frames[i].Summary)
		writeString(h, frames[i].ContentHash)
		writeString(h, frames[i].Verdict)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeString(h interface{ Write([]byte) (int, error) }, s string) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(s)))
	h.Write(b[:])
	h.Write([]byte(s))
}

func writeInt(h interface{ Write([]byte) (int, error) }, v int) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	h.Write(b[:])
}
