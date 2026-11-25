export namespace engine {
	
	export class CommitEntry {
	    glyph_id: string;
	    segment_id: number;
	    cache_key: string;
	
	    static createFrom(source: any = {}) {
	        return new CommitEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.glyph_id = source["glyph_id"];
	        this.segment_id = source["segment_id"];
	        this.cache_key = source["cache_key"];
	    }
	}
	export class PoolConfig {
	    MaxWorkers: number;
	    MaxBufferBytes: number;
	
	    static createFrom(source: any = {}) {
	        return new PoolConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.MaxWorkers = source["MaxWorkers"];
	        this.MaxBufferBytes = source["MaxBufferBytes"];
	    }
	}
	export class RenderStats {
	    rendered: number;
	    reused: number;
	    failed: number;
	    peak_buffer: number;
	    CommitOrder: CommitEntry[];
	
	    static createFrom(source: any = {}) {
	        return new RenderStats(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rendered = source["rendered"];
	        this.reused = source["reused"];
	        this.failed = source["failed"];
	        this.peak_buffer = source["peak_buffer"];
	        this.CommitOrder = this.convertValues(source["CommitOrder"], CommitEntry);
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

export namespace model {
	
	export class BBox {
	    min_x: number;
	    min_y: number;
	    max_x: number;
	    max_y: number;
	
	    static createFrom(source: any = {}) {
	        return new BBox(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.min_x = source["min_x"];
	        this.min_y = source["min_y"];
	        this.max_x = source["max_x"];
	        this.max_y = source["max_y"];
	    }
	}
	export class Point {
	    x: number;
	    y: number;
	
	    static createFrom(source: any = {}) {
	        return new Point(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.x = source["x"];
	        this.y = source["y"];
	    }
	}
	export class Segment {
	    id: string;
	    type: string;
	    p0: Point;
	    c1?: Point;
	    c2?: Point;
	    p1: Point;
	
	    static createFrom(source: any = {}) {
	        return new Segment(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.type = source["type"];
	        this.p0 = this.convertValues(source["p0"], Point);
	        this.c1 = this.convertValues(source["c1"], Point);
	        this.c2 = this.convertValues(source["c2"], Point);
	        this.p1 = this.convertValues(source["p1"], Point);
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
	export class Contour {
	    id: string;
	    closed: boolean;
	    segments: Segment[];
	
	    static createFrom(source: any = {}) {
	        return new Contour(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.closed = source["closed"];
	        this.segments = this.convertValues(source["segments"], Segment);
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
	export class FPoint {
	    x: number;
	    y: number;
	
	    static createFrom(source: any = {}) {
	        return new FPoint(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.x = source["x"];
	        this.y = source["y"];
	    }
	}
	export class SamplePointTruth {
	    stroke_segment_id: number;
	    point: FPoint;
	    density: number;
	
	    static createFrom(source: any = {}) {
	        return new SamplePointTruth(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.stroke_segment_id = source["stroke_segment_id"];
	        this.point = this.convertValues(source["point"], FPoint);
	        this.density = source["density"];
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
	export class GlyphTruth {
	    glyph_id: string;
	    sample_points: SamplePointTruth[];
	    closed_area: number;
	    bbox: BBox;
	    mask_width: number;
	    mask_height: number;
	    stroke_density: number;
	
	    static createFrom(source: any = {}) {
	        return new GlyphTruth(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.glyph_id = source["glyph_id"];
	        this.sample_points = this.convertValues(source["sample_points"], SamplePointTruth);
	        this.closed_area = source["closed_area"];
	        this.bbox = this.convertValues(source["bbox"], BBox);
	        this.mask_width = source["mask_width"];
	        this.mask_height = source["mask_height"];
	        this.stroke_density = source["stroke_density"];
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
	
	export class Polygon {
	    points: FPoint[];
	
	    static createFrom(source: any = {}) {
	        return new Polygon(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.points = this.convertValues(source["points"], FPoint);
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
	export class Quantization {
	    min: number;
	    max: number;
	    step: number;
	    unit: string;
	
	    static createFrom(source: any = {}) {
	        return new Quantization(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.min = source["min"];
	        this.max = source["max"];
	        this.step = source["step"];
	        this.unit = source["unit"];
	    }
	}
	
	
	export class StrokeParams {
	    width: number;
	    join: string;
	    cap: string;
	    spacing: number;
	    tolerance: number;
	    scale: number;
	    bbox_padding: number;
	
	    static createFrom(source: any = {}) {
	        return new StrokeParams(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.width = source["width"];
	        this.join = source["join"];
	        this.cap = source["cap"];
	        this.spacing = source["spacing"];
	        this.tolerance = source["tolerance"];
	        this.scale = source["scale"];
	        this.bbox_padding = source["bbox_padding"];
	    }
	}

}

export namespace service {
	
	export class SegmentView {
	    id: number;
	    contour_id: string;
	    state: string;
	    upstream: number[];
	    densities: number[];
	    boundary: model.FPoint[];
	    cache_key: string;
	    skeleton_polyline: model.FPoint[];
	    band_outline: model.Polygon;
	
	    static createFrom(source: any = {}) {
	        return new SegmentView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.contour_id = source["contour_id"];
	        this.state = source["state"];
	        this.upstream = source["upstream"];
	        this.densities = source["densities"];
	        this.boundary = this.convertValues(source["boundary"], model.FPoint);
	        this.cache_key = source["cache_key"];
	        this.skeleton_polyline = this.convertValues(source["skeleton_polyline"], model.FPoint);
	        this.band_outline = this.convertValues(source["band_outline"], model.Polygon);
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
	export class GlyphView {
	    id: string;
	    contours: model.Contour[];
	    skeleton: Record<string, Array<model.FPoint>>;
	    segments: SegmentView[];
	    mask_width: number;
	    mask_height: number;
	    mask_data_url: string;
	    truth: model.GlyphTruth;
	    bbox: model.BBox;
	
	    static createFrom(source: any = {}) {
	        return new GlyphView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.contours = this.convertValues(source["contours"], model.Contour);
	        this.skeleton = this.convertValues(source["skeleton"], Array<model.FPoint>, true);
	        this.segments = this.convertValues(source["segments"], SegmentView);
	        this.mask_width = source["mask_width"];
	        this.mask_height = source["mask_height"];
	        this.mask_data_url = source["mask_data_url"];
	        this.truth = this.convertValues(source["truth"], model.GlyphTruth);
	        this.bbox = this.convertValues(source["bbox"], model.BBox);
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
	
	export class SnapshotView {
	    algorithm: string;
	    seed: number;
	    params: model.StrokeParams;
	    quantization: model.Quantization;
	    glyphs: GlyphView[];
	    pool: engine.PoolConfig;
	
	    static createFrom(source: any = {}) {
	        return new SnapshotView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.algorithm = source["algorithm"];
	        this.seed = source["seed"];
	        this.params = this.convertValues(source["params"], model.StrokeParams);
	        this.quantization = this.convertValues(source["quantization"], model.Quantization);
	        this.glyphs = this.convertValues(source["glyphs"], GlyphView);
	        this.pool = this.convertValues(source["pool"], engine.PoolConfig);
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

