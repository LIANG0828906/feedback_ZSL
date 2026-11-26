package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"screenrelay/internal/mapping"
	"screenrelay/internal/model"
)

type Packet struct {
	WindowSeq       int               `json:"window_seq"`
	SourceWindowSeq int               `json:"source_window_seq"`
	ScreenID        string            `json:"screen_id"`
	PacketNo        int               `json:"packet_no"`
	Index           int               `json:"index"`
	Replay          bool              `json:"replay"`
	Coverage        mapping.PixelRect `json:"coverage"`
	ByteCount       int64             `json:"byte_count"`
	SHA256          string            `json:"sha256"`
}

type ScreenDispatch struct {
	ScreenID    string   `json:"screen_id"`
	ContentHash string   `json:"content_hash"`
	Packets     []Packet `json:"packets"`
}

func HashCanonical(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", model.NewError(model.EInternal, "canonical marshal: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func SplitPackets(windowSeq int, screenID string, rect mapping.PixelRect, maxPacketBytes, contentSeed int64) ([]Packet, string, error) {
	return SplitPacketsBudgeted(windowSeq, screenID, rect, maxPacketBytes, contentSeed, maxPacketBytes/model.PixelBytes, nil)
}

func SplitPacketsBudgeted(windowSeq int, screenID string, rect mapping.PixelRect, maxPacketBytes, contentSeed, bufferPixels int64, buffer []byte) ([]Packet, string, error) {
	if rect.W <= 0 || rect.H <= 0 {
		return nil, "", model.NewError(model.EPara, "cannot split empty packet region for %s", screenID)
	}
	if maxPacketBytes < model.PixelBytes {
		return nil, "", model.NewError(model.EPara, "max packet bytes %d is smaller than %d bytes/pixel", maxPacketBytes, model.PixelBytes)
	}
	maxPixels := maxPacketBytes / model.PixelBytes
	chunkPixels := maxPixels
	if bufferPixels > 0 && bufferPixels < chunkPixels {
		chunkPixels = bufferPixels
	}
	if chunkPixels < 1 {
		return nil, "", model.NewError(model.EBuffer, "pixel buffer budget %d cannot hold one pixel", bufferPixels)
	}
	if buffer != nil && int64(len(buffer)) < chunkPixels*model.PixelBytes {
		chunkPixels = int64(len(buffer)) / model.PixelBytes
		if chunkPixels < 1 {
			return nil, "", model.NewError(model.EBuffer, "pixel buffer length %d cannot hold one pixel", len(buffer))
		}
	}
	tileW := min64(rect.W, chunkPixels)
	tileH := maxInt64(1, chunkPixels/tileW)
	packets := []Packet{}
	index := 0
	for y := int64(0); y < rect.H; y += tileH {
		h := min64(tileH, rect.H-y)
		for x := int64(0); x < rect.W; x += tileW {
			w := min64(tileW, rect.W-x)
			coverage := mapping.PixelRect{X: rect.X + x, Y: rect.Y + y, W: w, H: h}
			byteCount := w * h * model.PixelBytes
			digest := packetHashBuffered(windowSeq, screenID, index+1, coverage, byteCount, contentSeed, buffer)
			packets = append(packets, Packet{
				WindowSeq: windowSeq, SourceWindowSeq: windowSeq, ScreenID: screenID,
				PacketNo: index + 1, Index: index, Coverage: coverage, ByteCount: byteCount, SHA256: digest,
			})
			index++
		}
	}
	contentHash, err := HashCanonical(map[string]any{
		"window_seq": windowSeq, "screen_id": screenID, "rect": rect,
		"content_seed": contentSeed, "packets": packets,
	})
	if err != nil {
		return nil, "", err
	}
	return packets, contentHash, nil
}

func packetHashBuffered(windowSeq int, screenID string, no int, rect mapping.PixelRect, byteCount, seed int64, buffer []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "screenrelay-packet/v1-buffered\n")
	fmt.Fprintf(h, "window=%d screen=%s no=%d\n", windowSeq, screenID, no)
	fmt.Fprintf(h, "x=%d y=%d w=%d h=%d bytes=%d seed=%d\n", rect.X, rect.Y, rect.W, rect.H, byteCount, seed)
	if buffer != nil && byteCount <= int64(len(buffer)) {
		chunk := buffer[:byteCount]
		for i := int64(0); i < byteCount; i++ {
			chunk[i] = byte(seed + i + rect.X*17 + rect.Y*31)
		}
		h.Write(chunk)
	} else {
		for i := int64(0); i < rect.W*rect.H; i++ {
			var pixel [4]byte
			pixel[0] = byte(seed + i)
			pixel[1] = byte((seed >> 8) + i/rect.W)
			pixel[2] = byte((seed >> 16) + i/rect.H)
			pixel[3] = 0xff
			h.Write(pixel[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ReplayPackets(targetWindowSeq int, sourceWindowSeq int, screenID string, rect mapping.PixelRect, maxPacketBytes, contentSeed int64) ([]Packet, error) {
	sourcePackets, _, err := SplitPackets(sourceWindowSeq, screenID, rect, maxPacketBytes, contentSeed)
	if err != nil {
		return nil, err
	}
	for i := range sourcePackets {
		sourcePackets[i].WindowSeq = targetWindowSeq
		sourcePackets[i].Replay = true
		sourcePackets[i].SHA256 = packetHash(targetWindowSeq, sourceWindowSeq, screenID, i+1, sourcePackets[i].Coverage, sourcePackets[i].ByteCount, contentSeed, true)
	}
	return sourcePackets, nil
}

func packetHash(windowSeq, sourceWindowSeq int, screenID string, no int, rect mapping.PixelRect, byteCount, seed int64, replay bool) string {
	h := sha256.New()
	fmt.Fprintf(h, "screenrelay-packet/v1\n")
	fmt.Fprintf(h, "window=%d source=%d screen=%s no=%d replay=%t\n", windowSeq, sourceWindowSeq, screenID, no, replay)
	fmt.Fprintf(h, "x=%d y=%d w=%d h=%d bytes=%d seed=%d\n", rect.X, rect.Y, rect.W, rect.H, byteCount, seed)
	for i := int64(0); i < rect.W*rect.H; i++ {
		var pixel [8]byte
		pixel[0] = byte(seed + i)
		pixel[1] = byte((seed >> 8) + i/rect.W)
		pixel[2] = byte((seed >> 16) + i/rect.H)
		pixel[3] = 0xff
		h.Write(pixel[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
