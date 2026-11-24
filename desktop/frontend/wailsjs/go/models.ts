export namespace model {
	
	export class AppError {
	    code: string;
	    message: string;
	    field?: string;
	    axis?: string;
	    endpoint?: number;
	    actual?: any;
	    allowed?: any;
	    details?: Record<string, any>;
	
	    static createFrom(source: any = {}) {
	        return new AppError(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.code = source["code"];
	        this.message = source["message"];
	        this.field = source["field"];
	        this.axis = source["axis"];
	        this.endpoint = source["endpoint"];
	        this.actual = source["actual"];
	        this.allowed = source["allowed"];
	        this.details = source["details"];
	    }
	}

}

export namespace service {
	
	export class APIResult {
	    ok: boolean;
	    data?: any;
	    error?: model.AppError;
	
	    static createFrom(source: any = {}) {
	        return new APIResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.data = source["data"];
	        this.error = this.convertValues(source["error"], model.AppError);
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

