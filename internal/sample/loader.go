package sample

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	"iconseg/internal/core"
)

const pngSignature = "\x89PNG\r\n\x1a\n"

func pngChannels(data []byte) (int, error) {
	if len(data) < 33 || string(data[:8]) != pngSignature || string(data[12:16]) != "IHDR" {
		return 0, core.DecodeError("invalid PNG signature or missing IHDR")
	}
	if binary.BigEndian.Uint32(data[8:12]) != 13 {
		return 0, core.DecodeError("invalid PNG IHDR length")
	}
	switch data[25] {
	case 0:
		return 1, nil
	case 2:
		return 3, nil
	case 3:
		return 1, nil
	case 4:
		return 2, nil
	case 6:
		return 4, nil
	default:
		return 0, core.DecodeError("unsupported PNG color type")
	}
}

// LoadDataset validates the manifest, PNG container, dimensions, channel
// count, and the embedded pixel-level reference mask.
func LoadDataset(dir string) (core.Icon, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return core.Icon{}, core.DecodeError("cannot read manifest: " + err.Error())
	}
	var manifest core.TruthManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return core.Icon{}, core.DecodeError("cannot parse manifest: " + err.Error())
	}
	if err := core.ValidateManifest(manifest); err != nil {
		return core.Icon{}, err
	}
	imagePath := filepath.Join(dir, manifest.File)
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return core.Icon{}, core.DecodeError("cannot read icon image: " + err.Error())
	}
	channels, err := pngChannels(data)
	if err != nil {
		return core.Icon{}, err
	}
	if channels != manifest.Channels {
		return core.Icon{}, core.ParaError("channels", fmt.Sprintf("[%d,%d]", manifest.Channels, manifest.Channels),
			fmt.Sprintf("PNG has %d channels, manifest declares %d", channels, manifest.Channels))
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return core.Icon{}, core.DecodeError("PNG decode failed: " + err.Error())
	}
	if decoded.Bounds().Dx() != manifest.Width {
		return core.Icon{}, core.ParaError("width", fmt.Sprintf("[%d,%d]", manifest.Width, manifest.Width),
			fmt.Sprintf("PNG width %d does not match manifest width %d", decoded.Bounds().Dx(), manifest.Width))
	}
	if decoded.Bounds().Dy() != manifest.Height {
		return core.Icon{}, core.ParaError("height", fmt.Sprintf("[%d,%d]", manifest.Height, manifest.Height),
			fmt.Sprintf("PNG height %d does not match manifest height %d", decoded.Bounds().Dy(), manifest.Height))
	}
	nrgba := toNRGBA(decoded)
	if maskHash := core.MaskHash(manifest.ReferenceMask); maskHash != manifest.ReferenceMaskHash {
		return core.Icon{}, core.DecodeError("embedded reference mask hash mismatch")
	}
	maskBytes, err := os.ReadFile(filepath.Join(dir, manifest.MaskFile))
	if err != nil {
		return core.Icon{}, core.DecodeError("cannot read reference mask: " + err.Error())
	}
	maskChannels, err := pngChannels(maskBytes)
	if err != nil {
		return core.Icon{}, err
	}
	if maskChannels != 1 {
		return core.Icon{}, core.ParaError("mask_channels", "[1,1]",
			fmt.Sprintf("reference mask PNG has %d channels, expected 1", maskChannels))
	}
	maskIMG, err := png.Decode(bytes.NewReader(maskBytes))
	if err != nil {
		return core.Icon{}, core.DecodeError("reference mask decode failed: " + err.Error())
	}
	if maskIMG.Bounds().Dx() != manifest.Width || maskIMG.Bounds().Dy() != manifest.Height {
		return core.Icon{}, core.DecodeError("reference mask dimensions do not match manifest")
	}
	for y := 0; y < manifest.Height; y++ {
		for x := 0; x < manifest.Width; x++ {
			r, _, _, _ := maskIMG.At(x, y).RGBA()
			got := byte(0)
			if r >= 0x8000 {
				got = 1
			}
			if got != manifest.ReferenceMask[y*manifest.Width+x] {
				return core.Icon{}, core.DecodeError(fmt.Sprintf("reference mask PNG disagrees at %d,%d", x, y))
			}
		}
	}
	if manifest.ReferenceFingerprint == "" {
		manifest.ReferenceFingerprint = core.FingerprintTruth(manifest)
	} else if core.FingerprintTruth(manifest) != manifest.ReferenceFingerprint {
		return core.Icon{}, core.DecodeError("reference fingerprint does not match the manifest truth")
	}
	return core.Icon{Manifest: manifest, Image: nrgba}, nil
}

func toNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(x, y, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
