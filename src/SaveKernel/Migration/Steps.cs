using System.Globalization;
using SaveKernel.Hashing;
using SaveKernel.Model;

namespace SaveKernel.Migration;

/// <summary>迁移规则版本：规则集合变化时必须递增，检查点据此判定快照有效性。</summary>
public static class MigrationRules
{
    public const string RuleVersion = "rules-1.0";
    public const int DefaultDurability = 100;
    public const string DefaultDurabilitySource = "rule:DefaultDurability=100";
    public const string DefaultSkillSource = "rule:DefaultSkillTree={vitality:1}";
}

/// <summary>v1 -> v2：时间秒转毫秒；为每个道具补齐默认耐久。</summary>
public sealed class StepV1ToV2 : IMigrationStep
{
    public int FromVersion => 1;
    public int ToVersion => 2;
    public bool TouchesItems => true;

    public void ApplyHeader(SaveModel model, List<ConversionTrace> headerTraces)
    {
        var oldSeconds = model.Playtime;
        model.Playtime = checked(oldSeconds * 1000);
        headerTraces.Add(new ConversionTrace(
            "1->2", "playtime", TraceActions.UnitConvert,
            oldSeconds.ToString(CultureInfo.InvariantCulture) + "s",
            model.Playtime.ToString(CultureInfo.InvariantCulture) + "ms",
            "rule:PlaytimeUnit s->ms"));
    }

    public void ApplyItem(SaveModel model, int itemIndex, ItemEntry item, List<ConversionTrace> itemTraces)
    {
        if (item.Durability is null)
        {
            item.Durability = MigrationRules.DefaultDurability;
            itemTraces.Add(new ConversionTrace(
                "1->2", CanonicalForm.ItemPath(itemIndex, "durability"), TraceActions.DefaultFill,
                null, MigrationRules.DefaultDurability.ToString(CultureInfo.InvariantCulture),
                MigrationRules.DefaultDurabilitySource));
        }
    }
}

/// <summary>v2 -> v3：补齐默认技能树。</summary>
public sealed class StepV2ToV3 : IMigrationStep
{
    public int FromVersion => 2;
    public int ToVersion => 3;
    public bool TouchesItems => false;

    public void ApplyHeader(SaveModel model, List<ConversionTrace> headerTraces)
    {
        if (model.SkillTree.Count == 0)
        {
            model.SkillTree["vitality"] = 1;
            headerTraces.Add(new ConversionTrace(
                "2->3", "skillTree.vitality", TraceActions.DefaultFill,
                null, "1", MigrationRules.DefaultSkillSource));
        }
    }

    public void ApplyItem(SaveModel model, int itemIndex, ItemEntry item, List<ConversionTrace> itemTraces)
    {
    }
}
