namespace SaveKernel.Model;

/// <summary>
/// 内部领域模型。Version 表示当前数据语义版本：
/// v1 时 Playtime 单位为秒、无技能树与耐久；v2 起 Playtime 为毫秒、有耐久；v3 起有技能树。
/// </summary>
public sealed class SaveModel
{
    public const int CurrentVersion = 3;

    public int SourceVersion { get; set; }
    public int Version { get; set; }

    public int LevelId { get; set; }
    public long Gold { get; set; }
    public double PosX { get; set; }
    public double PosY { get; set; }

    /// <summary>v1 语义为秒，v2+ 语义为毫秒。</summary>
    public long Playtime { get; set; }

    /// <summary>v3+ 才有；v1/v2 语义下为空。</summary>
    public SortedDictionary<string, int> SkillTree { get; } = new(StringComparer.Ordinal);

    public List<ItemEntry> Inventory { get; } = new();

    public SaveModel Clone()
    {
        var copy = new SaveModel
        {
            SourceVersion = SourceVersion,
            Version = Version,
            LevelId = LevelId,
            Gold = Gold,
            PosX = PosX,
            PosY = PosY,
            Playtime = Playtime
        };
        foreach (var kv in SkillTree) copy.SkillTree[kv.Key] = kv.Value;
        copy.Inventory.AddRange(Inventory.Select(i => i.Clone()));
        return copy;
    }
}

public sealed class ItemEntry
{
    public string ItemId { get; set; } = "";
    public int Count { get; set; }

    /// <summary>v2+ 才有；v1 语义下为 null。</summary>
    public int? Durability { get; set; }

    public ItemEntry Clone() => new() { ItemId = ItemId, Count = Count, Durability = Durability };
}
