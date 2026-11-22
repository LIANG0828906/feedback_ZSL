using SaveKernel.Model;

namespace SaveKernel.Loaders;

/// <summary>
/// v2：扁平键值对，PascalCase 命名，时间单位毫秒，有耐久、无技能树。
/// </summary>
public sealed class V2Loader : ISaveLoader
{
    public int Version => 2;

    public SaveModel Load(string raw)
    {
        var map = FlatKeyValueParser.Parse(raw, 2);
        var model = new SaveModel
        {
            SourceVersion = 2,
            Version = 2,
            LevelId = FlatKeyValueParser.Int(map, "Level"),
            Gold = FlatKeyValueParser.Long(map, "Gold"),
            PosX = FlatKeyValueParser.Double(map, "PlayerX"),
            PosY = FlatKeyValueParser.Double(map, "PlayerY"),
            Playtime = FlatKeyValueParser.Long(map, "PlaytimeMs") // 毫秒
        };
        var count = FlatKeyValueParser.Int(map, "InventoryCount");
        if (count < 0)
            throw new SaveKernelException(ErrorCode.E_DECODE, "InventoryCount 不能为负", "InventoryCount");
        for (var i = 0; i < count; i++)
        {
            model.Inventory.Add(new ItemEntry
            {
                ItemId = FlatKeyValueParser.Required(map, $"Inventory.{i}.ItemId"),
                Count = FlatKeyValueParser.Int(map, $"Inventory.{i}.Count"),
                Durability = FlatKeyValueParser.Int(map, $"Inventory.{i}.Durability")
            });
        }
        return model;
    }
}
