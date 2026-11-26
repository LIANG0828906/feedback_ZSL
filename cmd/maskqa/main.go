package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"

	"maskqa"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "local diagnostic listen address")
	checkpointDir := flag.String("checkpoints", ".maskqa-checkpoints", "repository-local checkpoint directory")
	genDir := flag.String("gen-dir", "", "write deterministic samples and schema artifacts to this directory")
	verify := flag.Bool("verify", false, "run fixed-sample verification and exit")
	flag.Parse()

	if *genDir != "" {
		if err := generateArtifacts(*genDir); err != nil {
			fmt.Fprintln(os.Stderr, "E_GEN:", err)
			os.Exit(1)
		}
		fmt.Printf("deterministic artifacts written to %s\n", *genDir)
		return
	}

	if *verify {
		if err := verifySamples(*checkpointDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("all fixed samples passed topology, direction and pixel-path checks")
		return
	}

	server, err := maskqa.NewHTTPServer(filepath.Join(*checkpointDir, "web"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "E_INIT:", err)
		os.Exit(1)
	}
	fmt.Printf("MaskQA workbench: http://%s\n", *addr)
	fmt.Println("no external services required; Ctrl-C to stop")
	if err := http.ListenAndServe(*addr, server.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "E_HTTP:", err)
		os.Exit(1)
	}
}

func generateArtifacts(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	samples, err := maskqa.GenerateSamples()
	if err != nil {
		return err
	}
	for _, project := range samples {
		for _, version := range []string{maskqa.SchemaV1, maskqa.SchemaV2, maskqa.SchemaV3} {
			exported, err := maskqa.ExportProject(project, version)
			if err != nil {
				return err
			}
			path := filepath.Join(dir, project.ID+"-"+versionSuffix(version)+".json")
			if err := os.WriteFile(path, exported.Data, 0o644); err != nil {
				return err
			}
		}
		result, err := maskqa.ComputeFull(project, maskqa.DefaultSchedulerOptions(), nil)
		if err != nil {
			return err
		}
		if err := writePNG(filepath.Join(dir, project.ID+"-icon.png"), project.Icon); err != nil {
			return err
		}
		if err := writeGrayPNG(filepath.Join(dir, project.ID+"-candidate.png"), project.Candidate); err != nil {
			return err
		}
		if err := writeGrayPNG(filepath.Join(dir, project.ID+"-reference.png"), project.Reference); err != nil {
			return err
		}
		if err := writeGrayPNG(filepath.Join(dir, project.ID+"-accumulated.png"), result.Accumulated); err != nil {
			return err
		}
	}
	return nil
}

func verifySamples(checkpointRoot string) error {
	samples, err := maskqa.GenerateSamples()
	if err != nil {
		return err
	}
	for i, project := range samples {
		dir := filepath.Join(checkpointRoot, "verify", project.ID)
		wb, err := maskqa.NewWorkbench(project, dir, maskqa.DefaultSchedulerOptions())
		if err != nil {
			return err
		}
		result, err := wb.InitializeFull()
		if err != nil {
			_ = wb.Close()
			return fmt.Errorf("sample %d: %w", i, err)
		}
		for _, domain := range result.Report.Domains {
			if domain.Direction == nil || !domain.Direction.Consistent {
				_ = wb.Close()
				return fmt.Errorf("sample %d domain %d direction inconsistent", i, domain.DomainNo)
			}
		}
		if err := wb.Close(); err != nil {
			return err
		}
		fmt.Printf("sample %s: %dx%d, %d domains, %d tiles, OK\n",
			project.ID, project.Width, project.Height,
			len(result.Report.Domains), len(result.Tiles))
	}
	return nil
}

func versionSuffix(version string) string {
	switch version {
	case maskqa.SchemaV1:
		return "v1"
	case maskqa.SchemaV2:
		return "v2"
	default:
		return "v3"
	}
}

func writePNG(path string, img image.Image) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return png.Encode(file, img)
}

func writeGrayPNG(path string, img *image.Gray) error {
	return writePNG(path, img)
}
