package httpui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"warmcache/internal/domain"
	"warmcache/internal/engine"
	"warmcache/internal/gen"
	"warmcache/internal/loader"
	"warmcache/internal/store"
)

//go:embed page.html
var pageFS embed.FS

type Server struct {
	seed     int64
	samples  []gen.VersionedSample
	projects map[domain.SchemaVersion]domain.Project
	services map[domain.SchemaVersion]*engine.Service
	raw      map[domain.SchemaVersion][]byte
	hashes   map[domain.SchemaVersion]string
}

type runRequest struct {
	Version          string             `json:"version"`
	Ticks            int64              `json:"ticks"`
	Workers          int                `json:"workers"`
	MaxWorkers       int                `json:"max_workers"`
	MaxBufferPerNode int                `json:"max_buffer_per_node"`
	Edits            []domain.Edit      `json:"edits,omitempty"`
	Interrupts       []domain.Interrupt `json:"interrupts,omitempty"`
	CheckpointTick   int64              `json:"checkpoint_tick,omitempty"`
}

type injectRequest struct {
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

type projectView struct {
	Version      domain.SchemaVersion `json:"version"`
	Schema       string               `json:"schema"`
	SemanticHash string               `json:"semantic_hash"`
	Project      domain.Project       `json:"project"`
	Traits       []string             `json:"traits"`
	RawBytes     int                  `json:"raw_bytes"`
}

func New(seed int64) (*Server, error) {
	if seed == 0 {
		seed = gen.DefaultSeed
	}
	samples, err := gen.Samples(seed)
	if err != nil {
		return nil, err
	}
	server := &Server{
		seed:     seed,
		samples:  samples,
		projects: map[domain.SchemaVersion]domain.Project{},
		services: map[domain.SchemaVersion]*engine.Service{},
		raw:      map[domain.SchemaVersion][]byte{},
		hashes:   map[domain.SchemaVersion]string{},
	}
	for _, sample := range samples {
		project, err := loader.Load(sample.Raw, sample.Version)
		if err != nil {
			return nil, fmt.Errorf("load generated %s sample: %w", sample.Version, err)
		}
		server.projects[sample.Version] = project
		server.raw[sample.Version] = append([]byte(nil), sample.Raw...)
		server.hashes[sample.Version] = rawHash(sample.Raw)
		server.services[sample.Version] = engine.NewService(project, gen.Requests(seed))
	}
	return server, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/health", s.health)
	mux.HandleFunc("/api/projects", s.projectsHandler)
	mux.HandleFunc("/api/sample", s.sample)
	mux.HandleFunc("/api/run", s.run)
	mux.HandleFunc("/api/edit", s.edit)
	mux.HandleFunc("/api/checkpoint/compare", s.checkpointCompare)
	mux.HandleFunc("/api/inject", s.inject)
	return s.recover(mux)
}

func (s *Server) index(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		s.writeError(writer, domain.NewBizError(domain.CodeNotFound, "index", "route not found", map[string]any{"path": request.URL.Path}))
		return
	}
	content, err := pageFS.ReadFile("page.html")
	if err != nil {
		s.writeError(writer, domain.AsBizError(err))
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = writer.Write(content)
}

func (s *Server) health(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		s.methodNotAllowed(writer, request, http.MethodGet)
		return
	}
	s.writeData(writer, http.StatusOK, map[string]any{
		"status": "ok", "seed": s.seed, "versions": []string{"v1", "v2", "v3"},
		"snapshot_version": store.SnapshotVersion,
	})
}

func (s *Server) projectsHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		s.methodNotAllowed(writer, request, http.MethodGet)
		return
	}
	views := make([]projectView, 0, len(s.samples))
	baseline := ""
	allEqual := true
	for _, sample := range s.samples {
		project := s.projects[sample.Version]
		if baseline == "" {
			baseline = project.SemanticHash()
		} else if baseline != project.SemanticHash() {
			allEqual = false
		}
		views = append(views, projectView{
			Version: sample.Version, Schema: sample.Schema,
			SemanticHash: project.SemanticHash(), Project: project,
			Traits: traits(sample.Version), RawBytes: len(s.raw[sample.Version]),
		})
	}
	s.writeData(writer, http.StatusOK, map[string]any{
		"seed": s.seed, "all_semantically_equal": allEqual,
		"baseline_hash": baseline, "projects": views,
	})
}

func (s *Server) sample(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		s.methodNotAllowed(writer, request, http.MethodGet)
		return
	}
	version, err := parseVersion(request.URL.Query().Get("version"))
	if err != nil {
		s.writeError(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Semantic-Hash", s.projects[version].SemanticHash())
	writer.Header().Set("X-Source-Hash", s.hashes[version])
	_, _ = writer.Write(s.raw[version])
}

func (s *Server) run(writer http.ResponseWriter, request *http.Request) {
	req, err := s.decodeRun(request)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	version, err := parseVersion(req.Version)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	opts := domain.Options{
		Ticks: req.Ticks, Workers: req.Workers, MaxWorkers: req.MaxWorkers,
		MaxBufferPerNode: req.MaxBufferPerNode, Interrupts: req.Interrupts,
	}
	result, err := s.services[version].Run(opts)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	s.writeData(writer, http.StatusOK, map[string]any{
		"version": version, "traits": traits(version), "result": result,
		"source_unchanged": s.sourceHashesUnchanged(),
	})
}

func (s *Server) edit(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		s.methodNotAllowed(writer, request, http.MethodPost)
		return
	}
	req, err := s.decodeRun(request)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	if len(req.Edits) == 0 {
		s.writeError(writer, domain.NewBizError(domain.CodeEditInvalid, "edit", "at least one edit is required", nil))
		return
	}
	version, err := parseVersion(req.Version)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	opts := domain.Options{
		Ticks: req.Ticks, Workers: req.Workers, MaxWorkers: req.MaxWorkers,
		MaxBufferPerNode: req.MaxBufferPerNode, Edits: req.Edits, Interrupts: req.Interrupts,
	}
	result, err := s.services[version].RunVerified(opts)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	baseOpts := opts
	baseOpts.Edits = nil
	baseline, baselineErr := s.services[version].Run(baseOpts)
	if baselineErr != nil {
		s.writeError(writer, baselineErr)
		return
	}
	s.writeData(writer, http.StatusOK, map[string]any{
		"version": version, "result": result, "matches_full": result.MatchesFull,
		"full_comparison": result.FullComparison, "source_unchanged": s.sourceHashesUnchanged(),
		"diff": incrementalDiff(baseline, result, minEditTick(req.Edits)),
	})
}

type tickDiff struct {
	Tick   int64    `json:"tick"`
	Fields []string `json:"fields"`
}

func incrementalDiff(baseline, edited domain.RunResult, editTick int64) map[string]any {
	changed := make([]tickDiff, 0)
	unchanged := 0
	for index := 0; index < len(edited.Timeline) && index < len(baseline.Timeline); index++ {
		before := baseline.Timeline[index]
		after := edited.Timeline[index]
		fields := make([]string, 0)
		if before.UsedBandwidth != after.UsedBandwidth || before.Capacity != after.Capacity {
			fields = append(fields, "bandwidth")
		}
		if !reflect.DeepEqual(before.Nodes, after.Nodes) {
			fields = append(fields, "nodes")
		}
		if !reflect.DeepEqual(before.Queue, after.Queue) {
			fields = append(fields, "queue")
		}
		if !reflect.DeepEqual(before.Commits, after.Commits) {
			fields = append(fields, "commits")
		}
		if !reflect.DeepEqual(before.Requests, after.Requests) {
			fields = append(fields, "requests")
		}
		if !reflect.DeepEqual(before.Shards, after.Shards) {
			fields = append(fields, "shards")
		}
		if len(fields) > 0 {
			changed = append(changed, tickDiff{Tick: after.Tick, Fields: fields})
		} else if after.Tick >= editTick {
			unchanged++
		}
	}
	return map[string]any{
		"edit_tick": editTick, "changed_ticks": changed,
		"changed_count": len(changed), "recomputed_without_visible_change": unchanged,
		"reused_ticks": edited.ReusedTicks,
	}
}

func minEditTick(edits []domain.Edit) int64 {
	if len(edits) == 0 {
		return 0
	}
	minimum := edits[0].AtTick
	for _, edit := range edits[1:] {
		if edit.AtTick < minimum {
			minimum = edit.AtTick
		}
	}
	return minimum
}

func (s *Server) checkpointCompare(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		s.methodNotAllowed(writer, request, http.MethodPost)
		return
	}
	req, err := s.decodeRun(request)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	version, err := parseVersion(req.Version)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	opts := domain.Options{
		Ticks: req.Ticks, Workers: req.Workers, MaxWorkers: req.MaxWorkers,
		MaxBufferPerNode: req.MaxBufferPerNode, Interrupts: req.Interrupts,
	}
	result, err := s.services[version].Run(opts)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	if len(result.Timeline) == 0 {
		s.writeError(writer, domain.NewBizError(domain.CodeCheckpointOrder, "checkpoint_compare", "run produced no committed ticks", nil))
		return
	}
	checkpointTick := req.CheckpointTick
	if checkpointTick <= 0 {
		checkpointTick = result.Timeline[len(result.Timeline)-1].Tick
	}
	if checkpointTick < 1 || checkpointTick > int64(len(result.Timeline)) {
		s.writeError(writer, domain.NewBizError(domain.CodeCheckpointOrder, "checkpoint_compare", "checkpoint tick is out of range", map[string]any{
			"checkpoint_tick": checkpointTick, "available": len(result.Timeline),
		}))
		return
	}
	directory, err := os.MkdirTemp("", "warmcache-checkpoint-*")
	if err != nil {
		s.writeError(writer, domain.NewBizError(domain.CodeCheckpointCorrupt, "checkpoint_compare", "cannot create temporary checkpoint directory", map[string]any{"error": err.Error()}))
		return
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "checkpoint.sqlite")
	checkpoint, err := store.Open(path)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	beforeRun := result
	beforeRun.Timeline = append([]domain.TickState(nil), result.Timeline[:checkpointTick]...)
	if err := checkpoint.SaveResult(beforeRun); err != nil {
		_ = checkpoint.Close()
		s.writeError(writer, err)
		return
	}
	if err := checkpoint.Close(); err != nil {
		s.writeError(writer, err)
		return
	}
	reopened, err := store.Open(path)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	recovered, err := reopened.Latest()
	if err != nil {
		_ = reopened.Close()
		s.writeError(writer, err)
		return
	}
	beforeTick := result.Timeline[checkpointTick-1]
	equalAtCheckpoint := recovered.Tick == checkpointTick &&
		recovered.StateHash == store.TickHash(beforeTick) &&
		recovered.SemanticHash == result.SemanticHash
	if err := reopened.SaveResult(result); err != nil {
		_ = reopened.Close()
		s.writeError(writer, err)
		return
	}
	last, err := reopened.Latest()
	if err != nil {
		_ = reopened.Close()
		s.writeError(writer, err)
		return
	}
	if err := reopened.Close(); err != nil {
		s.writeError(writer, err)
		return
	}
	finalTick := result.Timeline[len(result.Timeline)-1]
	finalEqual := last.Tick == finalTick.Tick && last.StateHash == store.TickHash(finalTick)
	info, _ := os.Stat(path)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}
	s.writeData(writer, http.StatusOK, map[string]any{
		"version": version, "checkpoint_tick": checkpointTick,
		"before": beforeTick, "after_reopen": recovered,
		"before_state_hash": store.TickHash(beforeTick), "after_state_hash": recovered.StateHash,
		"equal_at_checkpoint": equalAtCheckpoint,
		"continued_tick":      last.Tick, "equal_after_continue": finalEqual,
		"checkpoint_file_bytes": size, "snapshot_version": store.SnapshotVersion,
	})
}

func (s *Server) inject(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		s.methodNotAllowed(writer, request, http.MethodPost)
		return
	}
	var req injectRequest
	if err := decodeJSON(request, &req); err != nil {
		s.writeError(writer, err)
		return
	}
	version, err := parseVersion(req.Version)
	if err != nil {
		s.writeError(writer, err)
		return
	}
	before := s.sourceHashesUnchanged()
	err = s.runInjection(version, req.Kind)
	if err == nil {
		err = domain.NewBizError(domain.CodeInvalidSnapshot, "inject", "anomaly injection unexpectedly succeeded", map[string]any{
			"kind": req.Kind, "version": version,
		})
	}
	if !s.sourceHashesUnchanged() || !before {
		err = domain.NewBizError(domain.CodeCheckpointCorrupt, "inject", "source sample changed during injection", map[string]any{"kind": req.Kind})
	}
	s.writeError(writer, err)
}

func (s *Server) runInjection(version domain.SchemaVersion, kind string) error {
	switch kind {
	case "duplicate_shard", "zero_lease", "bad_bandwidth_scale", "retired_node_lease", "schema_conflict":
		raw, err := s.mutatedSample(version, kind)
		if err != nil {
			return err
		}
		_, err = loader.Load(raw, version)
		if err == nil {
			return domain.NewBizError(domain.CodeInvalidSnapshot, "inject", "loader accepted the mutated sample", map[string]any{"kind": kind})
		}
		return err
	case "thread_budget":
		_, err := s.services[version].Run(domain.Options{Ticks: 8, Workers: 9999, MaxWorkers: 32})
		return err
	case "buffer_budget":
		_, err := s.services[version].Run(domain.Options{Ticks: 8, Workers: 2, MaxBufferPerNode: 999})
		return err
	case "truncated_checkpoint":
		directory, err := os.MkdirTemp("", "warmcache-truncated-*")
		if err != nil {
			return domain.NewBizError(domain.CodeCheckpointCorrupt, "inject", "cannot create temporary directory", map[string]any{"error": err.Error()})
		}
		defer os.RemoveAll(directory)
		path := filepath.Join(directory, "truncated.sqlite")
		if err := os.WriteFile(path, []byte("SQLite format 3"), 0o644); err != nil {
			return domain.NewBizError(domain.CodeCheckpointCorrupt, "inject", "cannot create truncated checkpoint", map[string]any{"error": err.Error()})
		}
		_, err = store.Open(path)
		return err
	case "snapshot_version":
		directory, err := os.MkdirTemp("", "warmcache-version-*")
		if err != nil {
			return domain.NewBizError(domain.CodeCheckpointCorrupt, "inject", "cannot create temporary directory", map[string]any{"error": err.Error()})
		}
		defer os.RemoveAll(directory)
		path := filepath.Join(directory, "version.sqlite")
		checkpoint, err := store.Open(path)
		if err != nil {
			return err
		}
		if err := checkpoint.SetMeta("snapshot_version", "warmcache-checkpoint.v0"); err != nil {
			_ = checkpoint.Close()
			return err
		}
		if err := checkpoint.Close(); err != nil {
			return err
		}
		_, err = store.Open(path)
		return err
	default:
		return domain.NewBizError(domain.CodeBadRequest, "inject", "unknown anomaly kind", map[string]any{"kind": kind})
	}
}

func (s *Server) mutatedSample(version domain.SchemaVersion, kind string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(s.raw[version], &raw); err != nil {
		return nil, domain.NewBizError(domain.CodeBadRequest, "inject", "cannot decode generated sample", map[string]any{"error": err.Error()})
	}
	switch kind {
	case "duplicate_shard":
		list := firstList(raw)
		if len(list) == 0 {
			return nil, domain.NewBizError(domain.CodeInvalidSnapshot, "inject", "sample has no shard list", map[string]any{"version": version})
		}
		first := list[0]
		if err := appendToFirstList(raw, first); err != nil {
			return nil, err
		}
	case "zero_lease":
		mutateKeys(raw, map[string]any{
			"lease_ticks": 0, "leaseTicks": 0, "lease_ms": 0, "leaseMs": 0, "lease_seconds": 0, "leaseSeconds": 0,
		})
	case "bad_bandwidth_scale":
		bandwidth, ok := raw["bandwidth"].(map[string]any)
		if !ok {
			return nil, domain.NewBizError(domain.CodeInvalidSnapshot, "inject", "sample has no bandwidth object", map[string]any{"version": version})
		}
		bandwidth["unit"] = "out_of_range"
		bandwidth["scale"] = "out_of_range"
	case "retired_node_lease":
		raw["initial_lease_holder"] = "edge-a"
		mutateNodeRetirement(raw)
	case "schema_conflict":
		clock, ok := raw["clock"].(map[string]any)
		if !ok {
			return nil, domain.NewBizError(domain.CodeInvalidSnapshot, "inject", "sample has no clock object", map[string]any{"version": version})
		}
		delete(raw, "preheat_watermark")
		delete(raw, "preheatWatermark")
		if version == domain.SchemaV2 {
			clock["leaseTicks"] = 9
		} else if version == domain.SchemaV1 {
			clock["lease_ms"] = 1750
		} else {
			clock["lease_ms"] = 1750
		}
	}
	return json.Marshal(raw)
}

func (s *Server) decodeRun(request *http.Request) (runRequest, error) {
	req := runRequest{Version: "v1", Ticks: 30, Workers: 3, MaxWorkers: 32}
	if request.Method == http.MethodGet {
		query := request.URL.Query()
		if value := query.Get("version"); value != "" {
			req.Version = value
		}
		req.Ticks = queryInt(query.Get("ticks"), req.Ticks)
		req.Workers = int(queryInt(query.Get("workers"), int64(req.Workers)))
		req.MaxWorkers = int(queryInt(query.Get("max_workers"), int64(req.MaxWorkers)))
		req.MaxBufferPerNode = int(queryInt(query.Get("max_buffer_per_node"), 0))
		if value := query.Get("interrupt_tick"); value != "" {
			tick := queryInt(value, 0)
			shard := query.Get("interrupt_shard")
			req.Interrupts = append(req.Interrupts, domain.Interrupt{
				ID: "query-interrupt", Tick: tick, ShardID: shard,
				Reason: "query_parameter", ExitSignal: query.Get("exit") == "1",
			})
		}
		return req, nil
	}
	if request.Method != http.MethodPost {
		return req, domain.NewBizError(domain.CodeBadRequest, "decode_run", "run accepts GET or POST", map[string]any{"method": request.Method})
	}
	if err := decodeJSON(request, &req); err != nil {
		return req, err
	}
	if req.Version == "" {
		req.Version = "v1"
	}
	if req.Ticks == 0 {
		req.Ticks = 30
	}
	if req.Workers == 0 {
		req.Workers = 3
	}
	if req.MaxWorkers == 0 {
		req.MaxWorkers = 32
	}
	return req, nil
}

func decodeJSON(request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, 2<<20))
	if err := decoder.Decode(target); err != nil {
		return domain.NewBizError(domain.CodeBadRequest, "decode_json", "invalid JSON request", map[string]any{"error": err.Error()})
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return domain.NewBizError(domain.CodeBadRequest, "decode_json", "request must contain exactly one JSON value", nil)
	}
	return nil
}

func parseVersion(value string) (domain.SchemaVersion, error) {
	if value == "" {
		value = "v1"
	}
	return domain.ParseSchemaVersion(value)
}

func (s *Server) sourceHashesUnchanged() bool {
	for _, sample := range s.samples {
		if s.hashes[sample.Version] != rawHash(s.raw[sample.Version]) {
			return false
		}
	}
	return true
}

func rawHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func traits(version domain.SchemaVersion) []string {
	switch version {
	case domain.SchemaV1:
		return []string{"flat shard table", "tick clock", "permille bandwidth", "hex digest", "implicit watermark"}
	case domain.SchemaV2:
		return []string{"flat replicaTable", "millisecond durations", "basis-point bandwidth", "hex digest", "implicit watermark"}
	case domain.SchemaV3:
		return []string{"nested replica groups", "second and millisecond durations", "basis-point bandwidth", "integer fingerprint", "explicit watermark"}
	default:
		return nil
	}
}

func (s *Server) methodNotAllowed(writer http.ResponseWriter, request *http.Request, allowed string) {
	writer.Header().Set("Allow", allowed)
	s.writeError(writer, domain.NewBizError(domain.CodeBadRequest, "method", "method not allowed", map[string]any{"method": request.Method, "allowed": allowed}))
}

func (s *Server) writeData(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "data": data})
}

func (s *Server) writeError(writer http.ResponseWriter, err error) {
	biz := domain.AsBizError(err)
	status := biz.HTTPStatus
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"ok": false, "error": biz})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.writeError(writer, domain.NewBizError(domain.CodeInternal, "http", "request panic", map[string]any{"value": fmt.Sprint(recovered)}))
			}
		}()
		next.ServeHTTP(writer, request)
	})
}

func firstList(raw map[string]any) []any {
	keys := []string{"shards", "replicaTable", "replica_groups"}
	for _, key := range keys {
		if list, ok := raw[key].([]any); ok {
			if key == "replica_groups" {
				for _, group := range list {
					if groupMap, ok := group.(map[string]any); ok {
						if replicas, ok := groupMap["replicas"].([]any); ok {
							return replicas
						}
					}
				}
			}
			return list
		}
	}
	return nil
}

func appendToFirstList(raw map[string]any, value any) error {
	if list, ok := raw["shards"].([]any); ok {
		raw["shards"] = append(list, value)
		return nil
	}
	if list, ok := raw["replicaTable"].([]any); ok {
		raw["replicaTable"] = append(list, value)
		return nil
	}
	if list, ok := raw["replica_groups"].([]any); ok {
		for _, group := range list {
			groupMap, ok := group.(map[string]any)
			if !ok {
				continue
			}
			if replicas, ok := groupMap["replicas"].([]any); ok {
				groupMap["replicas"] = append(replicas, value)
				return nil
			}
		}
	}
	return domain.NewBizError(domain.CodeInvalidSnapshot, "inject", "cannot locate shard list", nil)
}

func mutateKeys(value any, mutations map[string]any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, replacement := range mutations {
			if _, ok := typed[key]; ok {
				typed[key] = replacement
			}
		}
		for _, child := range typed {
			mutateKeys(child, mutations)
		}
	case []any:
		for _, child := range typed {
			mutateKeys(child, mutations)
		}
	}
}

func mutateNodeRetirement(raw map[string]any) {
	mutateNodes(raw, func(node map[string]any) {
		id, _ := node["node_id"].(string)
		if id == "" {
			id, _ = node["nodeID"].(string)
		}
		if id == "" {
			id, _ = node["id"].(string)
		}
		if id != "edge-a" {
			return
		}
		if _, ok := node["retire_tick"]; ok {
			node["retire_tick"] = 1
		} else {
			node["retireTick"] = 1
		}
	})
}

func mutateNodes(value any, mutate func(map[string]any)) {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"nodes", "members"} {
			if list, ok := typed[key].([]any); ok {
				for _, item := range list {
					if node, ok := item.(map[string]any); ok {
						mutate(node)
					}
				}
			}
		}
		for _, child := range typed {
			mutateNodes(child, mutate)
		}
	case []any:
		for _, child := range typed {
			mutateNodes(child, mutate)
		}
	}
}

func queryInt(value string, fallback int64) int64 {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
