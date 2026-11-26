package dialoguecal

import (
	"context"
	"os"
	"testing"
)

func mustGenerate(t *testing.T, seed int64) *GeneratedProject {
	t.Helper()
	g, err := Generate(DefaultGenerateOptions(seed))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return g
}

func mustStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/checkpoints.db"
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func mustRun(t *testing.T, e *Engine, p *Project, mode RunMode) *RunResult {
	t.Helper()
	r, err := e.Run(context.Background(), p, RunOptions{Mode: mode})
	if err != nil {
		t.Fatalf("Run(%s): %v", mode, err)
	}
	return r
}

func requireCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	ke := AsError(err)
	if ke == nil {
		t.Fatalf("expected kernel error %s, got %T: %v", code, err, err)
	}
	if ke.Code != code {
		t.Fatalf("expected %s, got %s (%v)", code, ke.Code, err)
	}
	return ke
}

func copyGeneratedFiles(t *testing.T, g *GeneratedProject) {
	t.Helper()
	dir := t.TempDir()
	for v, data := range g.Docs {
		if err := os.WriteFile(dir+"/"+v.String()+".json", data, 0o600); err != nil {
			t.Fatalf("write generated doc: %v", err)
		}
	}
}
