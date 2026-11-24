package session

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

const manifestName = "session.json"
const faultsName = "faults.json"

// WriteManifest 原子写入会话清单。
func WriteManifest(dir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, manifestName), data)
}

// LoadManifest 读取会话清单。
func LoadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, NewError(EMeta, "cannot read session manifest: "+err.Error()).With("", manifestName)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, NewError(EMeta, "manifest JSON invalid: "+err.Error()).With("", manifestName)
	}
	return &m, nil
}

func writeFaults(dir string, faults []Fault) error {
	data, err := json.MarshalIndent(faults, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, faultsName), data)
}

// LoadFaults 读取注入故障记录（不存在时返回 nil）。
func LoadFaults(dir string) ([]Fault, error) {
	data, err := os.ReadFile(filepath.Join(dir, faultsName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var faults []Fault
	if err := json.Unmarshal(data, &faults); err != nil {
		return nil, err
	}
	return faults, nil
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RawFileSize 返回裸数据文件的期望字节数。
func RawFileSize(points int64) int64 { return points * 8 }

// ReadRaw 读取 float64le 裸采样，强制点数与编码可解码校验。
func ReadRaw(path string, expectPoints int64) ([]float64, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", NewError(EDecode, "raw file unreadable: "+err.Error())
	}
	if info.Size()%8 != 0 {
		return nil, "", NewError(EDecode,
			fmt.Sprintf("raw file size %d is not a multiple of 8 bytes", info.Size()))
	}
	if info.Size() != RawFileSize(expectPoints) {
		return nil, "", NewError(EDecode,
			fmt.Sprintf("raw file has %d bytes, want %d (%d points)",
				info.Size(), RawFileSize(expectPoints), expectPoints))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", NewError(EDecode, "raw file read failed: "+err.Error())
	}
	x := make([]float64, expectPoints)
	for i := range x {
		bits := binary.LittleEndian.Uint64(data[i*8:])
		v := math.Float64frombits(bits)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, "", NewError(EDecode,
				fmt.Sprintf("non-finite sample at index %d", i)).WithSample(int64(i))
		}
		x[i] = v
	}
	sum := sha256.Sum256(data)
	return x, hex.EncodeToString(sum[:]), nil
}

// HashRaw 计算一段 float64 采样的内容哈希（小端编码后 SHA-256）。
func HashRaw(x []float64) string {
	h := sha256.New()
	var buf [8]byte
	for _, v := range x {
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
