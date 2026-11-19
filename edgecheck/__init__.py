"""edgecheck: dual-protocol (HTTP/1.1 + HTTP/2) edge-cache compatibility checker.

Topology:
  client(h1/h2) -> Varnish 7.4 -> backend A (HTTP/1.1 only, chunked)
                               -> backend B (h1 + h2c, HEAD/GET CL mismatch)
                               -> bridge (h1 -> h2c upgrade) -> backend C (h2c only)
"""

__version__ = "1.0.0"
