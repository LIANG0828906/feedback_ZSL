using SaveKernel.Model;

namespace SaveKernel.Loaders;

public interface ISaveLoader
{
    int Version { get; }

    /// <summary>解析原始文本为内部模型（保留源版本语义），失败时抛出带错误码的异常。</summary>
    SaveModel Load(string raw);
}
