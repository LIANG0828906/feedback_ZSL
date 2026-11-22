namespace SaveKernel.Export;

/// <summary>一条 E_LOSSY 警告：字段路径、原值、降级处置方式。</summary>
public sealed record LossEntry(string FieldPath, string OriginalValue, string Disposition)
{
    public ErrorCode Code => ErrorCode.E_LOSSY;
}

public sealed class LossReport
{
    private readonly List<LossEntry> _entries = new();
    public IReadOnlyList<LossEntry> Entries => _entries;
    public bool HasLoss => _entries.Count > 0;

    public void Add(string fieldPath, string originalValue, string disposition) =>
        _entries.Add(new LossEntry(fieldPath, originalValue, disposition));
}
