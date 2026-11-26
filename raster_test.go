package strokelab

import "testing"

func TestBlockRasterMatchesFull(t *testing.T) {
	suite, err := GenerateSuite(424242)
	if err != nil {
		t.Fatal(err)
	}
	p := suite.Samples[0].Project
	blocks := BuildBlocks(p.Width, p.Height, p.BlockSize)
	for _, s := range p.Strokes {
		full := RasterizeStroke(s, p.Width, p.Height)
		for _, b := range blocks {
			local := RasterizeStrokeRect(s, b.Rect)
			for y := 0; y < b.Rect.H; y++ {
				for x := 0; x < b.Rect.W; x++ {
					got := local[y*b.Rect.W+x]
					want := full[(b.Rect.Y+y)*p.Width+b.Rect.X+x]
					if got != want {
						t.Fatalf("stroke %s block %d (%d,%d) got %d want %d", s.ID, b.ID, x, y, got, want)
					}
				}
			}
		}
	}
}
