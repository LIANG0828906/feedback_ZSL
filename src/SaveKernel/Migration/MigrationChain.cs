namespace SaveKernel.Migration;

/// <summary>
/// 显式迁移链：注册逐级迁移函数，支持路线查询。
/// 缺少中间迁移函数时抛 E_CHAIN 并列出断开的版本对。
/// </summary>
public sealed class MigrationChain
{
    private readonly Dictionary<int, IMigrationStep> _steps = new();

    public MigrationChain()
    {
        Register(new StepV1ToV2());
        Register(new StepV2ToV3());
    }

    /// <summary>测试可传入自定义步骤集合来模拟断链。</summary>
    public MigrationChain(IEnumerable<IMigrationStep> steps)
    {
        foreach (var s in steps) Register(s);
    }

    public void Register(IMigrationStep step) => _steps[step.FromVersion] = step;

    /// <summary>查询从 source 到 target 的迁移路线；断链抛 E_CHAIN。</summary>
    public IReadOnlyList<IMigrationStep> FindRoute(int sourceVersion, int targetVersion)
    {
        if (sourceVersion > targetVersion)
            throw new SaveKernelException(ErrorCode.E_CHAIN, $"迁移链只支持升级: {sourceVersion} -> {targetVersion}");
        var route = new List<IMigrationStep>();
        var current = sourceVersion;
        while (current < targetVersion)
        {
            if (!_steps.TryGetValue(current, out var step))
                throw new SaveKernelException(
                    ErrorCode.E_CHAIN,
                    $"迁移链断裂: 缺少 {current} -> {current + 1} 的迁移函数",
                    fieldPath: $"{current}->{current + 1}");
            route.Add(step);
            current = step.ToVersion;
        }
        return route;
    }

    /// <summary>路线是否可用（不抛异常）。</summary>
    public bool TryFindRoute(int sourceVersion, int targetVersion, out IReadOnlyList<IMigrationStep> route)
    {
        try
        {
            route = FindRoute(sourceVersion, targetVersion);
            return true;
        }
        catch (SaveKernelException ex) when (ex.Code == ErrorCode.E_CHAIN)
        {
            route = Array.Empty<IMigrationStep>();
            return false;
        }
    }
}
