using SaveKernel.Export;
using SaveKernel.Hashing;
using SaveKernel.Model;
using SaveKernel.Pipeline;

namespace SaveKernel.Compare;

public enum DivergenceKind { ModelField, TraceSequence, LossList }

/// <summary>
/// 自动比对多条迁移路径的结果：定位首个不一致字段路径与所属窗口编号，
/// 并区分模型字段差异 / 痕迹序列差异 / 损失清单差异。不一致抛 E_DIVERGE。
/// </summary>
public static class ConsistencyVerifier
{
    public static void VerifyAllEqual(IReadOnlyList<PipelineResult> results)
    {
        if (results.Count < 2) return;
        var reference = results[0];
        for (var i = 1; i < results.Count; i++)
            VerifyPair(reference, results[i]);
    }

    public static void VerifyPair(PipelineResult expected, PipelineResult actual)
    {
        var diff = CanonicalForm.FirstDiff(
            CanonicalForm.Entries(expected.Model), CanonicalForm.Entries(actual.Model));
        if (diff is not null)
            throw Divergence(DivergenceKind.ModelField, diff.Value.Path,
                diff.Value.Expected, diff.Value.Actual, WindowOf(diff.Value.Path, actual));

        for (var i = 0; i < Math.Max(expected.Traces.Count, actual.Traces.Count); i++)
        {
            var e = i < expected.Traces.Count ? expected.Traces[i] : null;
            var a = i < actual.Traces.Count ? actual.Traces[i] : null;
            if (e != a)
                throw Divergence(DivergenceKind.TraceSequence,
                    e?.FieldPath ?? a?.FieldPath ?? $"trace[{i}]",
                    e?.ToString() ?? "<missing>", a?.ToString() ?? "<missing>",
                    WindowOf(e?.FieldPath ?? a?.FieldPath ?? "", actual));
        }

        var eLoss = expected.Losses?.Entries ?? (IReadOnlyList<LossEntry>)Array.Empty<LossEntry>();
        var aLoss = actual.Losses?.Entries ?? (IReadOnlyList<LossEntry>)Array.Empty<LossEntry>();
        for (var i = 0; i < Math.Max(eLoss.Count, aLoss.Count); i++)
        {
            var e = i < eLoss.Count ? eLoss[i] : null;
            var a = i < aLoss.Count ? aLoss[i] : null;
            if (e != a)
                throw Divergence(DivergenceKind.LossList,
                    e?.FieldPath ?? a?.FieldPath ?? $"loss[{i}]",
                    e?.ToString() ?? "<missing>", a?.ToString() ?? "<missing>",
                    WindowOf(e?.FieldPath ?? a?.FieldPath ?? "", actual));
        }
    }

    /// <summary>比较两个模型在指定版本可表达字段上逐项等价（降级往返验证）。</summary>
    public static void VerifyExpressibleEqual(SaveModel expected, SaveModel actual, int expressibleVersion)
    {
        var filter = ExpressibleFilter(expressibleVersion);
        var e = CanonicalForm.Entries(expected).Where(kv => filter(kv.Key)).ToList();
        var a = CanonicalForm.Entries(actual).Where(kv => filter(kv.Key)).ToList();
        var diff = CanonicalForm.FirstDiff(e, a);
        if (diff is not null)
            throw new SaveKernelException(ErrorCode.E_DIVERGE,
                $"降级往返后可表达字段不一致: {diff.Value.Path}",
                diff.Value.Path, diff.Value.Expected, diff.Value.Actual);
    }

    public static Func<string, bool> ExpressibleFilter(int version) => version switch
    {
        1 => path => !path.StartsWith("skillTree.", StringComparison.Ordinal)
                     && !path.EndsWith(".durability", StringComparison.Ordinal),
        2 => path => !path.StartsWith("skillTree.", StringComparison.Ordinal),
        _ => _ => true
    };

    private static int? WindowOf(string fieldPath, PipelineResult result)
    {
        // 从 inventory.NNNNNN.field 形式的路径解析条目下标，定位所属窗口
        const string prefix = "inventory.";
        if (!fieldPath.StartsWith(prefix, StringComparison.Ordinal)) return null;
        var dot = fieldPath.IndexOf('.', prefix.Length);
        if (dot < 0) return null;
        if (!int.TryParse(fieldPath[prefix.Length..dot], out var itemIndex)) return null;
        foreach (var w in result.Windows)
            if (itemIndex >= w.Start && itemIndex < w.End)
                return w.Index;
        return null;
    }

    private static SaveKernelException Divergence(
        DivergenceKind kind, string path, string expected, string actual, int? window) =>
        new(ErrorCode.E_DIVERGE,
            $"迁移路径结果不一致({kind}): {path}",
            path, expected, actual, window);
}
