package bake

import (
	"errors"
	"math"
	"sync"
	"testing"

	"terrainnormalverify/internal/model"
	"terrainnormalverify/internal/sample"
)

func TestBakeBlockDeterministicAcrossPreviewExactAndConcurrency(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	block := project.Blocks[0]
	preview := project.Parameters
	preview.PreviewOnly = true
	exact := project.Parameters
	exact.PreviewOnly = false
	baseline, err := BakeExact(block, project.Layers, exact, block.PixelCount()*2)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.BBox.Min.X > baseline.BBox.Max.X || baseline.BBox.Min.Z <= 0 || baseline.BBox.Max.Z > 1 {
		t.Fatalf("invalid bounding box: %+v", baseline.BBox)
	}
	const workers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			params := exact
			if index%2 == 0 {
				params = preview
			}
			buffer := block.PixelCount()
			if index%3 == 0 {
				buffer = 1 + index%5
			}
			var result *BlockResult
			var err error
			if index%2 == 0 {
				result, err = BakePreview(block, project.Layers, params, buffer)
			} else {
				result, err = BakeExact(block, project.Layers, params, buffer)
			}
			if err != nil {
				errCh <- err
				return
			}
			if err := equalResults(baseline, result); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestBakeBlockMatchesAnalyticTruth(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	block := project.Blocks[0]
	result, err := BakeBlock(block, project.Layers, project.Parameters, block.PixelCount())
	if err != nil {
		t.Fatal(err)
	}
	truth := project.Truth.Blocks[0]
	for _, pixel := range truth.Pixels {
		actual := result.Normals[result.Index(pixel.X, pixel.Y)]
		errorNorm := actual.Sub(pixel.Normal).Length()
		if errorNorm > pixel.Tolerance {
			t.Fatalf("pixel (%d,%d) error %.6f > tolerance %.6f", pixel.X, pixel.Y, errorNorm, pixel.Tolerance)
		}
		if math.Abs(result.Slopes[result.Index(pixel.X, pixel.Y)]-pixel.SlopeDeg) > 1 {
			t.Fatalf("pixel (%d,%d) slope mismatch", pixel.X, pixel.Y)
		}
	}
}

func TestBakeParameterAndBudgetErrors(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	block := project.Blocks[0]
	tests := []struct {
		name   string
		params model.BakeParameters
		buffer int
		code   string
		field  string
	}{
		{
			name:   "operator",
			params: withOperator(project.Parameters, "bad"),
			buffer: block.PixelCount(),
			code:   model.ErrPara,
			field:  "differenceOperator",
		},
		{
			name:   "scale low",
			params: withScale(project.Parameters, 0),
			buffer: block.PixelCount(),
			code:   model.ErrPara,
			field:  "scale",
		},
		{
			name:   "scale high",
			params: withScale(project.Parameters, 1e9),
			buffer: block.PixelCount(),
			code:   model.ErrPara,
			field:  "scale",
		},
		{
			name:   "buffer",
			params: project.Parameters,
			buffer: 0,
			code:   model.ErrBudget,
			field:  "perThreadPixelBuffer",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BakeBlock(block, project.Layers, test.params, test.buffer)
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Code != test.code || typed.Field != test.field {
				t.Fatalf("got %v, want %s field %s", err, test.code, test.field)
			}
		})
	}
	badWeights := block
	badWeights.Weights = append([][]int(nil), block.Weights...)
	badWeights.Weights[0] = append([]int(nil), block.Weights[0]...)
	badWeights.Weights[0][0] = 1001
	_, err := BakeBlock(badWeights, project.Layers, project.Parameters, block.PixelCount())
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.ErrWeight {
		t.Fatalf("expected E_WEIGHT from core, got %v", err)
	}
}

func equalResults(a, b *BlockResult) error {
	for i := range a.Normals {
		if !a.Normals[i].Equal(b.Normals[i]) {
			return errors.New("normal mismatch")
		}
		if math.Float64bits(a.Slopes[i]) != math.Float64bits(b.Slopes[i]) {
			return errors.New("slope mismatch")
		}
	}
	if !a.BBox.Min.Equal(b.BBox.Min) || !a.BBox.Max.Equal(b.BBox.Max) {
		return errors.New("bbox mismatch")
	}
	for i := range a.Colors {
		if a.Colors[i] != b.Colors[i] {
			return errors.New("color mismatch")
		}
	}
	return nil
}

func withOperator(params model.BakeParameters, operator string) model.BakeParameters {
	params.DifferenceOperator = operator
	return params
}

func withScale(params model.BakeParameters, scale float64) model.BakeParameters {
	params.Scale = scale
	return params
}
