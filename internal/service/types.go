package service

import (
	"strata/internal/model"
	"strata/internal/pipeline"
	"strata/internal/render"
)

type Envelope struct {
	OK       bool            `json:"ok"`
	Data     any             `json:"data,omitempty"`
	Error    *model.AppError `json:"error,omitempty"`
	Warnings []model.Warning `json:"warnings,omitempty"`
}

type Notify struct {
	ID        int    `json:"id"`
	Level     string `json:"level"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	Timestamp string `json:"timestamp"`
}

type MeshDTO struct {
	Positions   []float64    `json:"positions"`
	Normals     []float64    `json:"normals"`
	Colors      []int        `json:"colors"`
	Indices     []int        `json:"indices"`
	Chunks      []ChunkDTO   `json:"chunks"`
	Bounds      model.Bounds `json:"bounds"`
	ContentHash string       `json:"contentHash"`
	Algorithm   string       `json:"algorithm"`
	SampleRate  float64      `json:"sampleRate"`
	BuildMode   string       `json:"buildMode"`
}

type ChunkDTO struct {
	ID          string       `json:"id"`
	Row         int          `json:"row"`
	Col         int          `json:"col"`
	Bounds      model.Bounds `json:"bounds"`
	VertexStart int          `json:"vertexStart"`
	VertexCount int          `json:"vertexCount"`
	FaceStart   int          `json:"faceStart"`
	FaceCount   int          `json:"faceCount"`
	PointCount  int          `json:"pointCount"`
	Reason      string       `json:"reason"`
	Rebuilt     bool         `json:"rebuilt"`
	Reused      bool         `json:"reused"`
	CacheHit    bool         `json:"cacheHit"`
	ContentHash string       `json:"contentHash"`
	Error       string       `json:"error,omitempty"`
}

type ChunkManifest struct {
	Version   int        `json:"version"`
	Total     int        `json:"total"`
	Rebuilt   int        `json:"rebuilt"`
	Reused    int        `json:"reused"`
	CacheHits int        `json:"cacheHits"`
	Affected  int        `json:"affected"`
	Items     []ChunkDTO `json:"items"`
	Sequence  []string   `json:"sequence"`
}

type SourceDTO struct {
	VersionID          string             `json:"versionId"`
	VersionNumber      int                `json:"versionNumber"`
	DisplayName        string             `json:"displayName"`
	Format             string             `json:"format"`
	Encoding           string             `json:"encoding"`
	File               string             `json:"file"`
	FileBytes          int64              `json:"fileBytes"`
	PointCount         int                `json:"pointCount"`
	FaceCount          int                `json:"faceCount"`
	Bounds             model.Bounds       `json:"bounds"`
	FieldOrder         []string           `json:"fieldOrder"`
	OptionalAttributes []string           `json:"optionalAttributes"`
	Fields             []model.FieldSpec  `json:"fields"`
	Quantization       model.Quantization `json:"quantization"`
	HeaderMeta         model.HeaderMeta   `json:"headerMeta"`
	HasNormals         bool               `json:"hasNormals"`
	HasColors          bool               `json:"hasColors"`
	HasIntensity       bool               `json:"hasIntensity"`
}

type StatsDTO struct {
	SourcePoints   int     `json:"sourcePoints"`
	PipelinePoints int     `json:"pipelinePoints"`
	Outliers       int     `json:"outliers"`
	Vertices       int     `json:"vertices"`
	Faces          int     `json:"faces"`
	Chunks         int     `json:"chunks"`
	PreviewLatency float64 `json:"previewLatencyMs"`
	ExactLatency   float64 `json:"exactLatencyMs"`
}

type FrameDTO struct {
	ID              string                 `json:"id"`
	Version         int                    `json:"version"`
	Mode            string                 `json:"mode"`
	AlgorithmMode   string                 `json:"algorithmMode"`
	RenderMode      string                 `json:"renderMode"`
	SampleRate      float64                `json:"sampleRate"`
	Width           int                    `json:"width"`
	Height          int                    `json:"height"`
	Camera          render.Camera          `json:"camera"`
	Clip            render.ClipPlane       `json:"clip"`
	Coverage        float64                `json:"coverage"`
	BBoxProjection  render.ProjectedBounds `json:"bboxProjection"`
	NormalHistogram []float64              `json:"normalHistogram"`
	LatencyMs       float64                `json:"latencyMs"`
	WithinBudget    bool                   `json:"withinBudget"`
	ImageURL        string                 `json:"imageUrl,omitempty"`
}

type WorkspaceDTO struct {
	Product        string              `json:"product"`
	DatasetName    string              `json:"datasetName"`
	Seed           int                 `json:"seed"`
	ActiveVersion  string              `json:"activeVersion"`
	Version        int                 `json:"version"`
	Ready          bool                `json:"ready"`
	Versions       []model.VersionInfo `json:"versions"`
	Source         SourceDTO           `json:"source"`
	Truth          model.Truth         `json:"truth"`
	Stats          StatsDTO            `json:"stats"`
	Params         pipeline.Config     `json:"params"`
	Mesh           MeshDTO             `json:"mesh"`
	Chunks         []ChunkDTO          `json:"chunks"`
	Camera         render.Camera       `json:"camera"`
	Clip           render.ClipPlane    `json:"clip"`
	LastComparison *render.Comparison  `json:"lastComparison,omitempty"`
	LastPreview    *FrameDTO           `json:"lastPreview,omitempty"`
	LastExact      *FrameDTO           `json:"lastExact,omitempty"`
	Cache          map[string]any      `json:"cache"`
}

type PreviewRequest struct {
	Camera *render.Camera    `json:"camera,omitempty"`
	Clip   *render.ClipPlane `json:"clip,omitempty"`
}

type ExactResult struct {
	RunID         string             `json:"runId"`
	Version       int                `json:"version"`
	Preview       *FrameDTO          `json:"preview"`
	Exact         *FrameDTO          `json:"exact"`
	Comparison    *render.Comparison `json:"comparison"`
	ChunkSequence []string           `json:"chunkSequence"`
	MeshHash      string             `json:"meshHash"`
	FrozenChunks  int                `json:"frozenChunks"`
	CheckpointMs  float64            `json:"checkpointMs"`
	BuildMs       float64            `json:"buildMs"`
}

type EditRequest struct {
	Index       int               `json:"index"`
	DeltaZ      float64           `json:"deltaZ"`
	BaseVersion int               `json:"baseVersion,omitempty"`
	Camera      *render.Camera    `json:"camera,omitempty"`
	Clip        *render.ClipPlane `json:"clip,omitempty"`
}

type IncrementalResult struct {
	Version   int           `json:"version"`
	Changed   int           `json:"changedChunks"`
	Manifest  ChunkManifest `json:"manifest"`
	MeshHash  string        `json:"meshHash"`
	ElapsedMs float64       `json:"elapsedMs"`
}

type ParamsRequest struct {
	Params pipeline.Config   `json:"params"`
	Camera *render.Camera    `json:"camera,omitempty"`
	Clip   *render.ClipPlane `json:"clip,omitempty"`
}

type ChunkParamRequest struct {
	ChunkID string  `json:"chunkId"`
	Key     string  `json:"key"`
	Value   float64 `json:"value"`
}

type ExportRequest struct {
	VersionID string `json:"versionId"`
	Kind      string `json:"kind"`
}

type ExportResult struct {
	VersionID      string          `json:"versionId"`
	Kind           string          `json:"kind"`
	Path           string          `json:"path"`
	Bytes          int64           `json:"bytes"`
	PointCount     int             `json:"pointCount"`
	ReadbackPoints int             `json:"readbackPoints"`
	FaceCount      int             `json:"faceCount"`
	ReadbackFaces  int             `json:"readbackFaces"`
	Warnings       []model.Warning `json:"warnings"`
	DowngradeTrace []string        `json:"downgradeTrace"`
	RoundTrip      bool            `json:"roundTrip"`
}

type PathResult struct {
	Passed              bool              `json:"passed"`
	FullSequence        []string          `json:"fullSequence"`
	IncrementalSequence []string          `json:"incrementalSequence"`
	ResumeSequence      []string          `json:"resumeSequence"`
	PreviewSequence     []string          `json:"previewSequence"`
	FullMeshHash        string            `json:"fullMeshHash"`
	IncrementalMeshHash string            `json:"incrementalMeshHash"`
	ResumeMeshHash      string            `json:"resumeMeshHash"`
	PreviewMeshHash     string            `json:"previewMeshHash"`
	FirstDifferingChunk string            `json:"firstDifferingChunk,omitempty"`
	Runs                map[string]string `json:"runs"`
}

type JobDTO struct {
	ID        string   `json:"id"`
	RunID     string   `json:"runId,omitempty"`
	Mode      string   `json:"mode"`
	Version   int      `json:"version"`
	Status    string   `json:"status"`
	Committed int      `json:"committed"`
	Total     int      `json:"total"`
	Sequence  []string `json:"sequence"`
	Error     string   `json:"error,omitempty"`
	StartedAt string   `json:"startedAt"`
	UpdatedAt string   `json:"updatedAt"`
}
