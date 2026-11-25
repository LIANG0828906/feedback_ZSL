package mesh

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"

	"meshdoctor/internal/errs"
)

type Vec3 [3]float64

type Face struct {
	A int `json:"a"`
	B int `json:"b"`
	C int `json:"c"`
}

func (f Face) Indices() [3]int { return [3]int{f.A, f.B, f.C} }

type Mesh struct {
	Vertices []Vec3 `json:"vertices"`
	Faces    []Face `json:"faces"`
}

func New() *Mesh { return &Mesh{} }

func (m *Mesh) Clone() *Mesh {
	if m == nil {
		return nil
	}
	out := &Mesh{Vertices: append([]Vec3(nil), m.Vertices...), Faces: append([]Face(nil), m.Faces...)}
	return out
}

func (m *Mesh) ValidateIndices() error {
	for fi, f := range m.Faces {
		for _, idx := range f.Indices() {
			if idx < 0 || idx >= len(m.Vertices) {
				err := errs.New(errs.EIDX, "face %d references vertex %d, valid range [0,%d)", fi, idx, len(m.Vertices))
				return errs.WithDetails(err, map[string]any{
					"face_index": fi, "vertex_index": idx, "vertex_count": len(m.Vertices),
				})
			}
		}
	}
	return nil
}

func (m *Mesh) DegenerateFaces(epsilon float64) []int {
	if epsilon < 0 {
		epsilon = 0
	}
	var out []int
	for fi, f := range m.Faces {
		a, b, c := m.Vertices[f.A], m.Vertices[f.B], m.Vertices[f.C]
		if f.A == f.B || f.B == f.C || f.A == f.C {
			out = append(out, fi)
			continue
		}
		n := Cross(Sub(b, a), Sub(c, a))
		if Norm(n) <= epsilon {
			out = append(out, fi)
		}
	}
	return out
}

// CanonicalBytes is the stable serialization used for content hashes and
// byte-for-byte path comparison. All integers are fixed-width little-endian.
func (m *Mesh) CanonicalBytes() []byte {
	var buf bytes.Buffer
	buf.WriteString("MESHDOCTOR-MESH-V1\n")
	var u64 [8]byte
	writeU64 := func(v uint64) {
		binary.LittleEndian.PutUint64(u64[:], v)
		buf.Write(u64[:])
	}
	writeU64(uint64(len(m.Vertices)))
	writeU64(uint64(len(m.Faces)))
	for _, v := range m.Vertices {
		for _, x := range v {
			writeU64(math.Float64bits(x))
		}
	}
	for _, f := range m.Faces {
		for _, idx := range f.Indices() {
			writeU64(uint64(idx))
		}
	}
	return buf.Bytes()
}

func (m *Mesh) Hash() string {
	sum := sha256.Sum256(m.CanonicalBytes())
	return hex.EncodeToString(sum[:])
}

func DecodeCanonical(b []byte) (*Mesh, error) {
	r := bytes.NewReader(b)
	magic := make([]byte, len("MESHDOCTOR-MESH-V1\n"))
	if _, err := r.Read(magic); err != nil || string(magic) != "MESHDOCTOR-MESH-V1\n" {
		return nil, fmt.Errorf("invalid mesh canonical magic")
	}
	readU64 := func() (uint64, error) {
		var arr [8]byte
		if _, err := r.Read(arr[:]); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint64(arr[:]), nil
	}
	vc, err := readU64()
	if err != nil {
		return nil, err
	}
	fc, err := readU64()
	if err != nil {
		return nil, err
	}
	if vc > 100_000_000 || fc > 100_000_000 {
		return nil, fmt.Errorf("mesh count too large")
	}
	out := &Mesh{Vertices: make([]Vec3, vc), Faces: make([]Face, fc)}
	for i := range out.Vertices {
		for j := range out.Vertices[i] {
			bits, err := readU64()
			if err != nil {
				return nil, err
			}
			out.Vertices[i][j] = math.Float64frombits(bits)
		}
	}
	for i := range out.Faces {
		idx := [3]int{}
		for j := range idx {
			v, err := readU64()
			if err != nil {
				return nil, err
			}
			idx[j] = int(v)
		}
		out.Faces[i] = Face{A: idx[0], B: idx[1], C: idx[2]}
	}
	return out, nil
}

func (m *Mesh) Summary() map[string]any {
	if m == nil {
		return map[string]any{"vertices": 0, "faces": 0, "hash": ""}
	}
	return map[string]any{
		"vertices": len(m.Vertices),
		"faces":    len(m.Faces),
		"hash":     m.Hash()[:16],
		"bytes":    len(m.CanonicalBytes()),
	}
}
