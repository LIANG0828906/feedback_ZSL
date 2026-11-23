"""Error codes for the tactical pathfinding component.

Every domain failure raises TacError with a stable machine-readable code.
"""

E_SCHEMA = "E_SCHEMA"        # map manifest / grid schema violation
E_POINT = "E_POINT"          # invalid start / goal / probe point
E_HEUR = "E_HEUR"            # heuristic may overestimate true remaining cost
E_SMOOTH = "E_SMOOTH"        # smoothing invariant violated
E_PARA = "E_PARA"            # advance parameter out of range
E_BOUND = "E_BOUND"          # observed error exceeds its derived upper bound
E_DIFF = "E_DIFF"            # realtime / resume / recompute outputs diverge
E_DECODE = "E_DECODE"        # map file corrupted / undecodable
E_OVERFLOW = "E_OVERFLOW"    # fixed-point accumulator overflow bound hit
E_CANCELLED = "E_CANCELLED"  # user cancellation / simulated kill boundary
E_NOPATH = "E_NOPATH"        # no path exists between the given points


class TacError(Exception):
    def __init__(self, code, message, details=None):
        super().__init__(f"[{code}] {message}")
        self.code = code
        self.details = details or {}
