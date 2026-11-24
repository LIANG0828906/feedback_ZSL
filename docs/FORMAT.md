# Data and persistence formats

## 1. Canonical scene model

All input schemas are converted to the internal `scene.Scene` model:

```text
Scene
  id
  sourceVersion
  workingVersion = "3.0"
  spheres[] { id, center{x,y,z}, radius, albedo{r,g,b}, coat }
  planes[]  { id, normal{x,y,z}, offset, albedo{r,g,b}, coat }
  lights[]  { id, center, normal, up, halfWidth, halfHeight,
              radiantPowerW, color{r,g,b} }
  cameras[] { id, eye, lookAt, up, fovYRad, width, height }
  settings  { shadowSoftness, toneMapping }
  trace[]   { field, oldValue, defaultValue, defaultSource,
              fromVersion, toVersion }
```

Plane normal is normalized on load. Its offset is divided by the original normal
length, preserving `dot(n, x) = offset`. Light normals/up vectors are normalized.

## 2. Schema-specific fields

### 1.0

```json
{
  "schema_version": "1.0",
  "scene_id": "example",
  "objects": [
    {
      "type": "sphere",
      "id": "sphere_main",
      "center": [0, 0.6, 0],
      "radius": 0.6,
      "albedo": [0.8, 0.2, 0.2]
    }
  ],
  "lights": [
    {
      "kind": "rectangle",
      "id": "light_key",
      "center": [1, 4, 1],
      "normal": [0, -1, 0],
      "up": [0, 0, 1],
      "half_width": 1.1,
      "half_height": 0.85,
      "radiant_power_w": 80,
      "color": [1, 0.94, 0.86]
    }
  ],
  "cameras": [
    {
      "id": "camera_0",
      "eye": [-2, 2, 4],
      "look_at": [-1.5, 1.8, 3.1],
      "up": [0, 1, 0],
      "fov_deg": 38,
      "width": 28,
      "height": 28
    }
  ]
}
```

### 2.0

- Grouped under `geometry`, `lighting`, `asset`.
- Sphere uses `diameter` and `position`.
- Plane uses `normal_xyz` and `distance_from_origin`.
- Power is `radiant_power_mw`.
- Camera is a row-major `world_to_camera` matrix + `vertical_fov_rad`.
- Optional `target_distance_m` preserves the original `look_at` magnitude.

### 3.0

- `schemaVersion`, `assetId`, `nodes`.
- Sphere uses `radiusMeters` and `material.baseColor`.
- `material.clearCoat` is present.
- Rectangle light uses full `sizeMeters` and `luminousFluxLm`.
- Camera uses `positionXYZ` and `[w,x,y,z]` `orientationQuat`.
- Optional `lookAtDistanceMeters` preserves the original target distance.

### Unit conversion

```text
v1 radiant_power_w  = canonical W
v2 radiant_power_mw = canonical W * 1000
v3 luminousFluxLm   = canonical W * 683
```

`683 lm/W` is a fixed interchange convention, not a spectral renderer.

## 3. Target image format

Target files are deterministic big-endian binary:

```text
offset  size       content
0       8          "DRTGT01\n"
8       4          uint32 width
12      4          uint32 height
16      4          uint32 cameraID byte length
20      N          cameraID UTF-8
20+N    3*W*H*8    float64 RGB values, row-major
end-32  32         SHA-256 over every preceding byte
```

Readers validate magic, exact length, dimensions, camera ID, finite pixel values,
and checksum before accepting an image. A mismatch returns `E_DECODE` with a
field such as `target.checksum`.

## 4. Checkpoint directory

```text
checkpoints/<run>/
  .commit.lock
  manifest.json
  window-000000.json
  window-000001.json
  ...
```

`manifest.json` is the commit point:

```json
{
  "formatVersion": 1,
  "count": 3,
  "lastContentHash": "...",
  "sourceVersion": "1.0",
  "algorithmVersion": "photometric-consistent-sgd-v1",
  "taskHash": "...",
  "configHash": "..."
}
```

A window stores:

```json
{
  "formatVersion": 1,
  "window": 0,
  "state": {
    "iteration": 4,
    "loss": 1.23e-5,
    "stepScale": 1.0,
    "gradientNorm": 1.1e-4,
    "acceptedSteps": 4,
    "rejectedSteps": 0
  },
  "scene": { "...": "canonical scene" },
  "parameters": [0.1, 0.2],
  "parameterIds": ["sphere_main.center.x", "..."],
  "blockGradients": [
    {
      "view": 0,
      "index": 0,
      "start": 0,
      "end": 128,
      "values": [0.0, 0.0]
    }
  ],
  "losses": [1.5e-4, 1.2e-5],
  "sourceVersion": "1.0",
  "workingVersion": "3.0",
  "algorithmVersion": "photometric-consistent-sgd-v1",
  "taskHash": "...",
  "configHash": "...",
  "contentHash": "..."
}
```

`contentHash` is SHA-256 over canonical compact JSON of the checkpoint with the
`contentHash` field blanked. It covers scene parameters, optimizer state, block
gradients, losses, and all version hashes.

### Validation and recovery

Recovery rejects the whole snapshot if any of these fail:

- manifest JSON or format version;
- source/algorithm/task/config metadata;
- window file presence, JSON, or index;
- non-increasing window iteration;
- scene validation or parameter/scene equality;
- loss sequence length/monotonicity;
- block gradient length/finite values;
- content hash or manifest head.

The runner accepts only a contiguous validated prefix ending at the manifest head.
Any failure is logged to `invalidated.txt`, manifest/window files are removed, and
optimization restarts from iteration zero.

Writers use an exclusive file lock plus an in-process mutex. Files are written to
a temporary name, synced, renamed, and the containing directory is synced.

## 5. Configuration hashing

The config hash includes:

- learning rate, iterations, gradient clip, tolerance;
- window size, backtrack limit, minimum step;
- block pixel size and render retry count.

It excludes execution-only controls:

- `MaxWorkers`;
- `QueueCapacity`;
- `PauseAfterWindow`;
- `GradientHook`;
- injected test fault function.

This lets serial and parallel runs share a checkpoint identity while still
covering all numerical choices. Result equality is checked bit-for-bit.
