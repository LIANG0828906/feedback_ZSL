// Package audio contains the deterministic PCM representation, WAV codec and
// content hashing used by every processing path.
package audio

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"

	"loudnessworkbench/apperr"
)

// Signal stores interleaved samples as float64. The canonical exported format
// is IEEE-754 float32, so all paths quantize at the limiter output boundary.
type Signal struct {
	SampleRateHz int
	Channels     int
	Data         []float64
}

func New(sampleRate, channels, frames int) Signal {
	return Signal{SampleRateHz: sampleRate, Channels: channels, Data: make([]float64, frames*channels)}
}

func (s Signal) Frames() int {
	if s.Channels <= 0 {
		return 0
	}
	return len(s.Data) / s.Channels
}

func (s Signal) Clone() Signal {
	out := s
	out.Data = append([]float64(nil), s.Data...)
	return out
}

func (s Signal) At(frame, channel int) float64 {
	return s.Data[frame*s.Channels+channel]
}

func (s Signal) Set(frame, channel int, value float64) {
	s.Data[frame*s.Channels+channel] = value
}

func (s Signal) Validate() error {
	if s.SampleRateHz <= 0 {
		return apperr.New(apperr.CodeParameter, "sample rate must be positive").WithField("sample_rate_hz").WithActual(s.SampleRateHz)
	}
	if s.Channels != 1 && s.Channels != 2 {
		return apperr.New(apperr.CodeParameter, "only mono and stereo signals are supported").WithField("channels").WithActual(s.Channels)
	}
	if len(s.Data)%s.Channels != 0 {
		return apperr.New(apperr.CodeDecode, "interleaved sample count is not frame aligned").WithField("data")
	}
	for i, v := range s.Data {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return apperr.New(apperr.CodeDecode, "signal contains a non-finite sample").WithField(fmt.Sprintf("data[%d]", i)).WithActual(v)
		}
	}
	return nil
}

// QuantizeF32 returns the exact values that survive a float32 WAV round trip.
func QuantizeF32(s Signal) Signal {
	out := s.Clone()
	for i, v := range out.Data {
		out.Data[i] = float64(float32(v))
	}
	return out
}

func Scale(s Signal, gain float64) Signal {
	out := s.Clone()
	for i := range out.Data {
		out.Data[i] *= gain
	}
	return out
}

func MaxAbs(s Signal) float64 {
	maxValue := 0.0
	for _, v := range s.Data {
		a := math.Abs(v)
		if a > maxValue {
			maxValue = a
		}
	}
	return maxValue
}

// ContentHash hashes decoded sample content, not container bytes. This makes
// references to the same samples equivalent across different WAV encodings.
func ContentHash(s Signal) string {
	h := sha256.New()
	header := fmt.Sprintf("lw-audio-content-v1\nrate=%d\nchannels=%d\nframes=%d\nformat=f64le\n", s.SampleRateHz, s.Channels, s.Frames())
	_, _ = h.Write([]byte(header))
	var raw [8]byte
	for _, v := range s.Data {
		binary.LittleEndian.PutUint64(raw[:], math.Float64bits(v))
		_, _ = h.Write(raw[:])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Concat concatenates segments in order. If any input is stereo the result is
// stereo and mono inputs are duplicated into both channels.
func Concat(segments []Signal) (Signal, error) {
	if len(segments) == 0 {
		return Signal{}, apperr.New(apperr.CodeParameter, "cannot concatenate zero segments").WithField("segments")
	}
	sampleRate := segments[0].SampleRateHz
	channels := 1
	totalFrames := 0
	for i, s := range segments {
		if s.SampleRateHz != sampleRate {
			return Signal{}, apperr.New(apperr.CodeSchema, "segment sample rates differ").
				WithField(fmt.Sprintf("segments[%d].sample_rate_hz", i)).
				WithExpected(sampleRate).WithActual(s.SampleRateHz)
		}
		if s.Channels == 2 {
			channels = 2
		}
		totalFrames += s.Frames()
	}
	out := New(sampleRate, channels, totalFrames)
	cursor := 0
	for _, s := range segments {
		frames := s.Frames()
		if channels == 1 {
			copy(out.Data[cursor:cursor+frames], s.Data)
		} else if s.Channels == 2 {
			copy(out.Data[cursor*2:(cursor+frames)*2], s.Data)
		} else {
			for i := 0; i < frames; i++ {
				v := s.Data[i]
				out.Data[(cursor+i)*2] = v
				out.Data[(cursor+i)*2+1] = v
			}
		}
		cursor += frames
	}
	return out, nil
}
