package diagnostic

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"ballistic/internal/kernel"
	"ballistic/internal/snapshot"
	"ballistic/internal/workbench"
)

type Server struct {
	mu          sync.Mutex
	seed        int64
	dbPath      string
	store       *snapshot.Store
	wb          *workbench.Workbench
	sample      *kernel.SampleSet
	schemaInfo  []SchemaInfo
	injection   *InjectionResult
	lastCompare *kernel.FrameComparison
}

type SchemaInfo struct {
	Version   int            `json:"version"`
	Loaded    bool           `json:"loaded"`
	SegmentN  int            `json:"segment_count"`
	FillCount int            `json:"fill_count"`
	LossCount int            `json:"loss_count"`
	Fills     []kernel.Issue `json:"fills"`
	Losses    []kernel.Issue `json:"losses"`
	Error     *kernel.Error  `json:"error,omitempty"`
}

type InjectionResult struct {
	Case         string        `json:"case"`
	ExpectedCode string        `json:"expected_code"`
	OK           bool          `json:"ok"`
	Issue        *kernel.Issue `json:"issue,omitempty"`
	Message      string        `json:"message,omitempty"`
}

type StateResponse struct {
	Seed             int64                       `json:"seed"`
	DBPath           string                      `json:"db_path"`
	Scene            *kernel.Scene               `json:"scene"`
	Report           *kernel.PredictionReport    `json:"report"`
	Camera           kernel.Camera               `json:"camera"`
	Tiles            []*kernel.TileMapping       `json:"tiles"`
	TileOrder        []string                    `json:"tile_order"`
	CacheKeys        map[string]string           `json:"cache_keys"`
	Plan             *workbench.InvalidationPlan `json:"plan,omitempty"`
	Comparison       *kernel.FrameComparison     `json:"comparison,omitempty"`
	ReusedTileIDs    []string                    `json:"reused_tile_ids"`
	RenderedTileIDs  []string                    `json:"rendered_tile_ids"`
	PoolAudit        []kernel.PoolAuditEvent     `json:"pool_audit"`
	Snapshots        []snapshot.Record           `json:"snapshots"`
	CommitLog        []snapshot.Record           `json:"commit_log"`
	SchemaInfo       []SchemaInfo                `json:"schema_info"`
	Injection        *InjectionResult            `json:"injection,omitempty"`
	AlgorithmVersion string                      `json:"algorithm_version"`
}

func NewServer(dbPath string) (*Server, error) {
	if dbPath == "" {
		dbPath = filepath.Join(".diagnostic", "ballistic.sqlite")
	}
	sample, err := kernel.GenerateSampleSet(kernel.DefaultSeed)
	if err != nil {
		return nil, err
	}
	store, err := snapshot.Open(dbPath)
	if err != nil {
		return nil, err
	}
	wb, err := workbench.New(kernel.DefaultSeed, store, workbench.DefaultFrameID)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := wb.RunResume(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	server := &Server{
		seed:       kernel.DefaultSeed,
		dbPath:     dbPath,
		store:      store,
		wb:         wb,
		sample:     sample,
		schemaInfo: buildSchemaInfo(sample),
	}
	return server, nil
}

func buildSchemaInfo(sample *kernel.SampleSet) []SchemaInfo {
	infos := []SchemaInfo{}
	for _, version := range []int{kernel.SchemaV1, kernel.SchemaV2, kernel.SchemaV3} {
		info := SchemaInfo{Version: version}
		loaded, err := kernel.LoadDocument(sample.Documents[version])
		if err != nil {
			if kernelErr, ok := err.(*kernel.Error); ok {
				info.Error = kernelErr
			}
			infos = append(infos, info)
			continue
		}
		info.Loaded = true
		info.SegmentN = len(loaded.Scene.Segments)
		info.FillCount = len(loaded.Transcript.Fills)
		info.Fills = loaded.Transcript.Fills

		exported, err := kernel.Export(loaded.Scene, version)
		if err == nil {
			info.LossCount = len(exported.Transcript.Losses)
			info.Losses = exported.Transcript.Losses
		}
		infos = append(infos, info)
	}
	return infos
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/document", s.handleDocument)
	mux.HandleFunc("/api/truth", s.handleTruth)
	mux.HandleFunc("/api/edit", s.handleEdit)
	mux.HandleFunc("/api/inject", s.handleInject)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/frame.png", s.handleFramePNG)
	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) stateResponseLocked(ctx context.Context) StateResponse {
	tiles := make([]*kernel.TileMapping, 0, len(s.wb.Mapping.TileOrder))
	for _, id := range s.wb.Mapping.TileOrder {
		tiles = append(tiles, s.wb.Mapping.Tiles[id])
	}
	cacheKeys := make(map[string]string, len(s.wb.Mapping.TileOrder))
	for _, id := range s.wb.Mapping.TileOrder {
		cacheKeys[id] = s.wb.Key(id).CacheKey()
	}
	snapshots, _ := s.store.ListCurrent(ctx, s.wb.FrameID)
	logs, _ := s.store.CommitLog(ctx, s.wb.FrameID)
	return StateResponse{
		Seed:             s.seed,
		DBPath:           s.dbPath,
		Scene:            s.wb.Scene,
		Report:           s.wb.Report,
		Camera:           s.wb.Camera,
		Tiles:            tiles,
		TileOrder:        append([]string{}, s.wb.Mapping.TileOrder...),
		CacheKeys:        cacheKeys,
		Plan:             s.wb.LastPlan,
		Comparison:       s.lastCompare,
		ReusedTileIDs:    append([]string{}, s.wb.ReusedTileIDs...),
		RenderedTileIDs:  append([]string{}, s.wb.RenderedTileIDs...),
		PoolAudit:        s.wb.Pool.AuditEvents(),
		Snapshots:        snapshots,
		CommitLog:        logs,
		SchemaInfo:       s.schemaInfo,
		Injection:        s.injection,
		AlgorithmVersion: kernel.AlgorithmVersion,
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, s.stateResponseLocked(r.Context()))
}

func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request) {
	version := kernel.SchemaV3
	switch r.URL.Query().Get("v") {
	case "1":
		version = kernel.SchemaV1
	case "2":
		version = kernel.SchemaV2
	case "3", "":
		version = kernel.SchemaV3
	default:
		http.Error(w, "unknown v", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(s.sample.Documents[version])
}

func (s *Server) handleTruth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(s.sample.TruthJSON)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	_ = s.store.Close()
	_ = os.Remove(s.dbPath)
	_ = os.Remove(s.dbPath + "-wal")
	_ = os.Remove(s.dbPath + "-shm")
	store, err := snapshot.Open(s.dbPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	wb, err := workbench.New(s.seed, store, workbench.DefaultFrameID)
	if err != nil {
		_ = store.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := wb.RunFull(ctx); err != nil {
		_ = store.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.store = store
	s.wb = wb
	s.injection = nil
	s.lastCompare = nil
	s.writeJSON(w, http.StatusOK, s.stateResponseLocked(ctx))
}

var _ = filepath.Join
