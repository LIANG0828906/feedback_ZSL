export namespace main {
	
	export class AppRunSummary {
	    mode: string;
	    decision: stage.ModeDecision;
	    progress: stage.Progress;
	    partial: boolean;
	    commit_order: number[];
	    budget_log: stage.BudgetRecord[];
	    statuses: stage.WindowStatus[];
	    checkpoint_invalidated: boolean;
	    invalidation_reason?: string;
	    // Go type: time
	    started_at: any;
	    // Go type: time
	    finished_at: any;
	
	    static createFrom(source: any = {}) {
	        return new AppRunSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.mode = source["mode"];
	        this.decision = this.convertValues(source["decision"], stage.ModeDecision);
	        this.progress = this.convertValues(source["progress"], stage.Progress);
	        this.partial = source["partial"];
	        this.commit_order = source["commit_order"];
	        this.budget_log = this.convertValues(source["budget_log"], stage.BudgetRecord);
	        this.statuses = this.convertValues(source["statuses"], stage.WindowStatus);
	        this.checkpoint_invalidated = source["checkpoint_invalidated"];
	        this.invalidation_reason = source["invalidation_reason"];
	        this.started_at = this.convertValues(source["started_at"], null);
	        this.finished_at = this.convertValues(source["finished_at"], null);
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
	export class AppState {
	    initialized: boolean;
	    project?: stage.Project;
	    params: stage.Params;
	    decision: stage.ModeDecision;
	    progress: stage.Progress;
	    windows: stage.WindowStatus[];
	    budget_log: stage.BudgetRecord[];
	    arbitrations: stage.Arbitration[];
	    merge_spans: stage.MergeSpan[];
	    last_run?: AppRunSummary;
	    last_verify?: stage.ConsistencyReport;
	    checkpoint_path: string;
	    truth_dirty: boolean;
	    running: boolean;
	    last_notice?: string;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new AppState(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.initialized = source["initialized"];
	        this.project = this.convertValues(source["project"], stage.Project);
	        this.params = this.convertValues(source["params"], stage.Params);
	        this.decision = this.convertValues(source["decision"], stage.ModeDecision);
	        this.progress = this.convertValues(source["progress"], stage.Progress);
	        this.windows = this.convertValues(source["windows"], stage.WindowStatus);
	        this.budget_log = this.convertValues(source["budget_log"], stage.BudgetRecord);
	        this.arbitrations = this.convertValues(source["arbitrations"], stage.Arbitration);
	        this.merge_spans = this.convertValues(source["merge_spans"], stage.MergeSpan);
	        this.last_run = this.convertValues(source["last_run"], AppRunSummary);
	        this.last_verify = this.convertValues(source["last_verify"], stage.ConsistencyReport);
	        this.checkpoint_path = source["checkpoint_path"];
	        this.truth_dirty = source["truth_dirty"];
	        this.running = source["running"];
	        this.last_notice = source["last_notice"];
	        this.error = source["error"];
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
	export class CuePatch {
	    time_ms?: number;
	    light_id?: string;
	    group_id?: string;
	    values?: stage.CueValues;
	    transition_ms?: number;
	    easing?: string;
	    curve_power?: number;
	    priority?: number;
	
	    static createFrom(source: any = {}) {
	        return new CuePatch(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.time_ms = source["time_ms"];
	        this.light_id = source["light_id"];
	        this.group_id = source["group_id"];
	        this.values = this.convertValues(source["values"], stage.CueValues);
	        this.transition_ms = source["transition_ms"];
	        this.easing = source["easing"];
	        this.curve_power = source["curve_power"];
	        this.priority = source["priority"];
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
	export class CueView {
	    id: string;
	    label: string;
	    time_ms: number;
	    light_id?: string;
	    group_id?: string;
	    kind: string;
	    values: stage.CueValues;
	    transition_ms: number;
	    easing: string;
	    priority: number;
	
	    static createFrom(source: any = {}) {
	        return new CueView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.label = source["label"];
	        this.time_ms = source["time_ms"];
	        this.light_id = source["light_id"];
	        this.group_id = source["group_id"];
	        this.kind = source["kind"];
	        this.values = this.convertValues(source["values"], stage.CueValues);
	        this.transition_ms = source["transition_ms"];
	        this.easing = source["easing"];
	        this.priority = source["priority"];
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
	export class GroupPatch {
	    members?: string[];
	    parent_id?: string;
	    priority?: number;
	
	    static createFrom(source: any = {}) {
	        return new GroupPatch(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.members = source["members"];
	        this.parent_id = source["parent_id"];
	        this.priority = source["priority"];
	    }
	}

}

export namespace stage {
	
	export class Candidate {
	    source_type: string;
	    source_id: string;
	    cue_id: string;
	    priority: number;
	    value: number;
	
	    static createFrom(source: any = {}) {
	        return new Candidate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.source_type = source["source_type"];
	        this.source_id = source["source_id"];
	        this.cue_id = source["cue_id"];
	        this.priority = source["priority"];
	        this.value = source["value"];
	    }
	}
	export class Arbitration {
	    time_us: number;
	    light_id: string;
	    field: string;
	    winner: string;
	    rule: string;
	    candidates: Candidate[];
	
	    static createFrom(source: any = {}) {
	        return new Arbitration(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.time_us = source["time_us"];
	        this.light_id = source["light_id"];
	        this.field = source["field"];
	        this.winner = source["winner"];
	        this.rule = source["rule"];
	        this.candidates = this.convertValues(source["candidates"], Candidate);
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
	export class BudgetRecord {
	    // Go type: time
	    at: any;
	    window: number;
	    event: string;
	    required_bytes: number;
	    available_bytes: number;
	    max_bytes: number;
	    wait_ms: number;
	    reason: string;
	
	    static createFrom(source: any = {}) {
	        return new BudgetRecord(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.at = this.convertValues(source["at"], null);
	        this.window = source["window"];
	        this.event = source["event"];
	        this.required_bytes = source["required_bytes"];
	        this.available_bytes = source["available_bytes"];
	        this.max_bytes = source["max_bytes"];
	        this.wait_ms = source["wait_ms"];
	        this.reason = source["reason"];
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
	
	export class ErrorDetail {
	    object?: string;
	    field?: string;
	    expected?: string;
	    actual?: string;
	    frame?: number;
	    time_ms?: number;
	    light_id?: string;
	    window?: number;
	    left?: string;
	    right?: string;
	    reason?: string;
	
	    static createFrom(source: any = {}) {
	        return new ErrorDetail(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.object = source["object"];
	        this.field = source["field"];
	        this.expected = source["expected"];
	        this.actual = source["actual"];
	        this.frame = source["frame"];
	        this.time_ms = source["time_ms"];
	        this.light_id = source["light_id"];
	        this.window = source["window"];
	        this.left = source["left"];
	        this.right = source["right"];
	        this.reason = source["reason"];
	    }
	}
	export class ModeDecision {
	    mode: string;
	    cue_count: number;
	    light_count: number;
	    frame_count: number;
	    estimated_ops: number;
	    preview_capacity: number;
	    max_exact_cues: number;
	    max_exact_ops: number;
	    preview_fps: number;
	    reasons: string[];
	    forced: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ModeDecision(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.mode = source["mode"];
	        this.cue_count = source["cue_count"];
	        this.light_count = source["light_count"];
	        this.frame_count = source["frame_count"];
	        this.estimated_ops = source["estimated_ops"];
	        this.preview_capacity = source["preview_capacity"];
	        this.max_exact_cues = source["max_exact_cues"];
	        this.max_exact_ops = source["max_exact_ops"];
	        this.preview_fps = source["preview_fps"];
	        this.reasons = source["reasons"];
	        this.forced = source["forced"];
	    }
	}
	export class Progress {
	    committed: number;
	    total: number;
	
	    static createFrom(source: any = {}) {
	        return new Progress(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.committed = source["committed"];
	        this.total = source["total"];
	    }
	}
	export class PathReport {
	    name: string;
	    progress: Progress;
	    commit_order: number[];
	    partial: boolean;
	    checkpoint_invalidated?: boolean;
	    budget_records?: BudgetRecord[];
	    duration: number;
	    decision: ModeDecision;
	
	    static createFrom(source: any = {}) {
	        return new PathReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.progress = this.convertValues(source["progress"], Progress);
	        this.commit_order = source["commit_order"];
	        this.partial = source["partial"];
	        this.checkpoint_invalidated = source["checkpoint_invalidated"];
	        this.budget_records = this.convertValues(source["budget_records"], BudgetRecord);
	        this.duration = source["duration"];
	        this.decision = this.convertValues(source["decision"], ModeDecision);
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
	export class ConsistencyReport {
	    project_id: string;
	    mode: string;
	    realtime: PathReport;
	    recovery: PathReport;
	    full: PathReport;
	    frames_compared: number;
	    windows_compared: number;
	    blocked_budget_events: number;
	    consistent: boolean;
	    first_divergence?: ErrorDetail;
	
	    static createFrom(source: any = {}) {
	        return new ConsistencyReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.project_id = source["project_id"];
	        this.mode = source["mode"];
	        this.realtime = this.convertValues(source["realtime"], PathReport);
	        this.recovery = this.convertValues(source["recovery"], PathReport);
	        this.full = this.convertValues(source["full"], PathReport);
	        this.frames_compared = source["frames_compared"];
	        this.windows_compared = source["windows_compared"];
	        this.blocked_budget_events = source["blocked_budget_events"];
	        this.consistent = source["consistent"];
	        this.first_divergence = this.convertValues(source["first_divergence"], ErrorDetail);
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
	export class RGB {
	    r: number;
	    g: number;
	    b: number;
	
	    static createFrom(source: any = {}) {
	        return new RGB(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.r = source["r"];
	        this.g = source["g"];
	        this.b = source["b"];
	    }
	}
	export class CueValues {
	    rgb?: RGB;
	    intensity?: number;
	    pan?: number;
	    tilt?: number;
	    focus?: number;
	    color_temp?: number;
	    strobe_hz?: number;
	
	    static createFrom(source: any = {}) {
	        return new CueValues(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rgb = this.convertValues(source["rgb"], RGB);
	        this.intensity = source["intensity"];
	        this.pan = source["pan"];
	        this.tilt = source["tilt"];
	        this.focus = source["focus"];
	        this.color_temp = source["color_temp"];
	        this.strobe_hz = source["strobe_hz"];
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
	export class Cue {
	    id: string;
	    label: string;
	    time_ms: number;
	    light_id?: string;
	    group_id?: string;
	    kind: string;
	    values: CueValues;
	    transition_ms: number;
	    easing: string;
	    curve_power: number;
	    priority: number;
	
	    static createFrom(source: any = {}) {
	        return new Cue(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.label = source["label"];
	        this.time_ms = source["time_ms"];
	        this.light_id = source["light_id"];
	        this.group_id = source["group_id"];
	        this.kind = source["kind"];
	        this.values = this.convertValues(source["values"], CueValues);
	        this.transition_ms = source["transition_ms"];
	        this.easing = source["easing"];
	        this.curve_power = source["curve_power"];
	        this.priority = source["priority"];
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
	
	
	export class LightOutput {
	    light_id: string;
	    rgb: RGB;
	    raw_rgb: RGB;
	    intensity: number;
	    pan: number;
	    tilt: number;
	    focus: number;
	    color_temp: number;
	    strobe_hz: number;
	    strobe_open: boolean;
	    x: number;
	    y: number;
	    z: number;
	
	    static createFrom(source: any = {}) {
	        return new LightOutput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.light_id = source["light_id"];
	        this.rgb = this.convertValues(source["rgb"], RGB);
	        this.raw_rgb = this.convertValues(source["raw_rgb"], RGB);
	        this.intensity = source["intensity"];
	        this.pan = source["pan"];
	        this.tilt = source["tilt"];
	        this.focus = source["focus"];
	        this.color_temp = source["color_temp"];
	        this.strobe_hz = source["strobe_hz"];
	        this.strobe_open = source["strobe_open"];
	        this.x = source["x"];
	        this.y = source["y"];
	        this.z = source["z"];
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
	export class FrameOutput {
	    frame: number;
	    time_us: number;
	    lights: LightOutput[];
	
	    static createFrom(source: any = {}) {
	        return new FrameOutput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.frame = source["frame"];
	        this.time_us = source["time_us"];
	        this.lights = this.convertValues(source["lights"], LightOutput);
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
	export class Group {
	    id: string;
	    name: string;
	    members?: string[];
	    parent_id?: string;
	    priority: number;
	
	    static createFrom(source: any = {}) {
	        return new Group(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.members = source["members"];
	        this.parent_id = source["parent_id"];
	        this.priority = source["priority"];
	    }
	}
	export class GroupSnapshot {
	    group_id: string;
	    members: string[];
	    priority: number;
	    last_cue?: string;
	    last_time_ms: number;
	    values: CueValues;
	
	    static createFrom(source: any = {}) {
	        return new GroupSnapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.group_id = source["group_id"];
	        this.members = source["members"];
	        this.priority = source["priority"];
	        this.last_cue = source["last_cue"];
	        this.last_time_ms = source["last_time_ms"];
	        this.values = this.convertValues(source["values"], CueValues);
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
	export class Range {
	    min: number;
	    max: number;
	
	    static createFrom(source: any = {}) {
	        return new Range(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.min = source["min"];
	        this.max = source["max"];
	    }
	}
	export class ParamRanges {
	    red: Range;
	    green: Range;
	    blue: Range;
	    intensity: Range;
	    pan: Range;
	    tilt: Range;
	    focus: Range;
	    color_temp: Range;
	    strobe_hz: Range;
	
	    static createFrom(source: any = {}) {
	        return new ParamRanges(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.red = this.convertValues(source["red"], Range);
	        this.green = this.convertValues(source["green"], Range);
	        this.blue = this.convertValues(source["blue"], Range);
	        this.intensity = this.convertValues(source["intensity"], Range);
	        this.pan = this.convertValues(source["pan"], Range);
	        this.tilt = this.convertValues(source["tilt"], Range);
	        this.focus = this.convertValues(source["focus"], Range);
	        this.color_temp = this.convertValues(source["color_temp"], Range);
	        this.strobe_hz = this.convertValues(source["strobe_hz"], Range);
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
	export class LightValues {
	    rgb: RGB;
	    intensity: number;
	    pan: number;
	    tilt: number;
	    focus: number;
	    color_temp: number;
	    strobe_hz: number;
	
	    static createFrom(source: any = {}) {
	        return new LightValues(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rgb = this.convertValues(source["rgb"], RGB);
	        this.intensity = source["intensity"];
	        this.pan = source["pan"];
	        this.tilt = source["tilt"];
	        this.focus = source["focus"];
	        this.color_temp = source["color_temp"];
	        this.strobe_hz = source["strobe_hz"];
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
	export class Vec3 {
	    x: number;
	    y: number;
	    z: number;
	
	    static createFrom(source: any = {}) {
	        return new Vec3(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.x = source["x"];
	        this.y = source["y"];
	        this.z = source["z"];
	    }
	}
	export class Light {
	    id: string;
	    name: string;
	    kind: string;
	    layout: Vec3;
	    base: LightValues;
	    ranges: ParamRanges;
	
	    static createFrom(source: any = {}) {
	        return new Light(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	        this.layout = this.convertValues(source["layout"], Vec3);
	        this.base = this.convertValues(source["base"], LightValues);
	        this.ranges = this.convertValues(source["ranges"], ParamRanges);
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
	
	
	export class MergeSpan {
	    first_cue_index: number;
	    last_cue_index: number;
	    first_cue_id: string;
	    last_cue_id: string;
	    start_us: number;
	    end_us: number;
	    key_frames: number[];
	    error_bound: number;
	
	    static createFrom(source: any = {}) {
	        return new MergeSpan(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.first_cue_index = source["first_cue_index"];
	        this.last_cue_index = source["last_cue_index"];
	        this.first_cue_id = source["first_cue_id"];
	        this.last_cue_id = source["last_cue_id"];
	        this.start_us = source["start_us"];
	        this.end_us = source["end_us"];
	        this.key_frames = source["key_frames"];
	        this.error_bound = source["error_bound"];
	    }
	}
	export class MergeInfo {
	    mode: string;
	    basis: string;
	    preview_fps: number;
	    frame_stride: number;
	    spans: MergeSpan[];
	    max_observed_error: number;
	
	    static createFrom(source: any = {}) {
	        return new MergeInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.mode = source["mode"];
	        this.basis = source["basis"];
	        this.preview_fps = source["preview_fps"];
	        this.frame_stride = source["frame_stride"];
	        this.spans = this.convertValues(source["spans"], MergeSpan);
	        this.max_observed_error = source["max_observed_error"];
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
	
	
	
	export class Params {
	    window_ms: number;
	    preview_fps: number;
	    max_exact_cues: number;
	    max_exact_ops: number;
	    max_workers: number;
	    per_worker_buffer_bytes: number;
	    memory_budget_bytes: number;
	    color_tolerance: number;
	    position_tolerance: number;
	    other_tolerance: number;
	    force_mode?: string;
	
	    static createFrom(source: any = {}) {
	        return new Params(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.window_ms = source["window_ms"];
	        this.preview_fps = source["preview_fps"];
	        this.max_exact_cues = source["max_exact_cues"];
	        this.max_exact_ops = source["max_exact_ops"];
	        this.max_workers = source["max_workers"];
	        this.per_worker_buffer_bytes = source["per_worker_buffer_bytes"];
	        this.memory_budget_bytes = source["memory_budget_bytes"];
	        this.color_tolerance = source["color_tolerance"];
	        this.position_tolerance = source["position_tolerance"];
	        this.other_tolerance = source["other_tolerance"];
	        this.force_mode = source["force_mode"];
	    }
	}
	
	
	export class Tolerance {
	    color: number;
	    position: number;
	    intensity: number;
	    focus: number;
	    color_temp: number;
	    strobe_hz: number;
	
	    static createFrom(source: any = {}) {
	        return new Tolerance(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.color = source["color"];
	        this.position = source["position"];
	        this.intensity = source["intensity"];
	        this.focus = source["focus"];
	        this.color_temp = source["color_temp"];
	        this.strobe_hz = source["strobe_hz"];
	    }
	}
	export class TruthLight {
	    light_id: string;
	    rgb: RGB;
	    intensity: number;
	    pan: number;
	    tilt: number;
	    focus: number;
	    color_temp: number;
	    strobe_hz: number;
	
	    static createFrom(source: any = {}) {
	        return new TruthLight(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.light_id = source["light_id"];
	        this.rgb = this.convertValues(source["rgb"], RGB);
	        this.intensity = source["intensity"];
	        this.pan = source["pan"];
	        this.tilt = source["tilt"];
	        this.focus = source["focus"];
	        this.color_temp = source["color_temp"];
	        this.strobe_hz = source["strobe_hz"];
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
	export class TruthSample {
	    frame: number;
	    time_ms: number;
	    lights: TruthLight[];
	
	    static createFrom(source: any = {}) {
	        return new TruthSample(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.frame = source["frame"];
	        this.time_ms = source["time_ms"];
	        this.lights = this.convertValues(source["lights"], TruthLight);
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
	export class Truth {
	    total_duration_ms: number;
	    sample_times_ms: number[];
	    samples: TruthSample[];
	    group_order: string[];
	    tolerance: Tolerance;
	
	    static createFrom(source: any = {}) {
	        return new Truth(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.total_duration_ms = source["total_duration_ms"];
	        this.sample_times_ms = source["sample_times_ms"];
	        this.samples = this.convertValues(source["samples"], TruthSample);
	        this.group_order = source["group_order"];
	        this.tolerance = this.convertValues(source["tolerance"], Tolerance);
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
	export class Scene {
	    id: string;
	    name: string;
	    cue_ids: string[];
	
	    static createFrom(source: any = {}) {
	        return new Scene(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.cue_ids = source["cue_ids"];
	    }
	}
	export class Project {
	    schema_version: number;
	    id: string;
	    name: string;
	    seed: number;
	    fps: number;
	    duration_ms: number;
	    window_ms: number;
	    lights: Light[];
	    groups: Group[];
	    cues: Cue[];
	    scenes: Scene[];
	    truth: Truth;
	
	    static createFrom(source: any = {}) {
	        return new Project(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.schema_version = source["schema_version"];
	        this.id = source["id"];
	        this.name = source["name"];
	        this.seed = source["seed"];
	        this.fps = source["fps"];
	        this.duration_ms = source["duration_ms"];
	        this.window_ms = source["window_ms"];
	        this.lights = this.convertValues(source["lights"], Light);
	        this.groups = this.convertValues(source["groups"], Group);
	        this.cues = this.convertValues(source["cues"], Cue);
	        this.scenes = this.convertValues(source["scenes"], Scene);
	        this.truth = this.convertValues(source["truth"], Truth);
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
	
	
	
	
	
	
	
	
	export class WindowResult {
	    index: number;
	    start_us: number;
	    end_us: number;
	    state: string;
	    frames: FrameOutput[];
	    cue_start: number;
	    cue_offset: number;
	    consumed_cues: number;
	    groups: GroupSnapshot[];
	    decisions: Arbitration[];
	    merge?: MergeInfo;
	    content_hash: string;
	    param_version: string;
	    algorithm_version: string;
	
	    static createFrom(source: any = {}) {
	        return new WindowResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.index = source["index"];
	        this.start_us = source["start_us"];
	        this.end_us = source["end_us"];
	        this.state = source["state"];
	        this.frames = this.convertValues(source["frames"], FrameOutput);
	        this.cue_start = source["cue_start"];
	        this.cue_offset = source["cue_offset"];
	        this.consumed_cues = source["consumed_cues"];
	        this.groups = this.convertValues(source["groups"], GroupSnapshot);
	        this.decisions = this.convertValues(source["decisions"], Arbitration);
	        this.merge = this.convertValues(source["merge"], MergeInfo);
	        this.content_hash = source["content_hash"];
	        this.param_version = source["param_version"];
	        this.algorithm_version = source["algorithm_version"];
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
	export class WindowStatus {
	    index: number;
	    start_us: number;
	    end_us: number;
	    state: string;
	    attempts: number;
	    cue_start: number;
	    cue_offset: number;
	    consumed_cues: number;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new WindowStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.index = source["index"];
	        this.start_us = source["start_us"];
	        this.end_us = source["end_us"];
	        this.state = source["state"];
	        this.attempts = source["attempts"];
	        this.cue_start = source["cue_start"];
	        this.cue_offset = source["cue_offset"];
	        this.consumed_cues = source["consumed_cues"];
	        this.error = source["error"];
	    }
	}

}

