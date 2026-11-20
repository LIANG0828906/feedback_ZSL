"""地形可视化增量更新工具。

模块划分::

    errors       统一错误码与异常
    binary_io    自定义二进制头/体数据读写
    hashing      内容哈希与哈希链
    quadtree     分块四叉树
    pipeline     法线 / 材质混合 / 接缝
    dependency   依赖图谱与增量失效判定
    engine       地形引擎（加载、补丁、增量更新、全量真值）
    snapshot     原子提交与强制终止恢复
    render       离屏渲染与增量重渲
    worker       后台重算任务（E_BUSY / 协作式取消）
    samples      确定性样本生成器
    ui           PySide6 示例界面
"""

from .errors import TerrainError, ErrorCode

__all__ = ["TerrainError", "ErrorCode"]
__version__ = "1.0.0"
