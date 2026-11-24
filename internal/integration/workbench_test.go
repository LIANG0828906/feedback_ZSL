package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/paths"
	"mediaforge/internal/project"
	"mediaforge/internal/runner"
	"mediaforge/internal/samplegen"
	"mediaforge/internal/state"
	"mediaforge/internal/store"
	"mediaforge/internal/verifier"
	"mediaforge/internal/web"
)

func setupSample(t *testing.T) (string, string, *domain.Project, *domain.ResolvedProject) {
	t.Helper()
	root := t.TempDir()
	if _, err := samplegen.Generate(samplegen.Options{OutDir: root, Seed: 42, Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(root, "arcade-hud", "project.json")
	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(projectPath)
	rp, err := project.Validate(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	return root, projectPath, p, rp
}

func openRunner(t *testing.T, dbPath, dir string, p *domain.Project, rp *domain.ResolvedProject) (*store.Store, *runner.Runner) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return st, runner.New(st, p, rp, project.Fingerprint(p, rp), dir)
}

func TestPauseRestartResumeAndFullMatch(t *testing.T) {
	root, projectPath, p, rp := setupSample(t)
	dir := filepath.Dir(projectPath)
	ctx := context.Background()

	rtStore, rtRunner := openRunner(t, filepath.Join(root, "rt.sqlite"), dir, p, rp)
	rtReport, err := rtRunner.Run(ctx, runner.Options{Mode: runner.ModeRealtime, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rtReport.Progress.Committed != len(rp.Variants) || rtReport.Progress.Failed != 0 {
		t.Fatalf("realtime progress: %+v", rtReport.Progress)
	}
	rtRecords, err := rtStore.Variants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := hashesByID(rtRecords)
	fullReport, err := rtRunner.Run(ctx, runner.Options{Mode: runner.ModeFull, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !fullReport.Finished || fullReport.Progress.Committed != len(rp.Variants) {
		t.Fatalf("full report: %+v", fullReport)
	}
	fullRecords, _ := rtStore.Variants(ctx)
	assertHashes(t, want, fullRecords)
	verifyReport, err := (&verifier.Verifier{Store: rtStore}).Verify(ctx)
	if err != nil || !verifyReport.AllMatch {
		t.Fatalf("verify after full: %+v %v", verifyReport, err)
	}
	rtStore.Close()

	resumePath := filepath.Join(root, "resume.sqlite")
	rsStore, rsRunner := openRunner(t, resumePath, dir, p, rp)
	paused, err := rsRunner.Run(ctx, runner.Options{Mode: runner.ModeResume, Workers: 2, PauseAfter: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !paused.Paused || paused.Progress.Committed < 1 || paused.Progress.Committed >= len(rp.Variants) {
		t.Fatalf("expected partial checkpoint, got %+v", paused.Progress)
	}
	rsStore.Close()

	rsStore, rsRunner = openRunner(t, resumePath, dir, p, rp)
	defer rsStore.Close()
	resumed, err := rsRunner.Run(ctx, runner.Options{Mode: runner.ModeResume, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Finished || resumed.Progress.Committed != len(rp.Variants) {
		t.Fatalf("resume progress: %+v", resumed.Progress)
	}
	resumeRecords, _ := rsStore.Variants(ctx)
	assertHashes(t, want, resumeRecords)
}

func TestCheckpointVersionMismatchInvalidatesAll(t *testing.T) {
	root, projectPath, p, rp := setupSample(t)
	dir := filepath.Dir(projectPath)
	dbPath := filepath.Join(root, "checkpoint.sqlite")
	st, r := openRunner(t, dbPath, dir, p, rp)
	if _, err := r.Run(context.Background(), runner.Options{Mode: runner.ModeRealtime, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	execSQL(t, dbPath, `UPDATE variants SET recipe_version='tampered' WHERE ordinal=0`)

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res, err := st.Ensure(context.Background(), p, rp, project.Fingerprint(p, rp), false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Invalidated || res.Committed != 0 || res.Pending != len(rp.Variants) {
		t.Fatalf("checkpoint was not fully invalidated: %+v", res)
	}
	recs, _ := st.Variants(context.Background())
	for _, rec := range recs {
		if rec.State != state.Pending || len(rec.OutputBlob) != 0 {
			t.Fatalf("stale checkpoint survived: %+v", rec)
		}
	}
}

func TestAdaptivePathUnderTightBudgetMatchesFine(t *testing.T) {
	root, projectPath, p, rp := setupSample(t)
	dir := filepath.Dir(projectPath)
	fineStore, fineRunner := openRunner(t, filepath.Join(root, "fine.sqlite"), dir, p, rp)
	if _, err := fineRunner.Run(context.Background(), runner.Options{Mode: runner.ModeFull, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	fineRecs, _ := fineStore.Variants(context.Background())
	want := hashesByID(fineRecs)
	fineStore.Close()

	p.Budget.FineMaxPixels = 1000
	p.Budget.FineMaxSamples = 1000
	p.Budget.FineMaxVariants = 1
	p.Budget.AdaptiveBlockPixels = 20000
	p.Budget.MaxPixelsPerWorker = 20000
	p.Budget.MaxSamplesPerWorker = 50000
	adaptiveStore, adaptiveRunner := openRunner(t, filepath.Join(root, "adaptive.sqlite"), dir, p, rp)
	defer adaptiveStore.Close()
	report, err := adaptiveRunner.Run(context.Background(), runner.Options{Mode: runner.ModeFull, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if report.Scale.Path != "adaptive" || report.Progress.Committed != len(rp.Variants) {
		t.Fatalf("adaptive run did not follow expected path: %+v", report.Scale)
	}
	recs, _ := adaptiveStore.Variants(context.Background())
	assertHashes(t, want, recs)
	for _, rec := range recs {
		if rec.Path != "adaptive" {
			t.Fatalf("%s used %s, want adaptive", rec.ID, rec.Path)
		}
		var decision map[string]any
		if err := json.Unmarshal([]byte(rec.DecisionJSON), &decision); err != nil {
			t.Fatal(err)
		}
		if decision["preview"] == nil {
			t.Fatalf("%s has no recorded preview", rec.ID)
		}
	}
}

func TestIndependentVerifierDetectsTruthMismatch(t *testing.T) {
	root, projectPath, p, rp := setupSample(t)
	dir := filepath.Dir(projectPath)
	dbPath := filepath.Join(root, "verify.sqlite")
	st, r := openRunner(t, dbPath, dir, p, rp)
	if _, err := r.Run(context.Background(), runner.Options{Mode: runner.ModeRealtime, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	var truthJSON string
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT truth_json FROM variants WHERE variant_id='v-main'`).Scan(&truthJSON); err != nil {
		t.Fatal(err)
	}
	var truth map[string]any
	_ = json.Unmarshal([]byte(truthJSON), &truth)
	truth["width"] = 999
	changed, _ := json.Marshal(truth)
	if _, err := db.Exec(`UPDATE variants SET truth_json=? WHERE variant_id='v-main'`, string(changed)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	report, err := (&verifier.Verifier{Store: st}).Verify(context.Background())
	if errs.CodeOf(err) != errs.EDiff {
		t.Fatalf("code = %s, want E_DIFF (%v)", errs.CodeOf(err), err)
	}
	if report.FirstFailure == nil || report.FirstFailure.Item != "width" || report.FirstFailure.Variant != "v-main" {
		t.Fatalf("unexpected first failure: %+v", report.FirstFailure)
	}
}

func TestIndependentVerifierOwnFailureUsesEVerify(t *testing.T) {
	root, projectPath, p, rp := setupSample(t)
	dir := filepath.Dir(projectPath)
	dbPath := filepath.Join(root, "verify-own.sqlite")
	st, r := openRunner(t, dbPath, dir, p, rp)
	if _, err := r.Run(context.Background(), runner.Options{Mode: runner.ModeRealtime, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	execSQL(t, dbPath, `UPDATE variants SET output_blob=x'00' WHERE variant_id='v-main'`)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = (&verifier.Verifier{Store: st}).Verify(context.Background())
	if errs.CodeOf(err) != errs.EVerify {
		t.Fatalf("code = %s, want E_VERIFY (%v)", errs.CodeOf(err), err)
	}
}

func TestThreePathComparison(t *testing.T) {
	_, projectPath, _, _ := setupSample(t)
	report, err := paths.Compare(context.Background(), projectPath, 2, 1)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !report.Equivalent {
		t.Fatalf("paths differ: %+v", report.FirstMismatch)
	}
	for _, hm := range report.Heatmaps {
		if hm.MaxDelta != 0 || hm.Changed != 0 {
			t.Fatalf("heatmap for %s is not zero: %+v", hm.Variant, hm)
		}
	}
}

func TestHTTPDiagnosticsAndFrozenCommit(t *testing.T) {
	root, _, _, _ := setupSample(t)
	srv, err := web.New(web.Config{
		DataDir: root,
		DBRoot:  filepath.Join(root, "web-work"),
		Addr:    "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "MFX FORGE") {
		t.Fatal("diagnostic page is missing its title")
	}
	var stateResp map[string]any
	getJSON(t, ts.URL+"/api/state", &stateResp)
	if stateResp["selected"] == nil {
		t.Fatal("no project auto-selected")
	}
	postJSON(t, ts.URL+"/api/run", `{"mode":"realtime","workers":2}`, &stateResp)
	deadline := time.Now().Add(20 * time.Second)
	for {
		getJSON(t, ts.URL+"/api/state", &stateResp)
		if running, _ := stateResp["running"].(bool); !running {
			selected := stateResp["selected"].(map[string]any)
			progress := selected["progress"].(map[string]any)
			if int(progress["committed"].(float64)) == 3 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTP batch did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	status, errBody := postStatus(ts.URL+"/api/recipe", `{"variant":"v-main","field":"targetWidth","value":100}`)
	if status != http.StatusConflict || !strings.Contains(errBody, "E_STATE") {
		t.Fatalf("committed mutation status=%d body=%s", status, errBody)
	}
	postJSON(t, ts.URL+"/api/verify", `{}`, &stateResp)
	deadline = time.Now().Add(10 * time.Second)
	for {
		getJSON(t, ts.URL+"/api/state", &stateResp)
		v := stateResp["verify"].(map[string]any)
		if !v["running"].(bool) {
			if v["report"] == nil {
				t.Fatal("verification report is nil")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTP verification did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func hashesByID(recs []store.VariantRecord) map[string]string {
	out := map[string]string{}
	for _, rec := range recs {
		out[rec.ID] = rec.OutputSHA256
	}
	return out
}

func assertHashes(t *testing.T, want map[string]string, recs []store.VariantRecord) {
	t.Helper()
	if len(recs) != len(want) {
		t.Fatalf("variant count = %d, want %d", len(recs), len(want))
	}
	for _, rec := range recs {
		if rec.State != state.Committed {
			t.Fatalf("%s is %s, want committed", rec.ID, rec.State)
		}
		if rec.OutputSHA256 != want[rec.ID] {
			t.Fatalf("%s hash = %s, want %s", rec.ID, rec.OutputSHA256, want[rec.ID])
		}
	}
}

func execSQL(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func postJSON(t *testing.T, url, body string, out any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s: %d %s", url, resp.StatusCode, data)
	}
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
}

func postStatus(url, body string) (int, string) {
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}
