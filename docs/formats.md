# Foreground Composite Format Mapping

The generator emits one layer source per version. `LoadFiles` merges them in source
order into the canonical stack. All three source declarations describe the same
24000-tick/second / 24-fps timeline, but use different units and field layouts.

## v1: Flat Studio Format

Top-level fields:

```text
format = foreground-composite/v1
scene.{name,width,height,channels,color_space,
       frame_rate_num,frame_rate_den,premultiplied}
layers[]
layer_order[]
```

Layer fields:

```text
id,name,kind,enabled,opacity,color,alpha,in_frame,out_frame,shape
```

Characteristics:

- one global `premultiplied` boolean,
- flat `layer_order`,
- rate expressed as integer fps numerator/denominator,
- no per-layer alpha convention,
- no hierarchy.

## v2: Foreground Composite 2.0

Top-level fields:

```text
format = foreground-composite/v2
spec_version = "2.0"
canvas.{width,height,channels}
working_color_space
alpha.mode
time.frame_rate.{numerator,denominator}, time.unit
layers[]
stack[]
```

Layer fields:

```text
uid,display_name,layer_type,
visibility.{visible,start_frame,until_frame},
hierarchy.{parent,order},
pixel.{color,opacity,alpha,encoding},
bounds.{kind,rect}
```

Characteristics:

- hierarchy expressed as a nullable parent reference,
- visible flag nested with lifecycle boundaries,
- global alpha mode,
- rational frame rate in seconds.

## v3: FC3

Top-level fields:

```text
format = foreground-composite/v3
schema_version = 3
canvas.{w,h,channels}
color.{space,alpha_mode,per_layer_alpha}
timing.{tick_rate,ticks_per_frame,unit}
layers[]
hierarchy.{roots,children,render_order}
```

Layer fields:

```text
layer_key,title,role,visible,lifecycle.{start,end},parent,
source.{color,encoding,alpha,premultiplied},
blend.opacity,mask.{type,rect}
```

Characteristics:

- per-layer premultiplication convention,
- nested root/child hierarchy plus explicit render order,
- visibility at the layer root,
- integer tick timeline.

On load, the 1000 ticks / 24000 tick-rate frame period is reduced to `1/24 s` for
timeline equivalence checks while the original tick declaration remains in
`SourceTimeBases`.

When exporting to v3, a 24 fps timeline is emitted as `tick_rate=24000` and
`ticks_per_frame=1000`, preserving the intended high-resolution tick expression.

## Error Envelope

```json
{
  "code": "E_SCHEMA",
  "message": "expected integer, got string",
  "path": "$.scene.width",
  "field": "width"
}
```

- `E_VER`: unknown/missing required version or export target.
- `E_SCHEMA`: field/type mismatch, including JSON path and field.
- `E_DECODE`: malformed JSON, corrupt packet or unreadable input.
- `E_PARA`: out-of-range parameter or buffer limit.
- `E_LAYER`: missing stack/hierarchy reference or cycle, with layer ID.
- `E_STATE`: illegal window state transition, with window number.
- `E_LOSSY`: warning code in an export degradation report; not a load failure.

## Bundle

`scene.bundle.json`:

```json
{
  "format": "virtual-stage-bundle/v1",
  "name": "Virtual Stage 42",
  "seed": 42,
  "sources": [
    "layers/layers-v1.json",
    "layers/layers-v2.json",
    "layers/layers-v3.json"
  ],
  "instructions": "instructions.json",
  "truth": "truth.json",
  "window_size": 6
}
```

Per-frame instructions use sparse overrides:

```json
{
  "format": "virtual-stage-instructions/v1",
  "frame_count": 24,
  "frames": [
    {"frame": 2, "layers": {"prop": {"opacity": 0.65}}}
  ]
}
```
