// Package fog implements a deterministic, tile-based participating-medium
// volume renderer for offline verification and integration into a larger
// pipeline.
//
// The package deliberately keeps all scene, ray, interval, integration,
// dependency and checkpoint state explicit. A minimal integration looks like:
//
//	scene := fog.Generate(fog.DefaultGenerateOptions(42))
//	if fault := scene.Validate(); fault != nil {
//		log.Fatal(fault)
//	}
//	store, err := fog.OpenCheckpointStore("scene.vfogj")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer store.Close()
//	engine := fog.NewEngine(scene, store)
//	preview, _ := engine.Render(ctx, fog.RenderOptions{Mode: fog.RenderPreview})
//	exact, _ := engine.Render(ctx, fog.RenderOptions{Mode: fog.RenderExact})
//	_, divergence := fog.CompareFrames(preview, exact, scene.Recipe.Tolerance)
//
// Fixed-point interval endpoints use Recipe.FixedScale. Medium coefficients
// use Recipe.CoefficientScale. Every overlap split records the ordered active
// medium set and both endpoint quantization residuals.
//
// The default persistence implementation is a pure-Go, checksummed,
// append-only transaction journal. This is the no-third-party-dependency
// alternative to a SQLite driver allowed by the project constraints. A
// non-truncated line is one atomic tile commit, serialized and fsynced before
// it becomes visible to the in-memory index.
package fog
