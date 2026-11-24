package spectralrender

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// SampleBundle is the integrable entry point for the generated local sample:
// scene, truth and their verified hashes are loaded together.
type SampleBundle struct {
	Directory string
	Scene     *Scene
	SceneHash string
	Truth     *TruthManifest
	TruthHash string
}

func LoadSampleBundle(dir string) (*SampleBundle, error) {
	scenePath := filepath.Join(dir, "scene.json")
	truthPath := filepath.Join(dir, "truth.json")
	if _, err := os.Stat(scenePath); err != nil {
		return nil, &Error{Code: ErrDecode, Path: scenePath, Message: "scene file is missing or unreadable", Err: err}
	}
	scene, sceneHash, err := LoadScene(scenePath)
	if err != nil {
		return nil, err
	}
	truth, truthHash, err := LoadTruth(truthPath)
	if err != nil {
		return nil, err
	}
	if terr := ValidateTruthForScene(truth, scene, sceneHash); terr != nil {
		return nil, terr
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if _, err := os.Stat(manifestPath); err == nil {
		manifest, merr := LoadSampleManifest(manifestPath)
		if merr != nil {
			return nil, merr
		}
		if manifest.SceneHash != sceneHash {
			return nil, &Error{Code: ErrSchema, Path: "samples.manifest.scene_hash",
				Message: "manifest scene hash does not match scene.json"}
		}
		truthBytes, rerr := os.ReadFile(truthPath)
		if rerr != nil {
			return nil, &Error{Code: ErrDecode, Path: truthPath, Message: "cannot read truth file", Err: rerr}
		}
		if sha256HexBytes(truthBytes) != manifest.TruthHash {
			return nil, &Error{Code: ErrDecode, Path: "samples.manifest.truth_hash",
				Message: "truth file hash does not match manifest"}
		}
		if manifest.CMFFile != "" {
			cmfPath := filepath.Join(dir, manifest.CMFFile)
			cmfBytes, rerr := os.ReadFile(cmfPath)
			if rerr != nil {
				return nil, &Error{Code: ErrDecode, Path: cmfPath, Message: "cannot read CMF table", Err: rerr}
			}
			if sha256HexBytes(cmfBytes) != manifest.CMFHash {
				return nil, &Error{Code: ErrDecode, Path: "samples.manifest.cmf_hash",
					Message: "CMF file hash does not match manifest"}
			}
			var cmfDoc CMFDocument
			if err := json.Unmarshal(cmfBytes, &cmfDoc); err != nil {
				return nil, &Error{Code: ErrDecode, Path: cmfPath, Message: "invalid CMF table JSON", Err: err}
			}
			if err := validateCMFDocument(&cmfDoc, &scene.Spectrum); err != nil {
				return nil, err
			}
		}
	}
	return &SampleBundle{
		Directory: dir,
		Scene:     scene, SceneHash: sceneHash,
		Truth: truth, TruthHash: truthHash,
	}, nil
}

func LoadSampleManifest(path string) (*SampleManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: ErrDecode, Path: path, Message: "cannot read sample manifest", Err: err}
	}
	var manifest SampleManifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		return nil, &Error{Code: ErrDecode, Path: path, Message: "invalid sample manifest JSON", Err: err}
	}
	if manifest.SchemaVersion != 1 {
		return nil, &Error{Code: ErrSchema, Path: "samples.manifest.schema_version",
			Message: "unsupported sample manifest schema"}
	}
	if manifest.SceneHash == "" || manifest.TruthHash == "" {
		return nil, &Error{Code: ErrSchema, Path: "samples.manifest",
			Message: "scene_hash and truth_hash are required"}
	}
	if manifest.SceneFile == "" || manifest.TruthFile == "" {
		return nil, &Error{Code: ErrSchema, Path: "samples.manifest",
			Message: "scene_file and truth_file are required"}
	}
	return &manifest, nil
}

func validateCMFDocument(doc *CMFDocument, sp *Spectrum) *Error {
	if doc.SchemaVersion != 1 {
		return schemaError("samples.cmf.schema_version", "unsupported CMF document schema")
	}
	if doc.StartNM != sp.StartNM || doc.EndNM != sp.EndNM || doc.StepNM != sp.StepNM {
		return schemaError("samples.cmf", "wavelength bounds do not match scene spectrum")
	}
	if !sameFloatSlice(doc.Wavelengths, sp.Wavelengths) {
		return schemaError("samples.cmf.wavelengths", "wavelength samples do not match scene spectrum")
	}
	if !sameFloatSlice(doc.CMF.X, sp.CMF.X) || !sameFloatSlice(doc.CMF.Y, sp.CMF.Y) || !sameFloatSlice(doc.CMF.Z, sp.CMF.Z) {
		return schemaError("samples.cmf.cmf", "CMF values do not match scene spectrum")
	}
	if doc.WhitePoint != sp.WhitePoint {
		return schemaError("samples.cmf.white_point_xyz", "white point does not match scene spectrum")
	}
	if doc.TargetGamut != sp.TargetGamut {
		return schemaError("samples.cmf.target_gamut", "target gamut does not match scene spectrum")
	}
	return nil
}

func sameFloatSlice(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
