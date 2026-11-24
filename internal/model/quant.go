package model

type CoeffError struct {
	Channel     int   `json:"channel"`
	Index       int   `json:"index"`
	BeforeFixed int64 `json:"before_fixed"`
	AfterFixed  int64 `json:"after_fixed"`
	ErrorFixed  int64 `json:"error_fixed"`
	StepFixed   int64 `json:"step_fixed"`
}

type BoundaryJump struct {
	BoundarySample int   `json:"boundary_sample"`
	FrameA         int   `json:"frame_a"`
	FrameB         int   `json:"frame_b"`
	ErrorBefore    int32 `json:"error_before"`
	ErrorAfter     int32 `json:"error_after"`
	Jump           int64 `json:"jump"`
}

type DiffusionPath struct {
	FromFrame      int     `json:"from_frame"`
	ToFrame        int     `json:"to_frame"`
	BoundarySample int     `json:"boundary_sample"`
	OverlapSamples int     `json:"overlap_samples"`
	Coupling       float64 `json:"coupling"`
	BoundaryJump   int64   `json:"boundary_jump"`
	Direction      string  `json:"direction"`
}

type FrameQuantRecord struct {
	Frame                 int             `json:"frame"`
	StartSample           int             `json:"start_sample"`
	QuantStep             int64           `json:"quant_step"`
	QuantStepFixed        int64           `json:"quant_step_fixed"`
	CoefficientFracBits   int             `json:"coefficient_frac_bits"`
	CoefficientCount      int             `json:"coefficient_count"`
	CoeffRMSErrorFixed    int64           `json:"coeff_rms_error_fixed"`
	CoeffMaxAbsErrorFixed int64           `json:"coeff_max_abs_error_fixed"`
	CoeffErrors           []CoeffError    `json:"coeff_errors,omitempty"`
	SampleErrorCount      int             `json:"sample_error_count"`
	SampleErrorSum        int64           `json:"sample_error_sum"`
	SampleErrorSumSq      int64           `json:"sample_error_sum_sq"`
	MaxAbsSampleError     int             `json:"max_abs_sample_error"`
	BoundaryJumps         []BoundaryJump  `json:"boundary_jumps,omitempty"`
	DiffusionPaths        []DiffusionPath `json:"diffusion_paths,omitempty"`
}
