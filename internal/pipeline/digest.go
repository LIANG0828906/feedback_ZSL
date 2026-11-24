package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"collectionrecon/internal/model"
)

// AlgoVersion is bumped whenever a deterministic algorithm changes. Stored in
// every checkpoint so stale rows can be rejected on restart.
const AlgoVersion = "recon-1.0.0"

func sha256Hex(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ZoneContentHash hashes every input that can change a zone's deterministic
// output: zone parameters, owning no-collect geometry, raster and observations.
func ZoneContentHash(region *model.Region, zoneID int) string {
	zone := findZone(region, zoneID)
	noCollect := make([]model.NoCollectZone, 0)
	for _, nz := range region.NoCollect {
		if nz.ZoneID == zoneID {
			noCollect = append(noCollect, nz)
		}
	}
	cells := make([]model.SampleCell, 0)
	obs := make([]model.Observation, 0)
	for _, c := range region.SampleGrid {
		if c.ZoneID == zoneID {
			cells = append(cells, c)
		}
	}
	for _, o := range region.Observations {
		if o.ZoneID == zoneID {
			obs = append(obs, o)
		}
	}
	payload := struct {
		Seed         uint64
		RasterCell   float64
		GridSpacing  float64
		QuantStep    float64
		Zone         model.Zone
		NoCollect    []model.NoCollectZone
		Cells        []model.SampleCell
		Observations []model.Observation
	}{
		region.Seed, region.RasterCell, region.GridSpacing, region.QuantStep,
		zone, noCollect, cells, obs,
	}
	return sha256Hex(payload)
}

// PointsDigest hashes a reconstructed point set in its stored order.
func PointsDigest(points []model.ReconstructedPoint) string {
	return sha256Hex(points)
}

// HeatDigest hashes the ordered heat tiles.
func HeatDigest(tiles []model.HeatTile) string {
	return sha256Hex(tiles)
}

func findZone(region *model.Region, zoneID int) model.Zone {
	for _, z := range region.Zones {
		if z.ID == zoneID {
			return z
		}
	}
	panic("unknown zone id")
}
