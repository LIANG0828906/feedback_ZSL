using System.Globalization;
using System.Text.Json;
using SaveKernel.Model;

namespace SaveKernel.Loaders;

/// <summary>
/// v3：分层 JSON 对象，camelCase 命名，时间单位毫秒，有技能树与耐久。
/// </summary>
public sealed class V3Loader : ISaveLoader
{
    public int Version => 3;

    public SaveModel Load(string raw)
    {
        JsonDocument doc;
        try
        {
            doc = JsonDocument.Parse(raw);
        }
        catch (JsonException ex)
        {
            throw new SaveKernelException(ErrorCode.E_FORMAT, $"JSON 容器结构损坏: {ex.Message}", "$", inner: ex);
        }
        using (doc)
        {
            var root = doc.RootElement;
            if (root.ValueKind != JsonValueKind.Object)
                throw new SaveKernelException(ErrorCode.E_FORMAT, "根节点必须是 JSON 对象", "$");

            var version = GetInt(root, "version", "$.version");
            if (version != 3)
                throw new SaveKernelException(ErrorCode.E_VER, $"版本号未知或不匹配: {version}", "$.version", "3", version.ToString(CultureInfo.InvariantCulture));

            var model = new SaveModel
            {
                SourceVersion = 3,
                Version = 3,
                LevelId = GetInt(root, "levelId", "$.levelId"),
                Gold = GetLong(root, "gold", "$.gold"),
                Playtime = GetLong(root, "playtimeMs", "$.playtimeMs")
            };

            if (!root.TryGetProperty("position", out var pos) || pos.ValueKind != JsonValueKind.Object)
                throw new SaveKernelException(ErrorCode.E_VER, "必填字段缺失: $.position", "$.position");
            model.PosX = GetDouble(pos, "x", "$.position.x");
            model.PosY = GetDouble(pos, "y", "$.position.y");

            if (!root.TryGetProperty("skillTree", out var skills) || skills.ValueKind != JsonValueKind.Object)
                throw new SaveKernelException(ErrorCode.E_VER, "必填字段缺失: $.skillTree", "$.skillTree");
            foreach (var skill in skills.EnumerateObject())
            {
                if (!skill.Value.TryGetInt32(out var lv))
                    throw new SaveKernelException(ErrorCode.E_DECODE, $"技能等级无法解码: {skill.Value}", $"$.skillTree.{skill.Name}", actual: skill.Value.ToString());
                model.SkillTree[skill.Name] = lv;
            }

            if (!root.TryGetProperty("inventory", out var inv) || inv.ValueKind != JsonValueKind.Array)
                throw new SaveKernelException(ErrorCode.E_VER, "必填字段缺失: $.inventory", "$.inventory");
            var index = 0;
            foreach (var entry in inv.EnumerateArray())
            {
                var path = $"$.inventory[{index}]";
                if (entry.ValueKind != JsonValueKind.Object)
                    throw new SaveKernelException(ErrorCode.E_FORMAT, "道具条目必须是对象", path);
                model.Inventory.Add(new ItemEntry
                {
                    ItemId = GetString(entry, "itemId", path + ".itemId"),
                    Count = GetInt(entry, "count", path + ".count"),
                    Durability = GetInt(entry, "durability", path + ".durability")
                });
                index++;
            }
            return model;
        }
    }

    private static JsonElement Required(JsonElement obj, string name, string path)
    {
        if (!obj.TryGetProperty(name, out var el))
            throw new SaveKernelException(ErrorCode.E_VER, $"必填字段缺失: {path}", path);
        return el;
    }

    private static int GetInt(JsonElement obj, string name, string path)
    {
        var el = Required(obj, name, path);
        if (!el.TryGetInt32(out var v))
            throw new SaveKernelException(ErrorCode.E_DECODE, $"字段无法解码为整数: {el}", path, actual: el.ToString());
        return v;
    }

    private static long GetLong(JsonElement obj, string name, string path)
    {
        var el = Required(obj, name, path);
        if (!el.TryGetInt64(out var v))
            throw new SaveKernelException(ErrorCode.E_DECODE, $"字段无法解码为长整数: {el}", path, actual: el.ToString());
        return v;
    }

    private static double GetDouble(JsonElement obj, string name, string path)
    {
        var el = Required(obj, name, path);
        if (el.ValueKind != JsonValueKind.Number || !el.TryGetDouble(out var v))
            throw new SaveKernelException(ErrorCode.E_DECODE, $"字段无法解码为浮点数: {el}", path, actual: el.ToString());
        return v;
    }

    private static string GetString(JsonElement obj, string name, string path)
    {
        var el = Required(obj, name, path);
        if (el.ValueKind != JsonValueKind.String)
            throw new SaveKernelException(ErrorCode.E_DECODE, $"字段无法解码为字符串: {el}", path, actual: el.ToString());
        return el.GetString()!;
    }
}
