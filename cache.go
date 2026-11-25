package dialogue

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// CacheKey contains the mandated inputs. ParamsVersion is an additional
// deterministic salt for calibration parameters such as gain and target.
type CacheKey struct {
	Kind          string `json:"kind"`
	ContentHash   string `json:"content_hash"`
	RangeID       string `json:"range_id"`
	QuantVersion  string `json:"quant_version"`
	UpstreamKey   string `json:"upstream_key"`
	ParamsVersion string `json:"params_version,omitempty"`
	AlgorithmVer  string `json:"algorithm_version,omitempty"`
}

func (k CacheKey) String() string {
	b, _ := json.Marshal(k)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func shortKey(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12]
}

// AudioHash hashes sample rate, timing and integer PCM samples. Gain and
// calibration parameters are deliberately excluded.
func AudioHash(sampleRate int, startSample int64, samples []int16) string {
	h := sha256.New()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(sampleRate))
	h.Write(buf[:])
	binary.LittleEndian.PutUint64(buf[:], uint64(startSample))
	h.Write(buf[:])
	for _, v := range samples {
		binary.LittleEndian.PutUint16(buf[:2], uint16(v))
		h.Write(buf[:2])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func quantVersion(project *Project) string {
	return fmt.Sprintf("%s/fps=%d/sr=%d", project.QuantVersion, project.FrameRate, project.SampleRate)
}

func calibrationParamsVersion(project *Project, clip *Clip) string {
	return fmt.Sprintf("calib-int-v1/session=%d/target=%d/maxadj=%d/clipgain=%d",
		project.SessionGainBP, project.TargetAmpMicro, project.MaxAdjustBP, clip.GainBP)
}

func decodeCacheKey(clip *Clip) CacheKey {
	return CacheKey{
		Kind:         "decode",
		ContentHash:  clip.AudioHash,
		RangeID:      "pcm",
		QuantVersion: "none",
	}
}

func featureCacheKey(project *Project, clip *Clip, interval FrameInterval) CacheKey {
	return CacheKey{
		Kind:         "feature",
		ContentHash:  clip.AudioHash,
		RangeID:      interval.RangeID,
		QuantVersion: project.QuantVersion,
		UpstreamKey:  decodeCacheKey(clip).String(),
		AlgorithmVer: AlgorithmVersion,
	}
}

func calibrationCacheKey(project *Project, clip *Clip, interval FrameInterval, featureKey string) CacheKey {
	return CacheKey{
		Kind:          "calibration",
		ContentHash:   clip.AudioHash,
		RangeID:       interval.RangeID,
		QuantVersion:  project.QuantVersion,
		UpstreamKey:   featureKey,
		ParamsVersion: calibrationParamsVersion(project, clip),
		AlgorithmVer:  AlgorithmVersion,
	}
}
