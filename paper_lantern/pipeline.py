from __future__ import annotations

from .render import RenderEngine
from .scene import Scene
from .state import RenderState, apply_tile, empty_state


def render_state_fresh(scene: Scene) -> RenderState:
    state = empty_state(scene)
    engine = RenderEngine(scene)
    for block in range(scene.tile_count):
        column = block % scene.tile_columns
        row = block // scene.tile_columns
        tile = engine.render_tile(block, column, row)
        apply_tile(state, scene, block, column, row, tile)
    return state

