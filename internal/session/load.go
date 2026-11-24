package session

import (
	"fmt"
	"path/filepath"
)

// Channel 是装载后的运行时通道：规格 + 解码后的采样与内容哈希。
type Channel struct {
	Spec ChannelSpec
	Data []float64
	Hash string
}

// Session 是装载并校验通过后的完整会话。
type Session struct {
	Manifest *Manifest
	Channels []*Channel
	RefIndex int
	dir      string
}

// Dir 返回会话所在工作目录。
func (s *Session) Dir() string { return s.dir }

// RequiredNonEmpty 列出清单中不允许缺失的标量字段。
func requiredFields() []string {
	return []string{
		"session_id", "sample_rate", "channels", "duration_sec",
		"points_per_chan", "core_points", "margin", "marker_shape",
		"marker_width", "residual_bound", "raw_encoding",
	}
}

// Load 读取并完整校验会话目录。任何元数据问题返回 E_META（带通道与字段），
// 数据不可解码返回 E_DECODE；处理过程不读取真值反推采样。
func Load(dir string) (*Session, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range requiredFields() {
		if err := requireField(m, f); err != nil {
			return nil, err
		}
	}
	if m.RawEncoding != "float64le" {
		return nil, NewError(EMeta, "unsupported raw encoding").With("", "raw_encoding")
	}
	if len(m.Channels_) == 0 {
		return nil, NewError(EMeta, "no channel specs present").With("", "channel_specs")
	}
	if m.Channels != len(m.Channels_) {
		return nil, NewError(EMeta,
			fmt.Sprintf("channel count mismatch: declared %d, specs %d", m.Channels, len(m.Channels_))).
			With("", "channels")
	}

	seen := map[string]bool{}
	refCount := 0
	sess := &Session{Manifest: m, RefIndex: -1, dir: dir}
	for i := range m.Channels_ {
		spec := &m.Channels_[i]
		if spec.ID == "" {
			return nil, NewError(EMeta, "channel id missing").With(fmt.Sprintf("#%d", i), "id")
		}
		if seen[spec.ID] {
			return nil, NewError(EMeta, "duplicate channel id").With(spec.ID, "id")
		}
		seen[spec.ID] = true
		if spec.File == "" {
			return nil, NewError(EMeta, "raw file field missing").With(spec.ID, "file")
		}
		if m.SampleRate <= 0 {
			return nil, NewError(EMeta, "sample rate must be positive").With(spec.ID, "sample_rate")
		}
		if spec.Points != m.PointsPerChan {
			return nil, NewError(EMeta, fmt.Sprintf(
				"point count mismatch: channel %d, manifest %d", spec.Points, m.PointsPerChan)).
				With(spec.ID, "points")
		}
		if spec.IsRef {
			refCount++
			sess.RefIndex = i
		}
		data, hash, derr := ReadRaw(filepath.Join(dir, spec.File), spec.Points)
		if derr != nil {
			return nil, derr.(*Error).With(spec.ID, "file")
		}
		sess.Channels = append(sess.Channels, &Channel{Spec: *spec, Data: data, Hash: hash})
	}
	if refCount != 1 {
		return nil, NewError(EMeta,
			fmt.Sprintf("exactly one reference channel required, got %d", refCount)).
			With("", "is_ref")
	}
	if sess.RefIndex < 0 {
		return nil, NewError(EMeta, "no reference channel marked").With("", "is_ref")
	}
	return sess, nil
}

func requireField(m *Manifest, field string) error {
	var bad bool
	switch field {
	case "session_id":
		bad = m.SessionID == ""
	case "sample_rate":
		bad = m.SampleRate <= 0
	case "channels":
		bad = m.Channels <= 0
	case "duration_sec":
		bad = m.DurationSec <= 0
	case "points_per_chan":
		bad = m.PointsPerChan <= 0
	case "core_points":
		bad = m.CorePoints <= 0
	case "margin":
		bad = m.Margin < 0
	case "marker_shape":
		bad = m.MarkerShape == ""
	case "marker_width":
		bad = m.MarkerWidth <= 0
	case "residual_bound":
		bad = m.ResidualBound <= 0
	case "raw_encoding":
		bad = m.RawEncoding == ""
	}
	if bad {
		return NewError(EMeta, "required field missing or invalid").With("", field)
	}
	return nil
}

// VerifyChannelPresent 用于故障注入链路：期望通道在磁盘上存在，否则 E_CHAN。
func VerifyChannelPresent(dir string, spec ChannelSpec) error {
	path := filepath.Join(dir, spec.File)
	data, err := osReadFileLen(path)
	if err != nil {
		return NewError(EChan, "channel raw data missing: "+err.Error()).With(spec.ID, "file")
	}
	if int64(data) != RawFileSize(spec.Points) {
		return NewError(EChan, "channel raw data truncated").With(spec.ID, "file")
	}
	return nil
}
