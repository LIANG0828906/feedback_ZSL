package audiofp

// Injection describes deterministic fault injection. It is intentionally part
// of the public surface so both tests and the demo drive the same error path.
type Injection struct {
	Kind     string `json:"kind"` // truncate, frame_order, recipe, stage
	AssetID  string `json:"asset_id,omitempty"`
	Shard    int    `json:"shard,omitempty"`
	Frame    int    `json:"frame,omitempty"`
	FailOnce bool   `json:"fail_once,omitempty"`
	Message  string `json:"message,omitempty"`
}
