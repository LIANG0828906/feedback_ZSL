package mirror

import "fmt"

// Packet is a deterministic tile of a screen's mapped region.
type Packet struct {
	Seq         int    `json:"seq"`
	WindowID    string `json:"windowId"`
	ScreenID    string `json:"screenId"`
	Region      Region `json:"region"`
	StartTick   int64  `json:"startTick"`
	EndTick     int64  `json:"endTick"`
	Bytes       int64  `json:"bytes"`
	ContentHash string `json:"contentHash"`
}

// DistributeScreen splits one mapping into tiles whose encoded byte size is
// never greater than either declared packet limit.
func DistributeScreen(screen Screen, window Window, mapping Mapping) ([]Packet, error) {
	limit := min64(window.MaxPacketBytes, screen.MaxPacketBytes)
	if limit <= 0 {
		return nil, &Error{Code: CodePacket, Path: "windows." + window.ID + ".maxPacketBytes",
			ScreenID: screen.ID, WindowID: window.ID, PacketSeq: 0, Measured: 0, Limit: limit,
			Message: "packet limit must be positive"}
	}
	num, den := window.Chroma.BytesPerPixelNum(), window.Chroma.BytesPerPixelDen()
	if num <= 0 || den <= 0 {
		return nil, &Error{Code: CodeCapacity, ScreenID: screen.ID, WindowID: window.ID,
			Requested: string(window.Chroma), Message: "unknown chroma encoding"}
	}
	maxPixels, _, err := MulDivChecked("windows."+window.ID+".packetBytes", limit, den, num, RoundFloor)
	if err != nil {
		return nil, err
	}
	if maxPixels <= 0 {
		onePixelBytes, _, _ := MulDivChecked("windows."+window.ID+".packetBytes", 1, num, den, RoundCeil)
		return nil, &Error{Code: CodePacket, Path: "windows." + window.ID + ".maxPacketBytes",
			ScreenID: screen.ID, WindowID: window.ID, PacketSeq: 0,
			Measured: onePixelBytes, Limit: limit, Message: "limit cannot hold one pixel"}
	}
	r := mapping.Output
	if r.Empty() {
		return nil, &Error{Code: CodeCapacity, ScreenID: screen.ID, WindowID: window.ID,
			Message: "cannot distribute empty mapped region"}
	}
	tileW := min64(r.W, maxPixels)
	tileH := int64(1)
	if maxPixels >= r.W {
		tileH = maxPixels / r.W
	}
	if tileH < 1 {
		tileH = 1
	}
	tileW = max64(1, min64(tileW, r.W))
	countX := (r.W + tileW - 1) / tileW
	countY := (r.H + tileH - 1) / tileH
	total := countX * countY
	packets := make([]Packet, 0, total)
	seq := 1
	for y := int64(0); y < r.H; y += tileH {
		h := min64(tileH, r.H-y)
		for x := int64(0); x < r.W; x += tileW {
			w := min64(tileW, r.W-x)
			area := w * h
			packetBytes, _, err := MulDivChecked("windows."+window.ID+".packetBytes", area, num, den, RoundCeil)
			if err != nil {
				return nil, err
			}
			if packetBytes > limit {
				return nil, &Error{Code: CodePacket, Path: "windows." + window.ID + ".packetBytes",
					ScreenID: screen.ID, WindowID: window.ID, Field: "bytes",
					PacketSeq: seq, Measured: packetBytes, Limit: limit,
					Message: "packet exceeds size limit"}
			}
			start, err := packetTick("packets["+fmt.Sprint(seq)+"].startTick", window.StartTick, window.EndTick, window.StartTick, int64(seq-1), int64(total), true)
			if err != nil {
				return nil, err
			}
			end, err := packetTick("packets["+fmt.Sprint(seq)+"].endTick", window.StartTick, window.EndTick, window.StartTick, int64(seq), int64(total), false)
			if err != nil {
				return nil, err
			}
			if start < window.StartTick || end > window.EndTick || start > end {
				return nil, &Error{Code: CodePacket, Path: "packets[" + fmt.Sprint(seq) + "].time",
					ScreenID: screen.ID, WindowID: window.ID, PacketSeq: seq,
					Measured: end - start, Limit: window.EndTick - window.StartTick,
					Message: "packet timing escapes window"}
			}
			region := Region{X: r.X + x, Y: r.Y + y, W: w, H: h}
			p := Packet{Seq: seq, WindowID: window.ID, ScreenID: screen.ID, Region: region,
				StartTick: start, EndTick: end, Bytes: packetBytes}
			p.ContentHash = HashJSON(struct {
				Window string `json:"window"`
				Screen string `json:"screen"`
				Seq    int    `json:"seq"`
				Region Region `json:"region"`
				Start  int64  `json:"start"`
				End    int64  `json:"end"`
				Bytes  int64  `json:"bytes"`
			}{window.ID, screen.ID, seq, region, start, end, packetBytes})
			packets = append(packets, p)
			seq++
		}
	}
	return packets, nil
}

func packetTick(field string, windowStart, windowEnd, base, index, count int64, numerator bool) (int64, error) {
	duration := windowEnd - windowStart
	if duration < 0 {
		return 0, &Error{Code: CodePacket, Path: field, Message: "window duration is negative"}
	}
	if count <= 0 {
		return 0, &Error{Code: CodePacket, Path: field, Message: "packet count must be positive"}
	}
	offset, _, err := MulDivChecked(field, index, duration, count, RoundFloor)
	if err != nil {
		return 0, err
	}
	tick, err := AddChecked(field, windowStart, offset)
	if err != nil {
		return 0, err
	}
	if !numerator && tick > windowEnd {
		tick = windowEnd
	}
	return tick, nil
}
