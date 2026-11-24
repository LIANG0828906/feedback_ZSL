package device

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"net"
	"sync"

	"audiocal"
)

const (
	reqMagic    = "REQ1"
	fwMagic     = "ACALFW01"
	supportedFW = uint16(1)
)

// Server 是本地回环上的模拟设备服务。
type Server struct {
	mu       sync.Mutex
	listener net.Listener
	addr     string
	closed   bool
}

// Listen 在 127.0.0.1 的指定端口（0 表示由系统分配高位端口）启动监听。
func Listen(port int) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+itoa(port))
	if err != nil {
		return nil, err
	}
	s := &Server{listener: ln, addr: ln.Addr().String()}
	go s.acceptLoop()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.addr }

// Close 停止服务。
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.listener.Close()
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	var req Request
	if err := ReadFrame(conn, &req); err != nil {
		return
	}
	resp := process(req)
	_ = WriteFrame(conn, resp)
}

// parsedFirmware 是服务端按声明格式解析出的固件视图。
type parsedFirmware struct {
	version    uint16
	sampleRate uint32
	channels   int
	segments   int
	gridCount  int
	windowUS   uint64
	paramVer   uint32
	channelMap []mappingEntry
	grid       []float64
	segs       []parsedSegment
}

type mappingEntry struct {
	logical, speaker, mic uint32
}

type parsedSegment struct {
	index, channel, coeffStart, coeffCount uint32
	startUS, endUS                         uint64
	merged                                 bool
	coeffs                                 []float64
	envelope                               []float64
	hash                                   []byte
}

func process(req Request) Response {
	if req.Magic != reqMagic {
		return Response{OK: false, Code: "E_FW", Message: "请求魔数非法", Field: "magic"}
	}
	raw, err := base64.StdEncoding.DecodeString(req.FirmwareB64)
	if err != nil {
		return Response{OK: false, Code: "E_FW", Message: "固件 base64 非法", Field: "firmware"}
	}
	pf, perr := parseFirmware(raw)
	if perr != nil {
		return *perr
	}

	// 异常注入：固件版本拒收 -> E_FW。
	if req.Fault == FaultVersion || req.DeclaredVer != supportedFW || pf.version != supportedFW {
		return Response{
			OK: false, Code: "E_FW",
			Message:   "设备固件版本被拒收",
			Field:     "version",
			SegmentID: req.SegmentIndex,
		}
	}
	// 异常注入：通道映射冲突 -> E_MAP。
	if conflict := detectMapConflict(pf); conflict >= 0 {
		return Response{
			OK: false, Code: "E_MAP",
			Message:   "通道映射冲突：同一逻辑通道被重复占用",
			Field:     "channel=" + itoa(conflict),
			ChannelID: conflict,
		}
	}
	if req.Fault == FaultMap {
		return Response{
			OK: false, Code: "E_MAP",
			Message:   "注入：通道映射冲突",
			Field:     "channel=" + itoa(int(pf.channelMap[0].logical)),
			ChannelID: int(pf.channelMap[0].logical),
		}
	}
	// 异常注入：时序窗越界 -> E_WIN。
	if req.Fault == FaultWindow {
		bad := 0
		if len(pf.segs) > 0 {
			bad = int(pf.segs[0].index)
		}
		return Response{
			OK: false, Code: "E_WIN",
			Message:   "注入：事件时序超出设备时间窗",
			Field:     "segment=" + itoa(bad),
			SegmentID: bad,
		}
	}
	// 异常注入：段缺失 -> E_SEG。
	if req.Fault == FaultSegment {
		missing := 0
		if len(pf.segs) > 1 {
			missing = int(pf.segs[1].index)
		}
		return Response{
			OK: false, Code: "E_SEG",
			Message:   "注入：固件段缺失",
			Field:     "segment=" + itoa(missing),
			SegmentID: missing,
		}
	}
	if vi := windowViolation(pf); vi >= 0 {
		return Response{
			OK: false, Code: "E_WIN",
			Message:   "段事件时序超出声明窗口",
			Field:     "segment=" + itoa(vi),
			SegmentID: vi,
		}
	}
	if miss := detectMissingSegments(pf); miss >= 0 {
		return Response{
			OK: false, Code: "E_SEG",
			Message:   "段编号不连续，存在缺失段",
			Field:     "segment=" + itoa(miss),
			SegmentID: miss,
		}
	}

	// 正常路径：重组各通道 FIR 并回显实际施加频响。
	applied := applyFirmware(pf, req.GridHz, req.Fault == FaultEchoDrift)
	return Response{
		OK:         true,
		AppliedDB:  applied,
		ChecksumOK: true,
	}
}

func parseFirmware(b []byte) (*parsedFirmware, *Response) {
	if len(b) < 64+32 {
		return nil, &Response{OK: false, Code: "E_FW", Message: "固件长度不足", Field: "length"}
	}
	if string(b[:8]) != fwMagic {
		return nil, &Response{OK: false, Code: "E_FW", Message: "固件魔数非法", Field: "magic"}
	}
	pf := &parsedFirmware{}
	p := 8
	pf.version = binary.BigEndian.Uint16(b[p:])
	p += 2
	pf.sampleRate = binary.BigEndian.Uint32(b[p:])
	p += 4
	pf.channels = int(binary.BigEndian.Uint32(b[p:]))
	p += 4
	pf.segments = int(binary.BigEndian.Uint32(b[p:]))
	p += 4
	pf.gridCount = int(binary.BigEndian.Uint32(b[p:]))
	p += 4
	pf.windowUS = binary.BigEndian.Uint64(b[p:])
	p += 8
	pf.paramVer = binary.BigEndian.Uint32(b[p:])
	p += 4
	p = 64 // 跳过定长头部剩余填充

	pf.channelMap = make([]mappingEntry, pf.channels)
	for i := 0; i < pf.channels; i++ {
		pf.channelMap[i] = mappingEntry{
			logical: binary.BigEndian.Uint32(b[p:]),
			speaker: binary.BigEndian.Uint32(b[p+4:]),
			mic:     binary.BigEndian.Uint32(b[p+8:]),
		}
		p += 12
	}
	pf.grid = make([]float64, pf.gridCount)
	for i := 0; i < pf.gridCount; i++ {
		pf.grid[i] = math.Float64frombits(binary.BigEndian.Uint64(b[p:]))
		p += 8
	}
	pf.segs = make([]parsedSegment, pf.segments)
	for i := 0; i < pf.segments; i++ {
		s := parsedSegment{}
		s.index = binary.BigEndian.Uint32(b[p:])
		s.channel = binary.BigEndian.Uint32(b[p+4:])
		s.coeffStart = binary.BigEndian.Uint32(b[p+8:])
		s.coeffCount = binary.BigEndian.Uint32(b[p+12:])
		s.startUS = binary.BigEndian.Uint64(b[p+16:])
		s.endUS = binary.BigEndian.Uint64(b[p+24:])
		s.merged = b[p+32] == 1
		p += 33
		s.coeffs = make([]float64, s.coeffCount)
		for j := range s.coeffs {
			s.coeffs[j] = math.Float64frombits(binary.BigEndian.Uint64(b[p:]))
			p += 8
		}
		// 包络长度固定等于网格频点数（与 audiocal.segmentEnvelope 对齐）。
		envCount := pf.gridCount
		s.envelope = make([]float64, envCount)
		for j := range s.envelope {
			s.envelope[j] = math.Float64frombits(binary.BigEndian.Uint64(b[p:]))
			p += 8
		}
		s.hash = append([]byte(nil), b[p:p+32]...)
		p += 32
		pf.segs[i] = s
	}
	return pf, nil
}

func detectMapConflict(pf *parsedFirmware) int {
	seen := map[uint32]bool{}
	for _, m := range pf.channelMap {
		if seen[m.logical] {
			return int(m.logical)
		}
		seen[m.logical] = true
	}
	return -1
}

func windowViolation(pf *parsedFirmware) int {
	for _, s := range pf.segs {
		if s.endUS <= s.startUS || s.endUS-s.startUS > pf.windowUS {
			return int(s.index)
		}
	}
	return -1
}

func detectMissingSegments(pf *parsedFirmware) int {
	seen := map[uint32]bool{}
	for _, s := range pf.segs {
		seen[s.index] = true
	}
	for i := 0; i < pf.segments; i++ {
		if !seen[uint32(i)] {
			return i
		}
	}
	return -1
}

// applyFirmware 重组每通道 FIR 并计算设备实际施加频响；drift=true 注入确定性回显漂移。
func applyFirmware(pf *parsedFirmware, gridHz []float64, drift bool) map[int][]float64 {
	type chanCoeffs struct {
		taps map[uint32]float64
		max  int
	}
	perChan := map[int]*chanCoeffs{}
	for _, s := range pf.segs {
		ch := int(s.channel)
		cc, ok := perChan[ch]
		if !ok {
			cc = &chanCoeffs{taps: map[uint32]float64{}}
			perChan[ch] = cc
		}
		for j, c := range s.coeffs {
			pos := s.coeffStart + uint32(j)
			cc.taps[pos] = c
			if int(pos) > cc.max {
				cc.max = int(pos)
			}
		}
	}
	out := map[int][]float64{}
	for ch, cc := range perChan {
		taps := make([]float64, cc.max+1)
		for pos, v := range cc.taps {
			taps[pos] = v
		}
		db := audiocal.FIRResponseDB(taps, int(pf.sampleRate), gridHz)
		if drift {
			for i := range db {
				db[i] += 0.5 + 0.05*float64(i) // 确定性漂移，必然超常规容差
			}
		}
		out[ch] = db
	}
	return out
}

func itoa(i int) string {
	neg := i < 0
	if neg {
		i = -i
	}
	if i == 0 {
		if neg {
			return "-0"
		}
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
