using SaveKernel.Model;

namespace SaveKernel.Loaders;

/// <summary>
/// v1：扁平键值对，小写命名，时间单位秒（整型），无技能树、无耐久。
/// </summary>
public sealed class V1Loader : ISaveLoader
{
    public int Version => 1;

    public SaveModel Load(string raw)
    {
        var map = FlatKeyValueParser.Parse(raw, 1);
        var model = new SaveModel
        {
            SourceVersion = 1,
            Version = 1,
            LevelId = FlatKeyValueParser.Int(map, "level_id"),
            Gold = FlatKeyValueParser.Long(map, "gold"),
            PosX = FlatKeyValueParser.Double(map, "pos_x"),
            PosY = FlatKeyValueParser.Double(map, "pos_y"),
            Playtime = FlatKeyValueParser.Long(map, "playtime_sec") // 秒
        };
        var count = FlatKeyValueParser.Int(map, "item_count");
        if (count < 0)
            throw new SaveKernelException(ErrorCode.E_DECODE, "item_count 不能为负", "item_count");
        for (var i = 0; i < count; i++)
        {
            model.Inventory.Add(new ItemEntry
            {
                ItemId = FlatKeyValueParser.Required(map, $"item.{i}.id"),
                Count = FlatKeyValueParser.Int(map, $"item.{i}.count"),
                Durability = null
            });
        }
        return model;
    }
}
