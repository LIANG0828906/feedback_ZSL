vcl 4.1;

# edgecheck generated VCL - cache key strategy: host-url-vary-v1
# hash inputs: req.url, req.http.host (client protocol NOT included)

backend backend_a {
    .host = "127.0.0.1";
    .port = "27381";
}
backend backend_b {
    .host = "127.0.0.1";
    .port = "27382";
}
backend backend_c_bridge {
    .host = "127.0.0.1";
    .port = "27384";
}

sub vcl_recv {
    if (req.url ~ "^/a/") {
        set req.backend_hint = backend_a;
    } else if (req.url ~ "^/b/") {
        set req.backend_hint = backend_b;
    } else if (req.url ~ "^/c/") {
        set req.backend_hint = backend_c_bridge;
    }

    # HEAD is never cached: backend B deliberately declares a wrong
    # Content-Length on HEAD and must not pollute GET cache objects.
    if (req.method == "HEAD") {
        return (pass);
    }
    # Conditional revalidation is passed to the origin so a 304 is a real
    # end-to-end revalidation and never becomes a new cache fill.
    if (req.http.if-none-match || req.http.if-modified-since) {
        return (pass);
    }

    # Canonicalize Accept-Encoding so Vary buckets are stable and shared
    # between HTTP/1.1 and HTTP/2 clients. (Varnish is started with
    # -p http_gzip_support=off so Vary: Accept-Encoding is honored instead
    # of being collapsed by the core gzip layer.)
    if (req.http.Accept-Encoding ~ "gzip") {
        set req.http.Accept-Encoding = "gzip";
    } else {
        unset req.http.Accept-Encoding;
    }
}

sub vcl_hash {
    # strategy host-url-vary-v1: identical to Varnish defaults, declared
    # explicitly. req.proto is intentionally absent.
    hash_data(req.url);
    if (req.http.host) {
        hash_data(req.http.host);
    } else {
        hash_data(server.ip);
    }
    return (lookup);
}

sub vcl_backend_response {
    # Only cache clean 200s. Error responses (including upstream h2c
    # failures surfaced as 502) and no-store responses are never stored.
    if (beresp.status != 200) {
        set beresp.ttl = 0s;
        set beresp.uncacheable = true;
        return (deliver);
    }
    if (beresp.http.Cache-Control ~ "(no-store|no-cache|private)") {
        set beresp.ttl = 0s;
        set beresp.uncacheable = true;
        return (deliver);
    }
    set beresp.ttl = 300s;
    set beresp.grace = 0s;
    return (deliver);
}

sub vcl_deliver {
    if (obj.hits > 0) {
        set resp.http.X-Cache = "HIT";
    } else {
        set resp.http.X-Cache = "MISS";
    }
    set resp.http.X-Cache-Key-Strategy = "host-url-vary-v1";
    return (deliver);
}
