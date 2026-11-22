using System.Globalization;
using SaveKernel.Model;

namespace SaveKernel.Loaders;

/// <summary>
/// 扁平键值对容器解析辅助（v1/v2 使用）。
/// 行格式损坏抛 E_FORMAT，数值解码失败抛 E_DECODE，必填缺失抛 E_VER。
/// </summary>
internal static class FlatKeyValueParser
{
    public static Dictionary<string, string> Parse(string raw, int expectedVersion)
    {
        var map = new Dictionary<string, string>(StringComparer.Ordinal);
        var lines = raw.Replace("\r\n", "\n").Split('\n');
        for (var i = 0; i < lines.Length; i++)
        {
            var line = lines[i].Trim();
            if (line.Length == 0 || line.StartsWith('#')) continue;
            var eq = line.IndexOf('=');
            if (eq <= 0)
                throw new SaveKernelException(
                    ErrorCode.E_FORMAT,
                    $"第 {i + 1} 行不是合法的 key=value 结构: '{line}'",
                    fieldPath: $"line[{i + 1}]");
            var key = line[..eq].Trim();
            var value = line[(eq + 1)..].Trim();
            if (key.Length == 0)
                throw new SaveKernelException(ErrorCode.E_FORMAT, $"第 {i + 1} 行键为空", $"line[{i + 1}]");
            if (!map.TryAdd(key, value))
                throw new SaveKernelException(ErrorCode.E_FORMAT, $"重复键 '{key}'", key);
        }
        if (!map.TryGetValue("version", out var v) || v != expectedVersion.ToString(CultureInfo.InvariantCulture))
            throw new SaveKernelException(
                ErrorCode.E_VER,
                $"版本头缺失或与期望版本 {expectedVersion} 不符",
                "version", expectedVersion.ToString(CultureInfo.InvariantCulture), v ?? "<missing>");
        return map;
    }

    public static string Required(Dictionary<string, string> map, string key)
    {
        if (!map.TryGetValue(key, out var value))
            throw new SaveKernelException(ErrorCode.E_VER, $"必填字段缺失: {key}", key);
        return value;
    }

    public static int Int(Dictionary<string, string> map, string key)
    {
        var raw = Required(map, key);
        if (!int.TryParse(raw, NumberStyles.Integer, CultureInfo.InvariantCulture, out var v))
            throw new SaveKernelException(ErrorCode.E_DECODE, $"字段 '{key}' 无法解码为整数: '{raw}'", key, actual: raw);
        return v;
    }

    public static long Long(Dictionary<string, string> map, string key)
    {
        var raw = Required(map, key);
        if (!long.TryParse(raw, NumberStyles.Integer, CultureInfo.InvariantCulture, out var v))
            throw new SaveKernelException(ErrorCode.E_DECODE, $"字段 '{key}' 无法解码为长整数: '{raw}'", key, actual: raw);
        return v;
    }

    public static double Double(Dictionary<string, string> map, string key)
    {
        var raw = Required(map, key);
        if (!double.TryParse(raw, NumberStyles.Float, CultureInfo.InvariantCulture, out var v))
            throw new SaveKernelException(ErrorCode.E_DECODE, $"字段 '{key}' 无法解码为浮点数: '{raw}'", key, actual: raw);
        return v;
    }
}
