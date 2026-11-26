package vpf

// LoadProject reads any supported VPF schema version and maps it to the
// canonical internal layer-stack model.
func LoadProject(data []byte) (*Project, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, err
	}

	versionPath := "$.version"
	rawVersion, exists := root["version"]
	if !exists || rawVersion == nil {
		versionPath = "$.format_version"
		rawVersion, exists = root["format_version"]
	}
	if !exists || rawVersion == nil {
		return nil, schemaError(0, "$.version", "required integer field is missing")
	}
	version64, ok := integerValue(rawVersion)
	if !ok {
		return nil, schemaError(0, versionPath, "expected an integer schema version")
	}
	version := int(version64)
	if version < SchemaV1 || version > SchemaV3 {
		return nil, schemaError(version, versionPath, "unsupported schema version; supported versions are 1,2,3")
	}

	switch version {
	case SchemaV1:
		return loadV1(root)
	case SchemaV2:
		return loadV2(root)
	case SchemaV3:
		return loadV3(root)
	default:
		return nil, schemaError(version, "$.version", "unsupported schema version")
	}
}

func finalizeLoadedProject(p *Project) error {
	if p.Channels == 0 {
		p.Channels = 4
	}
	if p.TileWidth <= 0 {
		p.TileWidth = 32
	}
	if p.TileHeight <= 0 {
		p.TileHeight = 32
	}
	if p.StackSnapshotVersion == 0 {
		p.StackSnapshotVersion = 1
	}
	if p.RippleSourceVersion == 0 {
		p.RippleSourceVersion = 1
	}
	for i := range p.Layers {
		if p.Layers[i].ParamVersion == 0 {
			p.Layers[i].ParamVersion = 1
		}
		for j := range p.Layers[i].Ripples {
			if p.Layers[i].Ripples[j].ParamVersion == 0 {
				p.Layers[i].Ripples[j].ParamVersion = 1
			}
		}
		if p.Layers[i].ContentHash == "" {
			p.Layers[i].ContentHash = LayerContentHash(p.Layers[i])
		}
	}
	if p.Root.ID == "" {
		p.Root = Node{ID: "root", Kind: NodeGroup, Visible: true}
	}
	return p.RecalculateStack()
}
