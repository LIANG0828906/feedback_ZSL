package app

import (
	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
	"meshdoctor/internal/pipeline"
	"meshdoctor/internal/sample"
)

type BoundaryCheck struct {
	Code         string         `json:"code"`
	Name         string         `json:"name"`
	OK           bool           `json:"ok"`
	ExpectedCode string         `json:"expected_code"`
	ActualCode   string         `json:"actual_code,omitempty"`
	Detail       string         `json:"detail,omitempty"`
	ErrorDetails map[string]any `json:"error_details,omitempty"`
}

func RunBoundaryChecks(samples []sample.Sample, params pipeline.Params) []BoundaryCheck {
	return []BoundaryCheck{
		checkError(errs.EIDX, "index out of range identifies face", func() error {
			m := &mesh.Mesh{Vertices: make([]mesh.Vec3, 3), Faces: []mesh.Face{{A: 0, B: 1, C: 9}}}
			return sample.ValidateLoadedMesh(m, &sample.Manifest{InputVertices: 3, InputFaces: 1})
		}),
		checkError(errs.EMETA, "face count manifest mismatch names field", func() error {
			m := triangleMesh()
			return sample.ValidateLoadedMesh(m, &sample.Manifest{InputVertices: 3, InputFaces: 2})
		}),
		checkError(errs.EMERGE, "over-large weld tolerance reports merged pairs and ring", func() error {
			octa := findSample(samples, "octa")
			if octa == nil {
				return errs.New(errs.EMERGE, "octa sample unavailable")
			}
			_, _, err := pipeline.Weld(&octa.Mesh, 0.6)
			return err
		}),
		checkError(errs.EHOLE, "unclosed target ring reports ring number", func() error {
			m := &mesh.Mesh{Vertices: make([]mesh.Vec3, 3)}
			return pipeline.VerifyRingClosure(m, pipeline.FilledRing{Index: 7, Vertices: []int{0, 1, 2}})
		}),
		checkError(errs.EPARA, "laplacian weight outside [0,1]", func() error {
			bad := params
			bad.LaplacianWeight = 1.5
			return bad.ValidateSmooth()
		}),
		checkError(errs.ETOPO, "euler change reports before and after", func() error {
			return pipeline.CheckTopologyUnchanged(mesh.TopologyStats{Vertices: 4, Faces: 4, Euler: 2}, mesh.TopologyStats{Vertices: 4, Faces: 4, Euler: 1})
		}),
		checkError(errs.ESTATE, "committed stage cannot move back to running", func() error {
			return pipeline.Transition(pipeline.StateCommitted, pipeline.StateRunning)
		}),
		checkError(errs.EVER, "mixed algorithm/parameter version names stage", func() error {
			return pipeline.CheckVersion("smooth", "pv-old", pipeline.AlgoVersion, params)
		}),
		checkError(errs.EDIFF, "byte comparison reports first difference", func() error {
			ma := triangleMesh()
			mb := triangleMesh()
			mb.Vertices[0][0] += 0.01
			a := &pipeline.PathResult{
				CommittedSequence: []string{pipeline.StageDiagnostic},
				Stages: []pipeline.StageSnapshot{{
					Stage: pipeline.StageDiagnostic, Mesh: ma, MeshHash: ma.Hash(), Stats: mesh.Analyze(ma),
				}},
			}
			b := &pipeline.PathResult{
				CommittedSequence: []string{pipeline.StageDiagnostic},
				Stages: []pipeline.StageSnapshot{{
					Stage: pipeline.StageDiagnostic, Mesh: mb, MeshHash: mb.Hash(), Stats: mesh.Analyze(mb),
				}},
			}
			return pipeline.CompareResults(a, b).AsError()
		}),
		checkError(errs.EPOINT, "known point outside tolerance identifies point", func() error {
			man := &sample.Manifest{KnownPoints: []sample.KnownPoint{{
				ID: "kp-boundary", Position: mesh.Vec3{9, 9, 9}, Tolerance: 1e-9,
			}}}
			_, err := pipeline.CheckTruth(triangleMesh(), man)
			return err
		}),
		checkError(errs.ELOOP, "broken loop reports ring coordinates", func() error {
			man := &sample.Manifest{HoleRings: []sample.HoleRing{{
				ID: "loop-boundary", Tolerance: 1e-9,
				Coordinates: []mesh.Vec3{{9, 9, 9}, {10, 9, 9}, {9, 10, 9}},
			}}}
			_, err := pipeline.CheckTruth(triangleMesh(), man)
			return err
		}),
	}
}

func checkError(expected, name string, fn func() error) BoundaryCheck {
	err := fn()
	actual := errs.Code(err)
	check := BoundaryCheck{Code: expected, Name: name, ExpectedCode: expected, ActualCode: actual, OK: actual == expected}
	if err != nil {
		check.Detail = err.Error()
		check.ErrorDetails = errs.Details(err)
	}
	if !check.OK {
		check.Detail = "unexpected result: " + check.Detail
	}
	return check
}

func triangleMesh() *mesh.Mesh {
	return &mesh.Mesh{
		Vertices: []mesh.Vec3{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}},
		Faces:    []mesh.Face{{A: 0, B: 1, C: 2}},
	}
}

func findSample(samples []sample.Sample, id string) *sample.Sample {
	for i := range samples {
		if samples[i].Manifest.ID == id {
			return &samples[i]
		}
	}
	generated, err := sample.GenerateAll(sample.DefaultSeed)
	if err != nil {
		return nil
	}
	for i := range generated {
		if generated[i].Manifest.ID == id {
			return &generated[i]
		}
	}
	return nil
}
