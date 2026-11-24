package editorpick

// AttachGeometry validates and attaches a geometry to an existing node, then
// commits a precise dependency invalidation.
func (e *Engine) AttachGeometry(nodeID string, dto GeometryDTO) (*InvalidationReport, error) {
	parsed, err := parseGeometries([]GeometryDTO{dto})
	if err != nil {
		return nil, err
	}
	g := parsed[0]

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.scene.NodeByID[nodeID]; !ok {
		return nil, newRef(nodeID, "cannot attach geometry to missing node")
	}
	next := e.scene.clone()
	idx := len(next.Geometries)
	g.Index = idx
	next.Geometries = append(next.Geometries, g)
	next.NodeByID[nodeID].GeometryIndex = idx
	e.graph.AddEdge(geometryObject(idx), entryObject(nodeID), "geometry-content")
	return e.commitChangesLocked(next, []Change{
		{ObjectID: worldObject(nodeID), Reason: "geometry-add"},
		{ObjectID: geometryObject(idx), Reason: "geometry-add"},
	})
}

// RemoveGeometry detaches the node geometry. The immutable geometry asset is
// retained in the scene list to preserve stable geometry indices.
func (e *Engine) RemoveGeometry(nodeID string) (*InvalidationReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	node, ok := e.scene.NodeByID[nodeID]
	if !ok {
		return nil, newRef(nodeID, "cannot remove geometry from missing node")
	}
	if node.GeometryIndex < 0 {
		return nil, newRef(nodeID, "node has no attached geometry")
	}
	next := e.scene.clone()
	next.NodeByID[nodeID].GeometryIndex = -1
	return e.commitChangesLocked(next, []Change{
		{ObjectID: worldObject(nodeID), Reason: "geometry-remove"},
	})
}
