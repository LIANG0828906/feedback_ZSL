using SaveKernel.Model;

namespace SaveKernel.Migration;

/// <summary>
/// 一级迁移函数。头部字段迁移在 ApplyHeader 中执行；
/// 道具条目级迁移在 ApplyItem 中按条目执行（供分段/并行窗口调用）。
/// 痕迹先由上下文收集，最终按 步骤 -> 头部 -> 条目下标 的规范顺序输出。
/// </summary>
public interface IMigrationStep
{
    int FromVersion { get; }
    int ToVersion { get; }

    void ApplyHeader(SaveModel model, List<ConversionTrace> headerTraces);

    /// <summary>是否需要逐条目处理（决定该步骤是否参与窗口化）。</summary>
    bool TouchesItems { get; }

    void ApplyItem(SaveModel model, int itemIndex, ItemEntry item, List<ConversionTrace> itemTraces);
}
