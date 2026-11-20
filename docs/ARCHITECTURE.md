# Architecture and Numerical Model

## Processing graph

The pipeline is a deterministic dependency graph. Each node's identity includes its relevant configuration and upstream content hashes, never wall-clock metadata.

```text
measurement file hash
  -> load
  -> align(channel, reference, threshold settings)
  -> segmented FFT band analysis(frame, overlap, window, band layout)
  -> per-band smoothing(log-frequency kernel width)
  -> per-band target correction(target curve, clamp limits)
  -> per-band filter grid segment
  -> per-channel linear-phase FIR
  -> per-band error propagation budget
  -> multichannel consistency diagnostics
```

The pipeline computes candidate results off the UI thread. The coordinator installs the result only after completion. The UI continues to paint the previous `ProcessResult` while a job is active. Failure and cancellation do not mutate the committed result.

## 1. WAV and direct-sound alignment

WAV input must be uncompressed 16-bit PCM at 48 kHz with mono, stereo, or 5.1 channel layout. Files are read with a context-managed `wave` handle; no handle survives the reader.

For channel signal `x[n]`:

```text
peak        = max |x[n]|
noise_sigma = 1.4826 * median(|x[0:128] - median(x[0:128])|)
threshold   = max(noise_sigma * sigma_factor, peak * peak_ratio, tiny)
arrival     = first index where |x[n]| > threshold
offset      = arrival(channel) - arrival(reference)
gain        = direct_peak(reference) / direct_peak(channel)
```

The aligned signal is integer-shifted by `-offset` and multiplied by `gain`. Arrival uncertainty is recorded as `0.5` samples. The output keeps the arrival, threshold, noise sigma, direct peak, normalization gain, and old/new offset delta when a previous committed result exists.

## 2. Segmented FFT and band estimation

For frame size `N`, overlap `r`, and segment count `M`, window `w[n]` has coherent gain:

```text
Cg = (1/N) * sum(w[n])
```

Frame starts are deterministic, and every frame is zero-padded to `N`. FFT frame phases are referred back to global sample zero with `exp(+j 2 pi k start / N)`. The complex mean transfer estimate is:

```text
H[k] = mean_m(FFT(x_m[n] w[n]) * exp(+j 2 pi k start_m / N))
```

For a band `B`, normalized bin weights `u_k` aggregate power:

```text
P_B = sum_k u_k * mean_m |2 FFT_m[k] / (N Cg)|^2
phase_B = angle(sum_k u_k * H[k])
```

Quantization amplitude per frame and band is bounded by:

```text
q_amp = 2 * (delta / sqrt(12)) * sqrt(sum(w^2)) / (N Cg sqrt(M))
q_power = 2 * sqrt(P_B) * q_amp * sqrt(sum(u_k^2))
```

where `delta = 1/32768` in normalized full-scale units. The segment term is a configurable safety factor times the standard error of frame band powers:

```text
segment_bound = safety * std(frame_power_B, ddof=1) / sqrt(M)
```

For one segment, the segment term is zero. The analysis stores `frame_starts`, `segment_count`, per-band segment variance, coherent gain, and bin L2 weights so the bound can be reconstructed.

## 3. Band smoothing and coupling

Let `d_ij = |log2(f_i / f_j)|` and `sigma = smoothing_octaves / 2.354820045...` (FWHM conversion). Initial Gaussian weights are:

```text
w_ij = exp(-0.5 * (d_ij / sigma)^2)
```

Weights below `1e-4` of the maximum are removed, the center band is retained, and the remainder is renormalized. The smoothed power and propagated bounds are:

```text
P_j' = sum_i w_ij P_i
Q_j' = sum_i w_ij Q_i
S_j' = sum_i w_ij S_i
C_j  = sum_{i != j} w_ij (Q_i + S_i)
```

`C_j` is the smoothing-coupling bound. The complete weight vector and contributing band labels are stored per band.

## 4. Target correction and linear-phase FIR

Each channel's mid-band median level is subtracted before applying the target, making compensation relative to spectral shape rather than absolute FFT scale:

```text
level = median(smoothed_db[100 Hz .. 10 kHz])
correction = clamp(target_db(f) - (smoothed_db(f) - level), cut, boost)
```

The target curve is interpolated in log2 frequency and validated for monotonic frequencies, finite values, and gains in `[-60, 60]` dB. A target failing validation raises `E_FILTER_INVALID` with `parameter="target_curve"`.

The correction is sampled on an FFT grid. A centered zero-phase response is shifted to the origin, truncated symmetrically to an odd tap count, and windowed. Symmetry gives:

```text
group_delay_samples = (filter_taps - 1) / 2
group_delay_seconds = group_delay_samples / sample_rate
```

The actual FIR response is evaluated at every band center. Truncation error is the absolute dB difference from the desired correction. Tap counts outside `[63, 8193]` or even tap counts raise `E_FILTER_INVALID` with `parameter="filter_taps"`.

Filter segments are cached as correction intervals between neighboring band centers, plus the below-first and above-last regions. If an endpoint correction changes, only the adjacent segments are invalidated; the frequency-grid assembly and one aggregate inverse FFT then produce the new channel FIR.

## 5. Error ledger

For band frequency `f`, residual alignment uncertainty `tau = 0.5` samples gives:

```text
A_f = (2 sin(pi f tau / fs))^2
```

The power-fraction terms are:

```text
alignment    = A_f
quantization = Q_j' / P_j'
segmentation = S_j' / P_j'
smoothing    = C_j / P_j'
truncation   = 10^(truncation_db / 10) - 1
```

Each term is converted independently and summed in dB:

```text
term_db(node) = 10 log10(1 + fraction(node))
total_db      = sum(term_db)
```

The status is `E_ERROR_BUDGET` when `total_db > threshold`, and `dominant_node` is the maximum term. Because the report stores both fractions and dB terms, an independent verifier can recompute every result.

## 6. Multichannel consistency

For every non-reference channel and band:

- relative gain is `channel_db - reference_db`;
- relative phase is wrapped to `[-180, 180]` degrees;
- phase differences above the configured threshold are `phase_mismatch`, and above 150 degrees are `phase_inversion`;
- adjacent-band gain changes above the jump threshold are reported as gain jumps.

For low-frequency jumps, the diagnostic compares the alignment, smoothing-coupling, and filter-truncation terms from the same error node. It names the largest modeled source and includes all three competing values, the alignment offset, the maximum smoothing neighbor weight, filter group delay, and total error bound.

## 7. Cache and publication

The cache is content-addressed. A key is:

```text
SHA256(stage + canonical identity + upstream content hashes + config subset)
```

Canonical hashing preserves integer/float types and uses hexadecimal float encoding internally. Cache files are written atomically via a temporary file and `os.replace`. A node is reused only if the decoded payload's hash still matches the stored content hash.

The publication hash excludes traces, cache-hit flags, version numbers, and offset deltas. Those are diagnostic or version metadata. This makes an unchanged processed result retain the same content hash across repeated versions. Version numbers increment only when a new result is committed.

## 8. GUI concurrency

`ProcessingCoordinator` owns one worker thread and one cancellation event. `submit()` rejects an active job with `E_BUSY`. The Qt main thread polls progress and completions with a timer; it never joins or sleeps on the worker. The WAV reader, cache writer, and report writer all close handles through context managers.

