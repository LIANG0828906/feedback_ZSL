package volume

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	"volumelab/pkg/model"
)

const (
	FileMagic       = "VLMVOL01"
	FileVersion     = uint32(1)
	HeaderSize      = 96
	BytesPerVoxel   = 2
	StorageAxisFast = "fastest-to-slowest"
)

type FileHeader struct {
	Version      uint32
	AxisOrder    string
	StorageDims  [3]uint32
	PhysicalDims [3]uint32
	Spacing      [3]float64
	Slope        float64
	Intercept    float64
	LayerCount   uint32
	DataBytes    uint64
}

func EncodeHeader(h FileHeader) ([]byte, error) {
	if len(h.AxisOrder) != 3 {
		return nil, fmt.Errorf("axis order must have 3 characters")
	}
	buf := make([]byte, HeaderSize)
	copy(buf[0:8], FileMagic)
	binary.LittleEndian.PutUint32(buf[8:12], h.Version)
	copy(buf[12:15], h.AxisOrder)
	for i := 0; i < 3; i++ {
		binary.LittleEndian.PutUint32(buf[16+i*4:], h.StorageDims[i])
		binary.LittleEndian.PutUint32(buf[28+i*4:], h.PhysicalDims[i])
		binary.LittleEndian.PutUint64(buf[40+i*8:], math.Float64bits(h.Spacing[i]))
	}
	binary.LittleEndian.PutUint64(buf[64:72], math.Float64bits(h.Slope))
	binary.LittleEndian.PutUint64(buf[72:80], math.Float64bits(h.Intercept))
	binary.LittleEndian.PutUint32(buf[80:84], h.LayerCount)
	binary.LittleEndian.PutUint64(buf[84:92], h.DataBytes)
	return buf, nil
}

func DecodeHeader(data []byte) (FileHeader, error) {
	var h FileHeader
	if len(data) < HeaderSize {
		return h, model.NewError("E_DECODE", "volume file is shorter than the fixed header")
	}
	if !bytes.Equal(data[0:8], []byte(FileMagic)) {
		return h, model.NewError("E_DECODE", "volume magic does not match VLMVOL01")
	}
	h.Version = binary.LittleEndian.Uint32(data[8:12])
	h.AxisOrder = string(data[12:15])
	for i := 0; i < 3; i++ {
		h.StorageDims[i] = binary.LittleEndian.Uint32(data[16+i*4:])
		h.PhysicalDims[i] = binary.LittleEndian.Uint32(data[28+i*4:])
		h.Spacing[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[40+i*8:]))
	}
	h.Slope = math.Float64frombits(binary.LittleEndian.Uint64(data[64:72]))
	h.Intercept = math.Float64frombits(binary.LittleEndian.Uint64(data[72:80]))
	h.LayerCount = binary.LittleEndian.Uint32(data[80:84])
	h.DataBytes = binary.LittleEndian.Uint64(data[84:92])
	if h.Version != FileVersion {
		return h, &model.AppError{
			Code:    "E_DECODE",
			Message: "unsupported volume file version",
			Actual:  h.Version,
			Allowed: FileVersion,
		}
	}
	return h, nil
}

func StorageDimensions(axisOrder string, physical [3]int) ([3]int, bool) {
	var dims [3]int
	seen := [3]bool{}
	for pos := 0; pos < 3; pos++ {
		axis, ok := model.AxisFromName(string(axisOrder[pos]))
		if !ok || seen[axis] {
			return dims, false
		}
		seen[axis] = true
		dims[pos] = physical[axis]
	}
	return dims, seen[0] && seen[1] && seen[2]
}

func storageOffset(axisOrder string, storageDims [3]int, voxel [3]int) (int, bool) {
	var indices [3]int
	for pos := 0; pos < 3; pos++ {
		axis, ok := model.AxisFromName(string(axisOrder[pos]))
		if !ok {
			return 0, false
		}
		indices[pos] = voxel[axis]
	}
	offset := indices[0] + storageDims[0]*(indices[1]+storageDims[1]*indices[2])
	return offset, true
}

func StorageOffset(axisOrder string, storageDims [3]int, voxel [3]int) (int, bool) {
	return storageOffset(axisOrder, storageDims, voxel)
}

func VoxelCount(shape [3]int) (uint64, bool) {
	var count uint64 = 1
	for _, n := range shape {
		if n <= 0 {
			return 0, false
		}
		count *= uint64(n)
		if count > uint64(^uint(0)>>1)/2 {
			return 0, false
		}
	}
	return count, true
}
