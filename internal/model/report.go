package model

type WindowMetric struct {
	WindowIndex   int   `json:"window_index"`
	Channel       int   `json:"channel"`
	StartSample   int   `json:"start_sample"`
	EndSample     int   `json:"end_sample"`
	Frame         int   `json:"frame"`
	SampleCount   int   `json:"sample_count"`
	PSNRDBFixed   int64 `json:"psnr_db_fixed"`
	PSNRInfinite  bool  `json:"psnr_infinite"`
	SNRDBFixed    int64 `json:"snr_db_fixed"`
	SNRInfinite   bool  `json:"snr_infinite"`
	SSIMFixed     int64 `json:"ssim_fixed"`
	NoiseStdFixed int64 `json:"noise_std_fixed"`
}

type ErrorBound struct {
	Frame                       int     `json:"frame"`
	QuantStep                   int64   `json:"quant_step"`
	FrameSize                   int     `json:"frame_size"`
	WindowSize                  int     `json:"window_size"`
	CoefficientFracBits         int     `json:"coefficient_frac_bits"`
	MetricScale                 int64   `json:"metric_scale"`
	CoeffErrorMax               float64 `json:"coeff_error_max"`
	TransformSampleErrorUpper   float64 `json:"transform_sample_error_upper"`
	ReconstructionRoundingUpper float64 `json:"reconstruction_rounding_upper"`
	SampleErrorUpper            float64 `json:"sample_error_upper"`
	RMSErrorUpperFixed          int64   `json:"rms_error_upper_fixed"`
	WindowEnergyUpper           float64 `json:"window_energy_upper"`
	WindowL1Upper               float64 `json:"window_l1_upper"`
	SNRMinimumDBFixed           int64   `json:"snr_minimum_db_fixed"`
	SNRUpperDBFixed             int64   `json:"snr_upper_db_fixed"`
	Formula                     string  `json:"formula"`
}

type FrameReport struct {
	Frame                 int              `json:"frame"`
	StartSample           int              `json:"start_sample"`
	PulseExpectedSample   int              `json:"pulse_expected_sample"`
	PulseObservedSample   int              `json:"pulse_observed_sample"`
	PulseMatch            bool             `json:"pulse_match"`
	SampleCount           int              `json:"sample_count"`
	PSNRDBFixed           int64            `json:"psnr_db_fixed"`
	PSNRInfinite          bool             `json:"psnr_infinite"`
	SNRDBFixed            int64            `json:"snr_db_fixed"`
	SNRInfinite           bool             `json:"snr_infinite"`
	SSIMFixed             int64            `json:"ssim_fixed"`
	NoiseFloorStdFixed    int64            `json:"noise_floor_std_fixed"`
	FrameResidualStdFixed int64            `json:"frame_residual_std_fixed"`
	Bound                 ErrorBound       `json:"bound"`
	Quant                 FrameQuantRecord `json:"quant"`
	WindowCount           int              `json:"window_count"`
	Heatmap               []int            `json:"heatmap,omitempty"`
}

type QueueRecord struct {
	BlockIndex         int    `json:"block_index"`
	Reason             string `json:"reason,omitempty"`
	EnqueueWaitNS      int64  `json:"enqueue_wait_ns"`
	ScheduleWaitNS     int64  `json:"schedule_wait_ns"`
	TotalWaitNS        int64  `json:"total_wait_ns"`
	WorkerID           int    `json:"worker_id"`
	QueueDepthAtSubmit int    `json:"queue_depth_at_submit"`
	BufferSamples      int    `json:"buffer_samples"`
	MaxBufferSamples   int    `json:"max_buffer_samples"`
}

type StageState struct {
	Stage     string `json:"stage"`
	State     string `json:"state"`
	UpdatedNS int64  `json:"updated_ns"`
	Detail    string `json:"detail,omitempty"`
}

type BlockResult struct {
	Block            BlockCoord    `json:"block"`
	Manifest         Manifest      `json:"manifest"`
	EvalParams       EvalParams    `json:"eval_params"`
	ContentHash      string        `json:"content_hash"`
	BlockHash        string        `json:"block_hash"`
	ParamVersion     string        `json:"param_version"`
	ParamHash        string        `json:"param_hash"`
	AlgorithmVersion string        `json:"algorithm_version"`
	Frames           []FrameReport `json:"frames"`
	Queue            QueueRecord   `json:"queue"`
	States           []StageState  `json:"states"`
	ReconSummary     SampleSummary `json:"recon_summary"`
	Original         []int32       `json:"original,omitempty"`
	Reconstructed    []int32       `json:"reconstructed,omitempty"`
	CommitSeq        int           `json:"commit_seq,omitempty"`
}

type SampleSummary struct {
	Count  int    `json:"count"`
	SHA256 string `json:"sha256"`
	Min    int32  `json:"min"`
	Max    int32  `json:"max"`
	Sum    int64  `json:"sum"`
	SumSq  int64  `json:"sum_sq"`
}

type Report struct {
	Identity          Identity           `json:"identity"`
	Manifest          Manifest           `json:"manifest"`
	EvalParams        EvalParams         `json:"eval_params"`
	Budget            Budget             `json:"budget"`
	Blocks            []BlockResult      `json:"blocks"`
	Frames            []FrameReport      `json:"frames"`
	CommitOrder       []int              `json:"commit_order"`
	Queue             []QueueRecord      `json:"queue"`
	Truncations       []TruncationRecord `json:"truncations"`
	GeneratedUnixNano int64              `json:"generated_unix_nano"`
}

type Budget struct {
	ChunkFrames      int `json:"chunk_frames"`
	MaxWorkers       int `json:"max_workers"`
	PerThreadSamples int `json:"per_thread_samples"`
	MaxPendingTasks  int `json:"max_pending_tasks"`
	SideBandSamples  int `json:"side_band_samples,omitempty"`
}
