using System.Text.Json;
using SaveKernel.Model;

namespace SaveKernel.Loaders;

/// <summary>按文件头部分发版本加载器；未知版本抛 E_VER。</summary>
public sealed class LoaderRegistry
{
    private readonly Dictionary<int, ISaveLoader> _loaders = new();

    public LoaderRegistry()
    {
        Register(new V1Loader());
        Register(new V2Loader());
        Register(new V3Loader());
    }

    public void Register(ISaveLoader loader) => _loaders[loader.Version] = loader;

    public IReadOnlyDictionary<int, ISaveLoader> Loaders => _loaders;

    public static int SniffVersion(string raw)
    {
        var trimmed = raw.TrimStart();
        if (trimmed.StartsWith('{'))
        {
            try
            {
                using var doc = JsonDocument.Parse(trimmed);
                if (doc.RootElement.TryGetProperty("version", out var v) && v.TryGetInt32(out var n))
                    return n;
                throw new SaveKernelException(ErrorCode.E_VER, "JSON 存档缺少 version 字段", "$.version");
            }
            catch (JsonException ex)
            {
                throw new SaveKernelException(ErrorCode.E_FORMAT, $"JSON 容器结构损坏: {ex.Message}", "$", inner: ex);
            }
        }
        foreach (var line in trimmed.Replace("\r\n", "\n").Split('\n'))
        {
            var t = line.Trim();
            if (t.Length == 0 || t.StartsWith('#')) continue;
            var eq = t.IndexOf('=');
            if (eq <= 0)
                throw new SaveKernelException(ErrorCode.E_FORMAT, $"首行不是合法 key=value: '{t}'", "line[1]");
            if (t[..eq].Trim() == "version" && int.TryParse(t[(eq + 1)..].Trim(), out var n))
                return n;
            throw new SaveKernelException(ErrorCode.E_VER, "首个有效行必须是 version=N", "version");
        }
        throw new SaveKernelException(ErrorCode.E_FORMAT, "存档内容为空", "$");
    }

    public SaveModel Load(string raw)
    {
        var version = SniffVersion(raw);
        if (!_loaders.TryGetValue(version, out var loader))
            throw new SaveKernelException(ErrorCode.E_VER, $"未知版本号: {version}", "version", actual: version.ToString());
        return loader.Load(raw);
    }
}
