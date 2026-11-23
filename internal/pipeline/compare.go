package pipeline

import (
	"fmt"

	"vqa/internal/errs"
	"vqa/internal/store"
)

// Divergence locates the first mismatch between two checkpoint stores. It is a
// pure function: it only reads in-memory records and never touches disk or
// mutates any persisted state.
type Divergence struct {
	Frame    int    `json:"frame"`
	BX       int    `json:"bx"`
	BY       int    `json:"by"`
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	PixelX   int    `json:"pixel_x,omitempty"`
	PixelY   int    `json:"pixel_y,omitempty"`
	Channel  int    `json:"channel,omitempty"`
}

// CompareStores compares two fully loaded stores block by block in the global
// raster sequence, then frame summaries. The first difference is returned with
// E_DIVERGE; identical stores return nil.
func CompareStores(a, b *store.Store) error {
	ra, rb := a.Blocks(), b.Blocks()
	if len(ra) != len(rb) {
		return errs.New(errs.EDiverge, "block count differs").
			WithInt("expected", int64(len(ra))).WithInt("actual", int64(len(rb)))
	}
	for i := range ra {
		if err := compareBlock(ra[i], rb[i]); err != nil {
			return err
		}
	}
	fa, fb := a.Frames(), b.Frames()
	if len(fa) != len(fb) {
		return errs.New(errs.EDiverge, "frame record count differs").
			WithInt("expected", int64(len(fa))).WithInt("actual", int64(len(fb)))
	}
	for i := range fa {
		if fa[i] != fb[i] {
			return errs.New(errs.EDiverge, "frame summary differs").
				WithInt("frame", int64(fa[i].Frame)).
				With("field", "frame_record")
		}
	}
	return nil
}

func compareBlock(a, b store.BlockRecord) error {
	loc := func(field string) *errs.Error {
		return errs.New(errs.EDiverge, "block field differs").
			WithInt("frame", int64(a.Frame)).
			WithInt("bx", int64(a.BX)).WithInt("by", int64(a.BY)).
			With("field", field)
	}
	if a.Frame != b.Frame || a.BX != b.BX || a.BY != b.BY {
		return loc("identity")
	}
	if a.InputHash != b.InputHash {
		return loc("input_hash")
	}
	if a.ParamHash != b.ParamHash {
		return loc("param_hash")
	}
	if a.Stats != b.Stats {
		return loc("stats")
	}
	if a.Quantiles != b.Quantiles {
		return loc("quantiles")
	}
	if a.EdgeFidelity != b.EdgeFidelity {
		return loc("edge_fidelity")
	}
	if a.Propagation.BX != b.Propagation.BX || a.Propagation.BY != b.Propagation.BY ||
		a.Propagation.MaxCoeffErr != b.Propagation.MaxCoeffErr ||
		a.Propagation.MeanCoeffErr != b.Propagation.MeanCoeffErr ||
		a.Propagation.MaxPixelErr != b.Propagation.MaxPixelErr ||
		a.Propagation.ObservedStep != b.Propagation.ObservedStep ||
		a.Propagation.PixelBound != b.Propagation.PixelBound ||
		len(a.Propagation.Neighbors) != len(b.Propagation.Neighbors) {
		return loc("propagation")
	}
	for i := range a.Propagation.Neighbors {
		if a.Propagation.Neighbors[i] != b.Propagation.Neighbors[i] {
			return loc("propagation.neighbors")
		}
	}
	if a.ReconDigest != b.ReconDigest {
		return loc("recon_digest")
	}
	if len(a.Windows) != len(b.Windows) {
		return loc("windows.length")
	}
	for i := range a.Windows {
		if a.Windows[i] != b.Windows[i] {
			return loc(fmt.Sprintf("windows[%d]", i))
		}
	}
	// Pixel-level comparison gives the exact first divergent coordinate.
	if len(a.ReconPixels) != len(b.ReconPixels) {
		return loc("recon_pixels.length")
	}
	ch := a.Params.Channels
	n := a.Params.BlockSize
	for i := range a.ReconPixels {
		if a.ReconPixels[i] != b.ReconPixels[i] {
			pix := i / ch
			c := i % ch
			return errs.New(errs.EDiverge, "first divergent reconstructed pixel").
				WithInt("frame", int64(a.Frame)).
				WithInt("bx", int64(a.BX)).WithInt("by", int64(a.BY)).
				WithInt("pixel_x", int64(a.BX*n+pix%n)).
				WithInt("pixel_y", int64(a.BY*n+pix/n)).
				WithInt("channel", int64(c)).
				With("field", "recon_pixels").
				WithInt("expected", int64(a.ReconPixels[i])).
				WithInt("actual", int64(b.ReconPixels[i]))
		}
	}
	return nil
}

// CompareDirectories opens two checkpoint directories read-only and compares
// them. It remains side-effect free: opening never appends or rewrites data.
func CompareDirectories(dirA, dirB string) error {
	sa, err := store.OpenReadOnly(dirA)
	if err != nil {
		return err
	}
	sb, err := store.OpenReadOnly(dirB)
	if err != nil {
		return err
	}
	return CompareStores(sa, sb)
}
