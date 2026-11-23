package spatialpick

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"sort"
)

func LocalMatrixDigest(m [16]float64, precision int) string {
	var raw [16]int64
	for i, v := range m {
		q, ok := quantizeFloat(v, precision)
		if !ok {
			q = 0
		}
		raw[i] = q
	}
	var buf [16 * 8]byte
	for i, v := range raw {
		binary.LittleEndian.PutUint64(buf[i*8:], uint64(v))
	}
	sum := sha256.Sum256(buf[:])
	return hex.EncodeToString(sum[:])
}

func ComputeSceneHash(sc *Scene) (string, error) {
	if sc == nil {
		return "", coded(CodeSchema, "hash_scene", "nil scene")
	}
	c := *sc
	c.ContentHash = ""
	b, err := json.Marshal(&c)
	if err != nil {
		return "", &CodedError{Code: CodeSchema, Op: "hash_scene", Cause: err}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ComputeStateHash binds immutable scene structure to the current fixed-point
// local transforms and the applied batch prefix.
func ComputeStateHash(baseSceneHash string, precision int, locals map[string]FixedMatrix, appliedBatches []string) string {
	return ComputeStateHashWithGeometry(baseSceneHash, precision, locals, appliedBatches, nil)
}

func ComputeStateHashWithGeometry(baseSceneHash string, precision int, locals map[string]FixedMatrix, appliedBatches []string, geometries map[string]*Geometry) string {
	h := sha256.New()
	h.Write([]byte("spatialpick-state-v1\x00"))
	h.Write([]byte(baseSceneHash))
	h.Write([]byte{byte(precision)})
	ids := make([]string, 0, len(locals))
	for id := range locals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var buf [8]byte
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
		for _, v := range locals[id] {
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
			h.Write(buf[:])
		}
	}
	for _, id := range appliedBatches {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	if geometries != nil {
		h.Write([]byte("geometry\x00"))
		geomIDs := make([]string, 0, len(geometries))
		for id := range geometries {
			geomIDs = append(geomIDs, id)
		}
		sort.Strings(geomIDs)
		for _, id := range geomIDs {
			b, err := json.Marshal(geometries[id])
			if err != nil {
				continue
			}
			h.Write([]byte(id))
			h.Write([]byte{0})
			h.Write(b)
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
