namespace SaveKernel.Migration;

/// <summary>一条可追溯的转换痕迹。</summary>
public sealed record ConversionTrace(
    string Step,        // 例如 "1->2"
    string FieldPath,   // 例如 "inventory.000003.durability"
    string Action,      // UnitConvert / DefaultFill / Rename
    string? OldValue,
    string NewValue,
    string DefaultSource); // 默认值来源规则标识，无默认时为 "-"

public static class TraceActions
{
    public const string UnitConvert = "UnitConvert";
    public const string DefaultFill = "DefaultFill";
}
