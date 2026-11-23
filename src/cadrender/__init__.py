"""CAD 几何与曲面专业桌面渲染应用核心包。"""

__version__ = "1.0.0"

from .errors import (  # noqa: F401
    E_GEOM, E_META, E_PARA, E_DIVERGE, E_VER, E_STATE, E_MEM, E_TILE,
    CadError,
)

__all__ = ["E_GEOM", "E_META", "E_PARA", "E_DIVERGE", "E_VER",
           "E_STATE", "E_MEM", "E_TILE", "CadError", "__version__"]
