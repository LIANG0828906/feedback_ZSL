package httpui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"

	"wardrobe/dependency"
	"wardrobe/diagnostics"
	"wardrobe/domain"
	"wardrobe/engine"
	"wardrobe/schema"
)

type Server struct {
	engine *engine.Engine
	mux    *http.ServeMux
}

type ProjectSummary struct {
	ProjectID   string                  `json:"projectId"`
	Name        string                  `json:"name"`
	Revision    int                     `json:"revision"`
	Constraints domain.BatchConstraints `json:"constraints"`
	Cases       []CaseSummary           `json:"cases"`
	MountCount  int                     `json:"mountCount"`
	PartCount   int                     `json:"partCount"`
}

type CaseSummary struct {
	CaseID      string `json:"caseId"`
	Description string `json:"description"`
	Valid       bool   `json:"valid"`
	BatchCount  int    `json:"batchCount"`
}

type MountView struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	ParentID        string   `json:"parentId,omitempty"`
	SkinMode        string   `json:"skinMode"`
	AllPartIDs      []string `json:"allPartIds"`
	SelectedPartIDs []string `json:"selectedPartIds"`
	AcceptedPartIDs []string `json:"acceptedPartIds"`
}

type PartView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	MountID       string   `json:"mountId"`
	Skinned       bool     `json:"skinned"`
	BatchPriority int      `json:"batchPriority"`
	VertexCount   int      `json:"vertexCount"`
	TriangleCount int      `json:"triangleCount"`
	MaterialSlots []string `json:"materialSlots"`
	Selected      bool     `json:"selected"`
	Accepted      bool     `json:"accepted"`
	BatchID       string   `json:"batchId,omitempty"`
}

type MaterialUsage struct {
	SlotID   string   `json:"slotId"`
	Name     string   `json:"name"`
	Version  int      `json:"version"`
	BatchIDs []string `json:"batchIds"`
	PartIDs  []string `json:"partIds"`
}

type GraphNode struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	State  string `json:"state,omitempty"`
	Parent string `json:"parent,omitempty"`
}

type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type GraphView struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

type Diagnostics struct {
	Project         ProjectSummary        `json:"project"`
	TruthCase       domain.ExpectedCase   `json:"truthCase"`
	Result          domain.CaseResult     `json:"result"`
	Mounts          []MountView           `json:"mounts"`
	Parts           []PartView            `json:"parts"`
	Materials       []MaterialUsage       `json:"materials"`
	Graph           GraphView             `json:"graph"`
	DependencyGraph dependency.Graph      `json:"dependencyGraph"`
	TruthMismatch   *diagnostics.Mismatch `json:"truthMismatch,omitempty"`
}

type APIError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Issues  []schema.Issue `json:"issues,omitempty"`
}

func New(engineUnderTest *engine.Engine) *Server {
	server := &Server{engine: engineUnderTest, mux: http.NewServeMux()}
	server.mux.HandleFunc("/", server.handleIndex)
	server.mux.HandleFunc("/api/health", server.handleHealth)
	server.mux.HandleFunc("/api/projects", server.handleProjects)
	server.mux.HandleFunc("/api/diagnostics", server.handleDiagnostics)
	server.mux.HandleFunc("/api/validate-assembly", server.handleValidateAssembly)
	server.mux.HandleFunc("/api/edit-part", server.handleEditPart)
	server.mux.HandleFunc("/api/tri-compare", server.handleTriCompare)
	server.mux.HandleFunc("/api/retry-batch", server.handleRetryBatch)
	server.mux.HandleFunc("/api/recover", server.handleRecover)
	return server
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.mux.ServeHTTP(writer, request)
}

func (s *Server) handleIndex(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src 'self' data:")
	_, _ = io.WriteString(writer, indexHTML)
}

func (s *Server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":           "ok",
		"algorithmVersion": domain.AlgorithmVersion,
		"schemaVersion":    domain.SchemaVersion,
		"projects":         len(s.engine.ProjectIDs()),
	})
}

func (s *Server) handleProjects(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	projects := s.engine.Projects()
	out := make([]ProjectSummary, 0, len(projects))
	for _, project := range projects {
		out = append(out, summarizeProject(project))
	}
	writeJSON(writer, http.StatusOK, out)
}

func (s *Server) handleDiagnostics(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	projectID := request.URL.Query().Get("project")
	caseID := request.URL.Query().Get("case")
	if projectID == "" {
		projects := s.engine.ProjectIDs()
		if len(projects) == 0 {
			writeError(writer, http.StatusNotFound, "E_PROJECT", "no projects loaded", nil)
			return
		}
		projectID = projects[0]
	}
	if caseID == "" {
		cases := s.engine.CaseIDs(projectID)
		if len(cases) == 0 {
			writeError(writer, http.StatusNotFound, "E_CASE", "project has no cases", nil)
			return
		}
		caseID = cases[0]
	}
	diagnostic, err := s.buildDiagnostics(projectID, caseID)
	if err != nil {
		writeEngineError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, diagnostic)
}

func (s *Server) handleValidateAssembly(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	var body struct {
		ProjectID       string   `json:"projectId"`
		SelectedPartIDs []string `json:"selectedPartIds"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "E_JSON", err.Error(), nil)
		return
	}
	result, err := s.engine.ValidateAssembly(body.ProjectID, body.SelectedPartIDs)
	if err != nil {
		writeEngineError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) handleEditPart(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	var edit engine.EditRequest
	if err := decodeJSON(request, &edit); err != nil {
		writeError(writer, http.StatusBadRequest, "E_JSON", err.Error(), nil)
		return
	}
	report, err := s.engine.EditPart(edit)
	if err != nil {
		writeEngineError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func (s *Server) handleTriCompare(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	var edit engine.EditRequest
	if err := decodeJSON(request, &edit); err != nil {
		writeError(writer, http.StatusBadRequest, "E_JSON", err.Error(), nil)
		return
	}
	report, err := s.engine.TriCompare(edit.ProjectID, edit.CaseID, edit)
	if err != nil {
		writeEngineError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func (s *Server) handleRetryBatch(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	var body struct {
		ProjectID string `json:"projectId"`
		CaseID    string `json:"caseId"`
		BatchID   string `json:"batchId"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "E_JSON", err.Error(), nil)
		return
	}
	batch, err := s.engine.RetryBatch(body.ProjectID, body.CaseID, body.BatchID)
	if err != nil {
		writeEngineError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, batch)
}

func (s *Server) handleRecover(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	var body struct {
		ProjectID string `json:"projectId"`
		CaseID    string `json:"caseId"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "E_JSON", err.Error(), nil)
		return
	}
	report, err := s.engine.RecoverCase(body.ProjectID, body.CaseID)
	if err != nil {
		writeEngineError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func (s *Server) buildDiagnostics(projectID, caseID string) (Diagnostics, error) {
	project, ok := s.engine.Project(projectID)
	if !ok {
		return Diagnostics{}, engine.ErrProjectNotFound
	}
	expected, ok := s.engine.TruthCase(projectID, caseID)
	if !ok {
		return Diagnostics{}, engine.ErrCaseNotFound
	}
	result, err := s.engine.EnsureCase(projectID, caseID)
	if err != nil {
		return Diagnostics{}, err
	}
	mismatch := diagnostics.CompareExpected(expected, result)
	selected := make(map[string]bool)
	accepted := make(map[string]bool)
	batchForPart := make(map[string]string)
	for _, id := range result.SelectedPartIDs {
		selected[id] = true
	}
	for _, id := range result.AcceptedPartIDs {
		accepted[id] = true
	}
	for _, batch := range result.Batches {
		for _, id := range batch.PartIDs {
			batchForPart[id] = batch.ID
		}
	}
	mounts := make([]MountView, 0, len(project.Mounts))
	allPartsByMount := make(map[string][]string)
	for _, part := range project.Parts {
		allPartsByMount[part.MountID] = append(allPartsByMount[part.MountID], part.ID)
	}
	for _, mount := range project.Mounts {
		all := append([]string(nil), allPartsByMount[mount.ID]...)
		sort.Strings(all)
		selectedIDs := make([]string, 0)
		acceptedIDs := make([]string, 0)
		for _, id := range all {
			if selected[id] {
				selectedIDs = append(selectedIDs, id)
			}
			if accepted[id] {
				acceptedIDs = append(acceptedIDs, id)
			}
		}
		mounts = append(mounts, MountView{
			ID: mount.ID, Name: mount.Name, ParentID: mount.ParentID, SkinMode: mount.SkinMode,
			AllPartIDs: all, SelectedPartIDs: selectedIDs, AcceptedPartIDs: acceptedIDs,
		})
	}
	parts := make([]PartView, 0, len(project.Parts))
	for _, part := range project.Parts {
		slots := make([]string, 0, len(part.Mesh.Submeshes))
		for _, submesh := range part.Mesh.Submeshes {
			slots = append(slots, submesh.MaterialSlot)
		}
		parts = append(parts, PartView{
			ID: part.ID, Name: part.Name, MountID: part.MountID, Skinned: part.Skinned,
			BatchPriority: part.BatchPriority, VertexCount: part.Mesh.VertexCount,
			TriangleCount: part.Mesh.TriangleCount, MaterialSlots: domain.SortedUnique(slots),
			Selected: selected[part.ID], Accepted: accepted[part.ID], BatchID: batchForPart[part.ID],
		})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].ID < parts[j].ID })

	materials := buildMaterialUsage(project, result)
	graph := buildGraph(project, selected, accepted, batchForPart)
	dependencyGraph := dependency.Build(project, result)
	return Diagnostics{
		Project:         summarizeProject(project),
		TruthCase:       expected,
		Result:          result,
		Mounts:          mounts,
		Parts:           parts,
		Materials:       materials,
		Graph:           graph,
		DependencyGraph: dependencyGraph,
		TruthMismatch:   mismatch,
	}, nil
}

func summarizeProject(project domain.Project) ProjectSummary {
	cases := make([]CaseSummary, 0, len(project.Truth.Cases))
	for _, truthCase := range project.Truth.Cases {
		cases = append(cases, CaseSummary{
			CaseID: truthCase.CaseID, Description: truthCase.Description,
			Valid: truthCase.Valid, BatchCount: len(truthCase.Batches),
		})
	}
	return ProjectSummary{
		ProjectID: project.ProjectID, Name: project.Name, Revision: project.Revision,
		Constraints: project.Constraints, Cases: cases,
		MountCount: len(project.Mounts), PartCount: len(project.Parts),
	}
}

func buildMaterialUsage(project domain.Project, result domain.CaseResult) []MaterialUsage {
	materialMap := project.MaterialMap()
	bySlot := make(map[string]*MaterialUsage)
	for _, batch := range result.Batches {
		for _, placement := range batch.Parts {
			for _, submesh := range placement.Submeshes {
				usage, ok := bySlot[submesh.MaterialSlot]
				if !ok {
					material := materialMap[submesh.MaterialSlot]
					usage = &MaterialUsage{
						SlotID: submesh.MaterialSlot, Name: material.Name, Version: material.Version,
						BatchIDs: []string{}, PartIDs: []string{},
					}
					bySlot[submesh.MaterialSlot] = usage
				}
				if !containsString(usage.BatchIDs, batch.ID) {
					usage.BatchIDs = append(usage.BatchIDs, batch.ID)
				}
				if !containsString(usage.PartIDs, placement.PartID) {
					usage.PartIDs = append(usage.PartIDs, placement.PartID)
				}
			}
		}
	}
	out := make([]MaterialUsage, 0, len(bySlot))
	for _, usage := range bySlot {
		sort.Strings(usage.BatchIDs)
		sort.Strings(usage.PartIDs)
		out = append(out, *usage)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SlotID < out[j].SlotID })
	return out
}

func buildGraph(project domain.Project, selected, accepted map[string]bool, batchForPart map[string]string) GraphView {
	nodes := make([]GraphNode, 0, len(project.Mounts)+len(project.Parts))
	edges := make([]GraphEdge, 0, len(project.Mounts)+len(project.Parts)+len(project.ExclusionRules))
	for _, mount := range project.Mounts {
		nodes = append(nodes, GraphNode{
			ID: "mount:" + mount.ID, Kind: "mount", Label: mount.Name,
			State: mount.SkinMode, Parent: mount.ParentID,
		})
		if mount.ParentID != "" {
			edges = append(edges, GraphEdge{From: "mount:" + mount.ParentID, To: "mount:" + mount.ID, Kind: "hierarchy"})
		}
	}
	for _, part := range project.Parts {
		state := "idle"
		if selected[part.ID] {
			state = "selected"
		}
		if accepted[part.ID] {
			state = "accepted"
		}
		if batchID := batchForPart[part.ID]; batchID != "" {
			state = "batched:" + batchID
		}
		nodes = append(nodes, GraphNode{ID: "part:" + part.ID, Kind: "part", Label: part.Name, State: state})
		edges = append(edges, GraphEdge{From: "mount:" + part.MountID, To: "part:" + part.ID, Kind: "mount"})
	}
	for _, rule := range project.ExclusionRules {
		edges = append(edges, GraphEdge{From: "part:" + rule.PartA, To: "part:" + rule.PartB, Kind: "exclusion"})
	}
	return GraphView{Nodes: nodes, Edges: edges}
}

func decodeJSON(request *http.Request, target any) error {
	contentType := request.Header.Get("Content-Type")
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			return fmt.Errorf("Content-Type must be application/json")
		}
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request body must contain exactly one JSON value")
	}
	return nil
}

func writeEngineError(writer http.ResponseWriter, err error) {
	var validationError *schema.ValidationError
	if errors.As(err, &validationError) {
		writeJSON(writer, http.StatusUnprocessableEntity, APIError{
			Code: schema.CodeSchema, Message: "schema validation failed", Issues: validationError.Issues,
		})
		return
	}
	switch {
	case errors.Is(err, engine.ErrProjectNotFound):
		writeError(writer, http.StatusNotFound, "E_PROJECT_NOT_FOUND", err.Error(), nil)
	case errors.Is(err, engine.ErrCaseNotFound):
		writeError(writer, http.StatusNotFound, "E_CASE_NOT_FOUND", err.Error(), nil)
	case errors.Is(err, engine.ErrPartNotFound):
		writeError(writer, http.StatusNotFound, "E_PART_NOT_FOUND", err.Error(), nil)
	default:
		writeError(writer, http.StatusUnprocessableEntity, "E_ENGINE", err.Error(), nil)
	}
}

func writeError(writer http.ResponseWriter, status int, code, message string, issues []schema.Issue) {
	writeJSON(writer, status, APIError{Code: code, Message: message, Issues: issues})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func methodNotAllowed(writer http.ResponseWriter, allowed string) {
	writer.Header().Set("Allow", allowed)
	writeError(writer, http.StatusMethodNotAllowed, "E_METHOD", "method not allowed", nil)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
