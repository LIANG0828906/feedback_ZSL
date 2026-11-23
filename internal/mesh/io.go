package mesh

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const FormatV1 = "meshforge.mesh.v1"

type Document struct {
	Format   string   `json:"format"`
	Vertices []Vec3   `json:"vertices"`
	Faces    [][3]int `json:"faces"`
}

func NewDocument(m Mesh) Document {
	d := Document{
		Format:   FormatV1,
		Vertices: append([]Vec3(nil), m.Vertices...),
		Faces:    make([][3]int, len(m.Faces)),
	}
	for i, f := range m.Faces {
		d.Faces[i] = f.Indices()
	}
	return d
}

func (d Document) Mesh() Mesh {
	m := Mesh{
		Vertices: append([]Vec3(nil), d.Vertices...),
		Faces:    make([]Face, len(d.Faces)),
	}
	for i, f := range d.Faces {
		m.Faces[i] = Face{A: f[0], B: f[1], C: f[2]}
	}
	return m
}

func SaveFile(path string, m Mesh) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(NewDocument(m))
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644)
}

func LoadFile(path string) (Mesh, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Mesh{}, err
	}
	var d Document
	if err := json.Unmarshal(data, &d); err != nil {
		return Mesh{}, fmt.Errorf("decode mesh: %w", err)
	}
	if d.Format != FormatV1 {
		return Mesh{}, fmt.Errorf("unsupported mesh format %q", d.Format)
	}
	return d.Mesh(), nil
}

// WriteFileAtomic writes and fsyncs a sibling temporary file before renaming
// it into place. It is safe to use for checkpoints and generated data.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
