package forensic

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/fnv"
	"strings"
)

func BlockContentHash(img *Image, chunk Chunk, algorithm string) (string, error) {
	h, err := newHasher(algorithm)
	if err != nil {
		return "", err
	}
	var header [12]byte
	putInt(header[0:4], chunk.Width())
	putInt(header[4:8], chunk.Height())
	putInt(header[8:12], img.Channels)
	_, _ = h.Write(header[:])
	_, _ = h.Write(img.BlockBytes(chunk))
	return hex.EncodeToString(h.Sum(nil)), nil
}

func newHasher(algorithm string) (hash.Hash, error) {
	switch algorithm {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	case "sha1":
		return sha1.New(), nil
	case "fnv64a":
		return fnv.New64a(), nil
	default:
		return nil, ParamError("hashAlgorithm", "unsupported hash algorithm")
	}
}

// Fingerprint computes a content-only quantized luma grid. The grid and the
// nibble quantization are the only parameters, so thread and scheduling state
// cannot influence the result.
func Fingerprint(img *Image, chunk Chunk, grid int) (string, error) {
	if grid < 1 || grid > 32 {
		return "", ParamError("fingerprintGrid", "must be within [1,32]")
	}
	values := make([]float64, grid*grid)
	counts := make([]int, grid*grid)
	for y := chunk.Y0; y < chunk.Y1; y++ {
		for x := chunk.X0; x < chunk.X1; x++ {
			cellX := (x - chunk.X0) * grid / chunk.Width()
			cellY := (y - chunk.Y0) * grid / chunk.Height()
			if cellX >= grid {
				cellX = grid - 1
			}
			if cellY >= grid {
				cellY = grid - 1
			}
			cell := cellY*grid + cellX
			values[cell] += float64(luma(img, x, y))
			counts[cell]++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "g%d:", grid)
	for i := range values {
		if counts[i] == 0 {
			b.WriteString("00")
			continue
		}
		avg := values[i] / float64(counts[i])
		q := int(avg/16.0 + 0.5)
		if q > 15 {
			q = 15
		}
		fmt.Fprintf(&b, "%02x", q)
	}
	return b.String(), nil
}

func luma(img *Image, x, y int) uint8 {
	i := (y*img.Width + x) * img.Channels
	if img.Channels == 1 {
		return img.Pix[i]
	}
	r := int(img.Pix[i])
	g := int(img.Pix[i+1])
	b := int(img.Pix[i+2])
	return uint8((299*r + 587*g + 114*b + 500) / 1000)
}

// VerifyContentHashes compares current block content digests with a persisted
// snapshot and reports the first mismatch in block-index order.
func VerifyContentHashes(img *Image, chunks []Chunk, expected map[int]string) error {
	for _, chunk := range chunks {
		want, ok := expected[chunk.Index]
		if !ok {
			continue
		}
		got := img.BlockDigest(chunk)
		if got != want {
			return &Error{
				Code: CodeHash, ImageID: img.ID, Block: chunk.Index,
				X: chunk.X0, Y: chunk.Y0, Stage: StageHash,
				Message: "content hash changed for the first block",
			}
		}
	}
	return nil
}
