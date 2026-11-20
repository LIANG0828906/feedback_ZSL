"""多版本骨骼动画重定向管线。

模块组成::

    errors      业务错误码与管线异常
    math3d      四元数/旋转/变换数学（确定性）
    model       骨架、动画片段、内部 v3 表示等数据模型
    codec       v1/v2/v3 解析与序列化（原子 JSON）
    naming      关节名规范化与层级路径
    mapping     源/目标关节映射、最近祖先回退、冲突报告
    compat      v1/v2 -> v3 升级与降级损失报告
    retarget    绑定姿态对齐与动作重定向
    validate    一致性校验与偏差统计
    snapshot    工程文件、快照、哈希链与崩溃恢复
    engine      后台线程、单任务锁、协作式取消
    samples     确定性样本生成器
    gui         PyQt6 预览界面
"""

__all__ = [
    "errors",
    "math3d",
    "model",
    "codec",
    "naming",
    "mapping",
    "compat",
    "retarget",
    "validate",
    "snapshot",
    "engine",
    "samples",
]

__version__ = "1.0.0"
