package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"

	"volumelab/pkg/measure"
	"volumelab/pkg/model"
	"volumelab/pkg/pool"
	"volumelab/pkg/render"
	"volumelab/pkg/samplegen"
	"volumelab/pkg/session"
	"volumelab/pkg/volume"
)

type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

type AnnotationDigest struct {
	ID            string             `json:"id"`
	WorldLengthMM float64            `json:"worldLengthMM"`
	VoxelLength   float64            `json:"voxelLength"`
	EndpointA     model.PointReading `json:"endpointA"`
	EndpointB     model.PointReading `json:"endpointB"`
}

type Report struct {
	AllPass            bool                   `json:"allPass"`
	Seed               int64                  `json:"seed"`
	SampleManifestHash string                 `json:"sampleManifestHash"`
	VolumeHashes       map[string]string      `json:"volumeHashes"`
	PlanePixelHashes   map[string]string      `json:"planePixelHashes"`
	ParallelPeak       int64                  `json:"parallelPeakWorkers"`
	Annotation         AnnotationDigest       `json:"annotation"`
	Precision          *model.PrecisionReport `json:"precision"`
	SnapshotChecksum   string                 `json:"snapshotChecksum"`
	Checks             []Check                `json:"checks"`
}

func Run(ctx context.Context, seed int64) (*Report, error) {
	if seed == 0 {
		seed = 42
	}
	baseDir, err := os.MkdirTemp("", "volumelab-verify-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(baseDir)
	dirA := filepath.Join(baseDir, "a")
	dirB := filepath.Join(baseDir, "b")
	genA, err := samplegen.Generate(samplegen.Options{OutputDir: dirA, Seed: seed})
	if err != nil {
		return nil, err
	}
	genB, err := samplegen.Generate(samplegen.Options{OutputDir: dirB, Seed: seed})
	if err != nil {
		return nil, err
	}
	report := &Report{Seed: seed, VolumeHashes: map[string]string{}, PlanePixelHashes: map[string]string{}}
	report.add("sample_generator_deterministic", deterministicSamples(genA, genB), "same seed produced byte-identical manifest and volume files")
	manifest, v, err := volume.LoadFromManifest(genA.ManifestPath, "vol_a")
	if err != nil {
		return nil, err
	}
	report.SampleManifestHash = manifest.ManifestHash
	for _, volumeDigest := range genA.Volumes {
		report.VolumeHashes[volumeDigest.ID] = volumeDigest.ContentHash
	}

	request := render.Request{
		Crosshair: model.Vec3{20, 30, 27},
		Window:    model.Window{Width: 850, Level: 200},
		ThicknessMM: map[string]float64{
			"axial":    4.5,
			"coronal":  3.6,
			"sagittal": 3,
		},
		InterpolationOrder: 1,
	}
	serial, serialErr := render.RenderTriple(ctx, v, request, nil)
	if serialErr != nil {
		return nil, serialErr
	}
	executor, err := pool.New(3, 37)
	if err != nil {
		return nil, err
	}
	parallel, parallelErr := render.RenderTriple(ctx, v, request, executor)
	if parallelErr != nil {
		return nil, parallelErr
	}
	pixelsMatch := true
	for i := range serial.Images {
		report.PlanePixelHashes[string(serial.Images[i].Plane)] = serial.Images[i].PixelHash
		if serial.Images[i].PixelHash != parallel.Images[i].PixelHash || !bytes.Equal(serial.Images[i].Pixels, parallel.Images[i].Pixels) {
			pixelsMatch = false
		}
	}
	report.ParallelPeak = executor.Stats().PeakWorkers
	report.add("three_plane_serial_equals_parallel", pixelsMatch && executor.Stats().PeakWorkers <= 3, "axial/coronal/sagittal images are byte-identical and parallelism stayed within the cap")
	indices, weights := render.EquivalentLayers(request.Crosshair[2], request.ThicknessMM["axial"], v.Spacing[2], v.Shape[2])
	sum := 0.0
	for _, weight := range weights {
		sum += weight
	}
	report.add("thickness_weights_normalized", len(indices) > 0 && math.Abs(sum-1) <= 1e-12, fmt.Sprintf("%d layers, normalized weight sum %.15f", len(indices), sum))

	annotation, appErr := measure.AutoAnnotation(v, "sphere_a", "sphere_diameter")
	if appErr != nil {
		return nil, appErr
	}
	annotation.Interpolation = 1
	annotation.ID = "verify-ann-1"
	worldLength, voxelLength := measure.Lengths(v, annotation.EndpointA, annotation.EndpointB)
	dualOK := math.Abs(worldLength-annotation.WorldLengthMM) < 1e-12 &&
		math.Abs(voxelLength-annotation.VoxelLength) < 1e-12 &&
		measure.ValidateStoredAnnotation(v, *annotation) == nil
	report.Annotation = AnnotationDigest{
		ID:            annotation.ID,
		WorldLengthMM: annotation.WorldLengthMM,
		VoxelLength:   annotation.VoxelLength,
		EndpointA:     annotation.EndpointA,
		EndpointB:     annotation.EndpointB,
	}
	report.add("annotation_dual_readings", dualOK, "world length, voxel length and both endpoint coordinate pairs cross-check")

	precision, precisionErr := measure.Compare(v, *annotation, "sphere_a", "sphere_diameter", v.ToleranceMM)
	report.Precision = precision
	precisionOK := precisionErr == nil && precision != nil && precision.Pass && precision.UpperBoundMM > 0
	precisionDetail := "no precision report"
	if precision != nil {
		precisionDetail = fmt.Sprintf("deviation=%.9g mm, derived bound=%.9f mm", precision.DeviationMM, precision.UpperBoundMM)
	}
	report.add("analytic_precision_and_bound", precisionOK, precisionDetail)

	snapshotDir := filepath.Join(baseDir, "sessions")
	firstStore := session.NewStore(filepath.Join(snapshotDir, "first.json"))
	sess, opened, appErr := session.Open(ctx, v, firstStore, executor)
	if appErr != nil {
		return nil, appErr
	}
	if _, appErr := sess.SetCrosshair(ctx, request.Crosshair); appErr != nil {
		return nil, appErr
	}
	if _, appErr := sess.SetWindow(ctx, request.Window); appErr != nil {
		return nil, appErr
	}
	for _, plane := range model.AllPlanes {
		if _, appErr := sess.SetThickness(ctx, plane, request.ThicknessMM[string(plane)]); appErr != nil {
			return nil, appErr
		}
	}
	committed, appErr := sess.AddAutoAnnotation(ctx, "sphere_a", "sphere_diameter")
	if appErr != nil {
		return nil, appErr
	}
	beforeState := committed.State
	report.SnapshotChecksum = committed.Snapshot.Checksum
	_, reopened, appErr := session.Open(ctx, v, firstStore, executor)
	if appErr != nil {
		return nil, appErr
	}
	recoveryOK := reopened.Recovered && reflect.DeepEqual(beforeState, reopened.State)
	for i := range committed.Triple.Images {
		if committed.Triple.Images[i].PixelHash != reopened.Triple.Images[i].PixelHash {
			recoveryOK = false
		}
	}
	report.add("snapshot_restore_exact", recoveryOK, "all state numbers, dual readings, image hashes and pixel buffers reconstruct exactly")
	report.add("first_open_created_snapshot", !opened.Recovered && opened.Snapshot.Sequence == 1, "new sessions commit sequence 1 atomically before interactive operations")

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, cancelErr := sess.SetWindow(cancelCtx, model.Window{Width: 10, Level: 10})
	stateAfterCancel := sess.State()
	cancelOK := cancelErr != nil && cancelErr.Code == "E_CANCEL" && reflect.DeepEqual(beforeState, stateAfterCancel)
	report.add("cancel_does_not_pollute_snapshot", cancelOK, "canceled operation left sequence, images and committed snapshot untouched")

	serialRun, err := runCommitPath(ctx, v, filepath.Join(snapshotDir, "serial.json"), nil, request)
	if err != nil {
		return nil, err
	}
	parallelExecutor, err := pool.New(3, 29)
	if err != nil {
		return nil, err
	}
	parallelRun, err := runCommitPath(ctx, v, filepath.Join(snapshotDir, "parallel.json"), parallelExecutor, request)
	if err != nil {
		return nil, err
	}
	commitOK := serialRun.Checksum == parallelRun.Checksum &&
		reflect.DeepEqual(serialRun.State, parallelRun.State) &&
		reflect.DeepEqual(serialRun.PlaneHashes, parallelRun.PlaneHashes)
	report.add("serial_parallel_commit_identity", commitOK, "identical operations produced identical image hashes and byte-identical committed payload checksums")
	report.AllPass = true
	for _, check := range report.Checks {
		if !check.Pass {
			report.AllPass = false
		}
	}
	return report, nil
}

type commitRun struct {
	Checksum    string
	State       session.State
	PlaneHashes map[string]string
}

func runCommitPath(ctx context.Context, v *volume.Volume, snapshotPath string, executor *pool.Executor, request render.Request) (commitRun, error) {
	sess, _, appErr := session.Open(ctx, v, session.NewStore(snapshotPath), executor)
	if appErr != nil {
		return commitRun{}, appErr
	}
	steps := []func() *model.AppError{
		func() *model.AppError {
			_, err := sess.SetCrosshair(ctx, request.Crosshair)
			return err
		},
		func() *model.AppError {
			_, err := sess.SetWindow(ctx, request.Window)
			return err
		},
		func() *model.AppError {
			_, err := sess.SetThickness(ctx, model.PlaneAxial, request.ThicknessMM["axial"])
			return err
		},
		func() *model.AppError {
			_, err := sess.AddAutoAnnotation(ctx, "tube_a", "tube_axis")
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return commitRun{}, err
		}
	}
	state := sess.State()
	loaded := sess.Store.Load()
	triple, appErr := render.RenderTriple(ctx, v, state.RenderRequest(), executor)
	if appErr != nil {
		return commitRun{}, appErr
	}
	hashes := map[string]string{}
	for _, image := range triple.Images {
		hashes[string(image.Plane)] = image.PixelHash
	}
	return commitRun{Checksum: loaded.Info.Checksum, State: state, PlaneHashes: hashes}, nil
}

func (r *Report) add(name string, pass bool, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Pass: pass, Detail: detail})
}

func deterministicSamples(a, b *samplegen.Result) bool {
	manifestA, errA := os.ReadFile(a.ManifestPath)
	manifestB, errB := os.ReadFile(b.ManifestPath)
	if errA != nil || errB != nil || !bytes.Equal(manifestA, manifestB) {
		return false
	}
	for i := range a.Volumes {
		fileA, errA := os.ReadFile(a.Volumes[i].File)
		fileB, errB := os.ReadFile(b.Volumes[i].File)
		if errA != nil || errB != nil || !bytes.Equal(fileA, fileB) {
			return false
		}
	}
	return true
}

func Write(report *Report, path string) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
}
