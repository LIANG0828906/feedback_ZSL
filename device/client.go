package device

import (
	"encoding/base64"
	"net"
	"sync"

	"audiocal"
)

// Client 是组件侧的回环设备客户端；下发串行化以保证段序稳定（业务要求 8）。
type Client struct {
	mu   sync.Mutex
	addr string
}

func NewClient(addr string) *Client {
	return &Client{addr: addr}
}

// Upload 下发整包固件并返回设备回显。fault 可注入异常。
func (c *Client) Upload(fwBytes []byte, gridHz []float64, sampleRate int, toleranceDB float64, fault Fault) (*Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, err := net.Dial("tcp", c.addr)
	if err != nil {
		return nil, &audiocal.BizError{Code: audiocal.CodeStore, Message: "连接模拟设备失败: " + err.Error(), Field: "dial"}
	}
	defer conn.Close()

	req := Request{
		Magic:        reqMagic,
		Fault:        fault,
		DeclaredVer:  supportedFW,
		ToleranceDB:  toleranceDB,
		GridHz:       append([]float64(nil), gridHz...),
		SampleRateHz: sampleRate,
		FirmwareB64:  base64.StdEncoding.EncodeToString(fwBytes),
		SegmentIndex: -1,
	}
	if err := WriteFrame(conn, req); err != nil {
		return nil, err
	}
	var resp Response
	if err := ReadFrame(conn, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return &resp, mapDeviceError(resp)
	}
	return &resp, nil
}

// mapDeviceError 把设备回显码映射为组件稳定业务码。
func mapDeviceError(r Response) error {
	code := audiocal.ErrorCode(r.Code)
	switch code {
	case audiocal.CodeFW, audiocal.CodeWin, audiocal.CodeMap, audiocal.CodeSeg:
		return &audiocal.BizError{
			Code:    code,
			Message: r.Message,
			Field:   r.Field,
			Detail: map[string]any{
				"segment_id": r.SegmentID,
				"channel_id": r.ChannelID,
			},
		}
	default:
		return &audiocal.BizError{Code: audiocal.CodeFW, Message: r.Message, Field: r.Field}
	}
}
