using System.Globalization;
using System.Security.Cryptography;
using System.Text;
using SaveKernel.Model;

namespace SaveKernel.Hashing;

/// <summary>
/// 把内部模型展开为有序 (路径, 值) 序列。只依赖语义内容与量化参数，
/// 与容器内字段物理顺序无关。道具条目按下标顺序（语义顺序）排列。
/// </summary>
public static class CanonicalForm
{
    public const string HashAlgorithm = "SHA256";
    public const string DoubleFormat = "G17";

    public static string ItemPath(int index, string field) =>
        $"inventory.{index:D6}.{field}";

    public static string FmtDouble(double v) => v.ToString(DoubleFormat, CultureInfo.InvariantCulture);

    public static List<KeyValuePair<string, string>> Entries(SaveModel model)
    {
        var list = new List<KeyValuePair<string, string>>
        {
            new("gold", model.Gold.ToString(CultureInfo.InvariantCulture)),
            new("levelId", model.LevelId.ToString(CultureInfo.InvariantCulture)),
            new("playtimeMs", model.Playtime.ToString(CultureInfo.InvariantCulture)),
            new("position.x", FmtDouble(model.PosX)),
            new("position.y", FmtDouble(model.PosY)),
        };
        foreach (var kv in model.SkillTree)
            list.Add(new($"skillTree.{kv.Key}", kv.Value.ToString(CultureInfo.InvariantCulture)));
        for (var i = 0; i < model.Inventory.Count; i++)
        {
            var item = model.Inventory[i];
            list.Add(new(ItemPath(i, "itemId"), item.ItemId));
            list.Add(new(ItemPath(i, "count"), item.Count.ToString(CultureInfo.InvariantCulture)));
            list.Add(new(ItemPath(i, "durability"),
                item.Durability?.ToString(CultureInfo.InvariantCulture) ?? "<none>"));
        }
        list.Sort(static (a, b) => string.CompareOrdinal(a.Key, b.Key));
        return list;
    }

    public static string ComputeHash(SaveModel model) => HashEntries(Entries(model));

    public static string HashEntries(IEnumerable<KeyValuePair<string, string>> entries)
    {
        var sb = new StringBuilder();
        sb.Append("alg=").Append(HashAlgorithm).Append(";fmt=").Append(DoubleFormat).Append('\n');
        foreach (var kv in entries)
            sb.Append(kv.Key).Append('=').Append(kv.Value).Append('\n');
        var bytes = SHA256.HashData(Encoding.UTF8.GetBytes(sb.ToString()));
        return Convert.ToHexString(bytes).ToLowerInvariant();
    }

    /// <summary>找到首个不一致字段；完全一致返回 null。</summary>
    public static (string Path, string Expected, string Actual)? FirstDiff(
        IReadOnlyList<KeyValuePair<string, string>> expected,
        IReadOnlyList<KeyValuePair<string, string>> actual)
    {
        var expectedMap = expected.ToDictionary(kv => kv.Key, kv => kv.Value, StringComparer.Ordinal);
        var actualMap = actual.ToDictionary(kv => kv.Key, kv => kv.Value, StringComparer.Ordinal);
        var allPaths = expectedMap.Keys.Union(actualMap.Keys).OrderBy(p => p, StringComparer.Ordinal);
        foreach (var path in allPaths)
        {
            var hasE = expectedMap.TryGetValue(path, out var e);
            var hasA = actualMap.TryGetValue(path, out var a);
            if (!hasE || !hasA || e != a)
                return (path, hasE ? e! : "<missing>", hasA ? a! : "<missing>");
        }
        return null;
    }
}
