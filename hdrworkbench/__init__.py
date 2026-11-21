"""HDR 帧序列色彩转换工作台。

包内模块：

- errors      统一错误码与异常
- hdrformat   自定义二进制头格式读写与校验
- color       传递函数、基色矩阵、白点适应、色调映射
- pipeline    浮点 / 定点双路径转换链
- errorbound  逐像素误差上界传播
- cache       增量重算依赖判定与帧缓存
- versions    不可变版本化发布与查询隔离
- snapshot    分块原子快照与崩溃恢复
- generator   确定性样本生成器
- experiment  脚本化实验入口与轨迹日志
- gui         可选 PySide6 界面（懒加载）
"""

__all__ = [
    "errors",
    "hdrformat",
    "color",
    "pipeline",
    "errorbound",
    "cache",
    "versions",
    "snapshot",
    "generator",
    "experiment",
]

