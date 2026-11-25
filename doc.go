// Package acousticfield provides a deterministic, chunked early-reflection
// energy-field reconstruction kernel for planar levels.
//
// A typical integration is:
//
//	sample, err := acousticfield.Generate(42)
//	if err != nil { ... }
//	store, err := acousticfield.OpenStore("/tmp/acoustic.db")
//	if err != nil { ... }
//	defer store.Close()
//	engine, err := acousticfield.NewEngine(sample.Level, &sample.Truth, acousticfield.DefaultOptions(), store)
//	if err != nil { ... }
//	if _, err := engine.Initialize(context.Background()); err != nil { ... }
//	report, err := engine.Rebuild(context.Background(), acousticfield.BuildOptions{})
//	if err != nil { ... }
//	_ = report.Conservation.OK
//
// All geometry uses integer micrometres, absorption coefficients and normals
// use Q16 fixed point, and energy is an integer unit. Rebuilds are
// deterministic across worker counts and checkpoint recovery.
package acousticfield
