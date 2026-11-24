package pipeline

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const windowMagic = "VSPWINDOW"

type WindowResult struct {
	Window         int            `json:"window"`
	StartFrame     int            `json:"start_frame"`
	EndFrame       int            `json:"end_frame"`
	Frames         []*Frame       `json:"-"`
	FrameSummaries []FrameSummary `json:"frame_summaries"`
	ContentHash    string         `json:"content_hash"`
	FileHash       string         `json:"file_hash,omitempty"`
}

func CompositeWindow(ctx context.Context, model *Model, window Window, cfg CompositeConfig) (*WindowResult, error) {
	if window.EndFrame <= window.StartFrame {
		return nil, paraError(window.Index, "window has an empty frame range")
	}
	result := &WindowResult{Window: window.Index, StartFrame: window.StartFrame, EndFrame: window.EndFrame}
	for frame := window.StartFrame; frame < window.EndFrame; frame++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		composed, err := CompositeFrame(model, frame, cfg)
		if err != nil {
			return nil, err
		}
		result.Frames = append(result.Frames, composed)
		result.FrameSummaries = append(result.FrameSummaries, SummarizeFrame(composed))
	}
	raw := EncodeWindow(result)
	sum := sha256.Sum256(raw)
	result.ContentHash = hex.EncodeToString(sum[:])
	return result, nil
}

func EncodeWindow(result *WindowResult) []byte {
	var buf bytes.Buffer
	buf.WriteString(windowMagic)
	buf.WriteByte(1)
	_ = binary.Write(&buf, binary.LittleEndian, int32(result.Window))
	_ = binary.Write(&buf, binary.LittleEndian, int32(result.StartFrame))
	_ = binary.Write(&buf, binary.LittleEndian, int32(result.EndFrame))
	_ = binary.Write(&buf, binary.LittleEndian, int32(len(result.Frames)))
	for _, frame := range result.Frames {
		data := EncodeFrame(frame)
		_ = binary.Write(&buf, binary.LittleEndian, int64(len(data)))
		buf.Write(data)
	}
	return buf.Bytes()
}

func DecodeWindow(data []byte) (*WindowResult, error) {
	reader := bytes.NewReader(data)
	magic := make([]byte, len(windowMagic))
	if _, err := reader.Read(magic); err != nil || string(magic) != windowMagic {
		return nil, errCode(CodeDecode, "invalid window packet magic")
	}
	version, err := reader.ReadByte()
	if err != nil || version != 1 {
		return nil, errCode(CodeDecode, "unsupported window packet version")
	}
	var window, start, end, count int32
	for _, target := range []*int32{&window, &start, &end, &count} {
		if err := binary.Read(reader, binary.LittleEndian, target); err != nil {
			return nil, errCode(CodeDecode, "truncated window header")
		}
	}
	if count < 0 || count > 100000 {
		return nil, errCode(CodeDecode, "invalid window frame count")
	}
	result := &WindowResult{
		Window: int(window), StartFrame: int(start), EndFrame: int(end),
		Frames: make([]*Frame, 0, count),
	}
	for i := 0; i < int(count); i++ {
		var size int64
		if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
			return nil, errCode(CodeDecode, "truncated window frame length")
		}
		if size < 0 || size > int64(reader.Len()) {
			return nil, errCode(CodeDecode, "invalid window frame length")
		}
		payload := make([]byte, size)
		if _, err := reader.Read(payload); err != nil {
			return nil, errCode(CodeDecode, "truncated window frame payload")
		}
		frame, err := DecodeFrame(payload)
		if err != nil {
			return nil, err
		}
		result.Frames = append(result.Frames, frame)
		result.FrameSummaries = append(result.FrameSummaries, SummarizeFrame(frame))
	}
	if reader.Len() != 0 {
		return nil, errCode(CodeDecode, "trailing bytes in window packet")
	}
	if len(result.Frames) != result.EndFrame-result.StartFrame {
		return nil, errCode(CodeDecode, "window frame count does not match range")
	}
	sum := sha256.Sum256(data)
	result.ContentHash = hex.EncodeToString(sum[:])
	return result, nil
}

func (s FrameStore) WindowPath(window int) string {
	return filepath.Join(s.Dir, fmt.Sprintf("window-%05d.vspw", window))
}

func (s FrameStore) WriteWindow(result *WindowResult) (string, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", errCode(CodeDecode, "create frame store: %v", err)
	}
	data := EncodeWindow(result)
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		writer.Close()
		return "", errCode(CodeDecode, "compress window packet: %v", err)
	}
	if err := writer.Close(); err != nil {
		return "", errCode(CodeDecode, "finish window packet compression: %v", err)
	}
	fileSum := sha256.Sum256(compressed.Bytes())
	result.FileHash = hex.EncodeToString(fileSum[:])
	path := s.WindowPath(result.Window)
	tmp, err := os.CreateTemp(s.Dir, ".window-*.tmp")
	if err != nil {
		return "", errCode(CodeDecode, "create frame temp file: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(compressed.Bytes()); err != nil {
		tmp.Close()
		return "", errCode(CodeDecode, "write window packet: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", errCode(CodeDecode, "sync window packet: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return "", errCode(CodeDecode, "close window packet: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", errCode(CodeDecode, "commit window packet: %v", err)
	}
	return path, nil
}

func (s FrameStore) ReadWindow(window int) (*WindowResult, error) {
	compressed, err := os.ReadFile(s.WindowPath(window))
	if err != nil {
		return nil, errCode(CodeDecode, "read cached window: %v", err)
	}
	reader, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, errCode(CodeDecode, "open cached window: %v", err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, 512<<20))
	if err != nil {
		return nil, errCode(CodeDecode, "decompress cached window: %v", err)
	}
	result, err := DecodeWindow(raw)
	if err != nil {
		return nil, err
	}
	fileSum := sha256.Sum256(compressed)
	result.FileHash = hex.EncodeToString(fileSum[:])
	return result, nil
}
