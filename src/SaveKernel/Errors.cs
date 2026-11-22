namespace SaveKernel;

public enum ErrorCode
{
    None = 0,
    E_VER,        // 未知版本号 / 必填字段缺失
    E_FORMAT,     // 容器结构损坏
    E_DECODE,     // 字段值解码失败
    E_CHAIN,      // 迁移链断裂
    E_LOSSY,      // 降级损失（警告级，出现在损失清单中）
    E_HASH,       // 内容哈希不一致
    E_DIVERGE,    // 三条迁移路径结果不一致
    E_CHECKPOINT, // 检查点快照损坏或版本不匹配
    E_CANCELLED   // 用户取消
}

public sealed class SaveKernelException : Exception
{
    public ErrorCode Code { get; }
    public string? FieldPath { get; }
    public string? ExpectedValue { get; }
    public string? ActualValue { get; }
    public int? WindowIndex { get; }

    public SaveKernelException(
        ErrorCode code,
        string message,
        string? fieldPath = null,
        string? expected = null,
        string? actual = null,
        int? windowIndex = null,
        Exception? inner = null)
        : base($"[{code}] {message}", inner)
    {
        Code = code;
        FieldPath = fieldPath;
        ExpectedValue = expected;
        ActualValue = actual;
        WindowIndex = windowIndex;
    }
}
