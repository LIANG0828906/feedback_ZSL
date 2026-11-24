export namespace app {
	
	export class ExportVerification {
	    report: pipeline.ExportReport;
	    round_trip_ok: boolean;
	    stack_equal: boolean;
	    visibility_equal: boolean;
	    deterministic: boolean;
	    warnings: pipeline.LossyWarning[];
	    reloaded_source_version: number;
	
	    static createFrom(source: any = {}) {
	        return new ExportVerification(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.report = this.convertValues(source["report"], pipeline.ExportReport);
	        this.round_trip_ok = source["round_trip_ok"];
	        this.stack_equal = source["stack_equal"];
	        this.visibility_equal = source["visibility_equal"];
	        this.deterministic = source["deterministic"];
	        this.warnings = this.convertValues(source["warnings"], pipeline.LossyWarning);
	        this.reloaded_source_version = source["reloaded_source_version"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Heatmap {
	    available: boolean;
	    window: number;
	    frame: number;
	    cells_x: number;
	    cells_y: number;
	    max_delta: number;
	    diff: number[][];
	    message?: string;
	
	    static createFrom(source: any = {}) {
	        return new Heatmap(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.available = source["available"];
	        this.window = source["window"];
	        this.frame = source["frame"];
	        this.cells_x = source["cells_x"];
	        this.cells_y = source["cells_y"];
	        this.max_delta = source["max_delta"];
	        this.diff = source["diff"];
	        this.message = source["message"];
	    }
	}
	export class Snapshot {
	    model?: pipeline.Model;
	    windows: pipeline.Window[];
	    committed_windows: number;
	    total_windows: number;
	    source_hash: string;
	    model_hash: string;
	    recovery: pipeline.RecoveryResult;
	    last_invalidation: pipeline.InvalidationList;
	
	    static createFrom(source: any = {}) {
	        return new Snapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.model = this.convertValues(source["model"], pipeline.Model);
	        this.windows = this.convertValues(source["windows"], pipeline.Window);
	        this.committed_windows = source["committed_windows"];
	        this.total_windows = source["total_windows"];
	        this.source_hash = source["source_hash"];
	        this.model_hash = source["model_hash"];
	        this.recovery = this.convertValues(source["recovery"], pipeline.RecoveryResult);
	        this.last_invalidation = this.convertValues(source["last_invalidation"], pipeline.InvalidationList);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TruthValidation {
	    ok: boolean;
	    frames_checked: number;
	    samples_checked: number;
	    max_abs_error: number;
	    source_versions: number[];
	    layer_versions: number[];
	    layer_order: string[];
	    errors: string[];
	
	    static createFrom(source: any = {}) {
	        return new TruthValidation(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.frames_checked = source["frames_checked"];
	        this.samples_checked = source["samples_checked"];
	        this.max_abs_error = source["max_abs_error"];
	        this.source_versions = source["source_versions"];
	        this.layer_versions = source["layer_versions"];
	        this.layer_order = source["layer_order"];
	        this.errors = source["errors"];
	    }
	}

}

export namespace main {
	
	export class EditResult {
	    invalidation: pipeline.InvalidationList;
	    snapshot: app.Snapshot;
	
	    static createFrom(source: any = {}) {
	        return new EditResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.invalidation = this.convertValues(source["invalidation"], pipeline.InvalidationList);
	        this.snapshot = this.convertValues(source["snapshot"], app.Snapshot);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace pipeline {
	
	export class Canvas {
	    width: number;
	    height: number;
	    channels: number;
	    color_space: string;
	
	    static createFrom(source: any = {}) {
	        return new Canvas(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.width = source["width"];
	        this.height = source["height"];
	        this.channels = source["channels"];
	        this.color_space = source["color_space"];
	    }
	}
	export class DegradationTrace {
	    layer_id?: string;
	    from: string;
	    to: string;
	    path: string;
	
	    static createFrom(source: any = {}) {
	        return new DegradationTrace(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.layer_id = source["layer_id"];
	        this.from = source["from"];
	        this.to = source["to"];
	        this.path = source["path"];
	    }
	}
	export class Rational {
	    num: number;
	    den: number;
	
	    static createFrom(source: any = {}) {
	        return new Rational(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.num = source["num"];
	        this.den = source["den"];
	    }
	}
	export class TimeBase {
	    unit: string;
	    frame_period: Rational;
	    ticks_per_frame?: number;
	    tick_rate?: number;
	
	    static createFrom(source: any = {}) {
	        return new TimeBase(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.unit = source["unit"];
	        this.frame_period = this.convertValues(source["frame_period"], Rational);
	        this.ticks_per_frame = source["ticks_per_frame"];
	        this.tick_rate = source["tick_rate"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class EditRequest {
	    kind: string;
	    layer_id?: string;
	    opacity?: number;
	    alpha?: number;
	    color?: number[];
	    visible?: boolean;
	    time_base?: TimeBase;
	
	    static createFrom(source: any = {}) {
	        return new EditRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.layer_id = source["layer_id"];
	        this.opacity = source["opacity"];
	        this.alpha = source["alpha"];
	        this.color = source["color"];
	        this.visible = source["visible"];
	        this.time_base = this.convertValues(source["time_base"], TimeBase);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LossyWarning {
	    code: string;
	    layer_id?: string;
	    property: string;
	    from: string;
	    to: string;
	    message: string;
	    trace_path?: string;
	
	    static createFrom(source: any = {}) {
	        return new LossyWarning(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.code = source["code"];
	        this.layer_id = source["layer_id"];
	        this.property = source["property"];
	        this.from = source["from"];
	        this.to = source["to"];
	        this.message = source["message"];
	        this.trace_path = source["trace_path"];
	    }
	}
	export class ExportReport {
	    target_version: number;
	    format: string;
	    layer_file: string;
	    content_hash: string;
	    warnings: LossyWarning[];
	    traces: DegradationTrace[];
	    layer_order: string[];
	
	    static createFrom(source: any = {}) {
	        return new ExportReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.target_version = source["target_version"];
	        this.format = source["format"];
	        this.layer_file = source["layer_file"];
	        this.content_hash = source["content_hash"];
	        this.warnings = this.convertValues(source["warnings"], LossyWarning);
	        this.traces = this.convertValues(source["traces"], DegradationTrace);
	        this.layer_order = source["layer_order"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LayerOverride {
	    visible?: boolean;
	    opacity?: number;
	    alpha?: number;
	    color?: number[];
	
	    static createFrom(source: any = {}) {
	        return new LayerOverride(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.visible = source["visible"];
	        this.opacity = source["opacity"];
	        this.alpha = source["alpha"];
	        this.color = source["color"];
	    }
	}
	export class FrameInstruction {
	    frame: number;
	    layers?: Record<string, LayerOverride>;
	
	    static createFrom(source: any = {}) {
	        return new FrameInstruction(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.frame = source["frame"];
	        this.layers = this.convertValues(source["layers"], LayerOverride, true);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class FrameSummary {
	    frame: number;
	    timestamp: Rational;
	    pixel_count: number;
	    mean_luma: number;
	    min_luma: number;
	    max_luma: number;
	    mean_alpha: number;
	    content_hash: string;
	
	    static createFrom(source: any = {}) {
	        return new FrameSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.frame = source["frame"];
	        this.timestamp = this.convertValues(source["timestamp"], Rational);
	        this.pixel_count = source["pixel_count"];
	        this.mean_luma = source["mean_luma"];
	        this.min_luma = source["min_luma"];
	        this.max_luma = source["max_luma"];
	        this.mean_alpha = source["mean_alpha"];
	        this.content_hash = source["content_hash"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Invalidation {
	    window: number;
	    layer_ids: string[];
	    reason: string;
	    stage: string;
	
	    static createFrom(source: any = {}) {
	        return new Invalidation(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.window = source["window"];
	        this.layer_ids = source["layer_ids"];
	        this.reason = source["reason"];
	        this.stage = source["stage"];
	    }
	}
	export class InvalidationList {
	    operation: string;
	    affected: Invalidation[];
	    reused: number[];
	    total_windows: number;
	    total_frames: number;
	
	    static createFrom(source: any = {}) {
	        return new InvalidationList(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.operation = source["operation"];
	        this.affected = this.convertValues(source["affected"], Invalidation);
	        this.reused = source["reused"];
	        this.total_windows = source["total_windows"];
	        this.total_frames = source["total_frames"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Shape {
	    type: string;
	    rect?: number[];
	    radius?: number;
	
	    static createFrom(source: any = {}) {
	        return new Shape(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.rect = source["rect"];
	        this.radius = source["radius"];
	    }
	}
	export class Layer {
	    id: string;
	    name: string;
	    kind: string;
	    parent_id?: string;
	    visible: boolean;
	    in_frame: number;
	    out_frame: number;
	    visible_until_frame: number;
	    opacity: number;
	    alpha: number;
	    color: number[];
	    color_encoding: string;
	    alpha_mode: string;
	    shape: Shape;
	    source_version: number;
	
	    static createFrom(source: any = {}) {
	        return new Layer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	        this.parent_id = source["parent_id"];
	        this.visible = source["visible"];
	        this.in_frame = source["in_frame"];
	        this.out_frame = source["out_frame"];
	        this.visible_until_frame = source["visible_until_frame"];
	        this.opacity = source["opacity"];
	        this.alpha = source["alpha"];
	        this.color = source["color"];
	        this.color_encoding = source["color_encoding"];
	        this.alpha_mode = source["alpha_mode"];
	        this.shape = this.convertValues(source["shape"], Shape);
	        this.source_version = source["source_version"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	
	export class SourceRef {
	    path: string;
	    version: number;
	    format: string;
	
	    static createFrom(source: any = {}) {
	        return new SourceRef(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.version = source["version"];
	        this.format = source["format"];
	    }
	}
	export class Model {
	    name: string;
	    source_version: number;
	    source_versions: number[];
	    sources?: SourceRef[];
	    canvas: Canvas;
	    time_base: TimeBase;
	    source_time_bases?: TimeBase[];
	    layers: Layer[];
	    stack: string[];
	    frame_count: number;
	    instructions?: FrameInstruction[];
	
	    static createFrom(source: any = {}) {
	        return new Model(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.source_version = source["source_version"];
	        this.source_versions = source["source_versions"];
	        this.sources = this.convertValues(source["sources"], SourceRef);
	        this.canvas = this.convertValues(source["canvas"], Canvas);
	        this.time_base = this.convertValues(source["time_base"], TimeBase);
	        this.source_time_bases = this.convertValues(source["source_time_bases"], TimeBase);
	        this.layers = this.convertValues(source["layers"], Layer);
	        this.stack = source["stack"];
	        this.frame_count = source["frame_count"];
	        this.instructions = this.convertValues(source["instructions"], FrameInstruction);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class SourceSnapshot {
	    versions: number[];
	    time_base: TimeBase;
	    canvas: Canvas;
	
	    static createFrom(source: any = {}) {
	        return new SourceSnapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.versions = source["versions"];
	        this.time_base = this.convertValues(source["time_base"], TimeBase);
	        this.canvas = this.convertValues(source["canvas"], Canvas);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class WindowSnapshot {
	    window_index: number;
	    start_frame: number;
	    end_frame: number;
	    layers: Layer[];
	    source: SourceSnapshot;
	    model_hash: string;
	    source_hash: string;
	    algorithm_version: string;
	    frame_hash: string;
	    frame_path: string;
	    summary: FrameSummary;
	    frame_summaries?: FrameSummary[];
	    committed_sequence: number;
	    committed_at: string;
	
	    static createFrom(source: any = {}) {
	        return new WindowSnapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.window_index = source["window_index"];
	        this.start_frame = source["start_frame"];
	        this.end_frame = source["end_frame"];
	        this.layers = this.convertValues(source["layers"], Layer);
	        this.source = this.convertValues(source["source"], SourceSnapshot);
	        this.model_hash = source["model_hash"];
	        this.source_hash = source["source_hash"];
	        this.algorithm_version = source["algorithm_version"];
	        this.frame_hash = source["frame_hash"];
	        this.frame_path = source["frame_path"];
	        this.summary = this.convertValues(source["summary"], FrameSummary);
	        this.frame_summaries = this.convertValues(source["frame_summaries"], FrameSummary);
	        this.committed_sequence = source["committed_sequence"];
	        this.committed_at = source["committed_at"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RecoveryResult {
	    restored: WindowSnapshot[];
	    purged: boolean;
	    reason?: string;
	    checked: number;
	
	    static createFrom(source: any = {}) {
	        return new RecoveryResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.restored = this.convertValues(source["restored"], WindowSnapshot);
	        this.purged = source["purged"];
	        this.reason = source["reason"];
	        this.checked = source["checked"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	
	
	
	export class Window {
	    index: number;
	    start_frame: number;
	    end_frame: number;
	    state: string;
	    snapshot?: WindowSnapshot;
	    failure?: string;
	    invalidated: boolean;
	    reused: boolean;
	    invalidation?: Invalidation;
	
	    static createFrom(source: any = {}) {
	        return new Window(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.index = source["index"];
	        this.start_frame = source["start_frame"];
	        this.end_frame = source["end_frame"];
	        this.state = source["state"];
	        this.snapshot = this.convertValues(source["snapshot"], WindowSnapshot);
	        this.failure = source["failure"];
	        this.invalidated = source["invalidated"];
	        this.reused = source["reused"];
	        this.invalidation = this.convertValues(source["invalidation"], Invalidation);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

