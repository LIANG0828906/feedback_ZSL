package device

import (
	"encoding/binary"
	"encoding/json"
	"io"
)

// 协议：4 字节大端长度前缀 + JSON 载荷；固件字节以 base64 内嵌。
// 请求由组件下发，响应由模拟设备回显。

// Fault 枚举模拟设备支持注入的四类异常（业务要求 8）。
type Fault string

const (
	FaultNone      Fault = ""
	FaultVersion   Fault = "firmware_version_reject" // -> E_FW
	FaultWindow    Fault = "timing_window_violation" // -> E_WIN
	FaultMap       Fault = "channel_map_conflict"    // -> E_MAP
	FaultSegment   Fault = "segment_missing"         // -> E_SEG
	FaultEchoDrift Fault = "echo_drift"              // 回显漂移，客户端判定 E_ECHO
)

// Request 是下发给模拟设备的请求。
type Request struct {
	Magic        string    `json:"magic"` // 固定 REQ1
	Fault        Fault     `json:"fault"`
	DeclaredVer  uint16    `json:"declared_version"`
	ToleranceDB  float64   `json:"tolerance_db"`
	GridHz       []float64 `json:"grid_hz"`
	SampleRateHz int       `json:"sample_rate_hz"`
	FirmwareB64  string    `json:"firmware_b64"`
	SegmentIndex int       `json:"segment_index"` // 分段下发时的段号，-1 表示整包
}

// Response 是模拟设备的回显。
type Response struct {
	OK         bool              `json:"ok"`
	Code       string            `json:"code,omitempty"`
	Message    string            `json:"message,omitempty"`
	Field      string            `json:"field,omitempty"`
	ChannelID  int               `json:"channel_id,omitempty"`
	SegmentID  int               `json:"segment_id,omitempty"`
	AppliedDB  map[int][]float64 `json:"applied_db,omitempty"` // 设备实际施加频响（按通道）
	ChecksumOK bool              `json:"checksum_ok"`
}

// WriteFrame 写长度前缀 JSON 帧。
func WriteFrame(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// ReadFrame 读长度前缀 JSON 帧。
func ReadFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}
