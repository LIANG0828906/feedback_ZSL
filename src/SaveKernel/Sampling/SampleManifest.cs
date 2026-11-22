using System.Text.Json;
using System.Text.Json.Serialization;

namespace SaveKernel.Sampling;

/// <summary>样本清单：声明内容哈希、规范化字段与解析真值，用于迁移结果观测。</summary>
public sealed class SampleManifest
{
    [JsonPropertyName("saveId")] public required string SaveId { get; init; }
    [JsonPropertyName("version")] public required int Version { get; init; }
    [JsonPropertyName("seed")] public required int Seed { get; init; }
    [JsonPropertyName("hashAlgorithm")] public string HashAlgorithm { get; init; } = "SHA256";
    [JsonPropertyName("contentHash")] public required string ContentHash { get; init; }
    [JsonPropertyName("currentHash")] public required string CurrentHash { get; init; }
    [JsonPropertyName("fields")] public required Dictionary<string, string> Fields { get; init; }
    [JsonPropertyName("currentFields")] public required Dictionary<string, string> CurrentFields { get; init; }
    [JsonPropertyName("groundTruth")] public required GroundTruth Truth { get; init; }

    public static SampleManifest Load(string path) =>
        JsonSerializer.Deserialize<SampleManifest>(File.ReadAllText(path))
        ?? throw new SaveKernelException(ErrorCode.E_FORMAT, $"清单文件损坏: {path}");

    public void Save(string path) =>
        File.WriteAllText(path, JsonSerializer.Serialize(this, new JsonSerializerOptions { WriteIndented = true }));
}

public sealed class GroundTruth
{
    [JsonPropertyName("levelId")] public required int LevelId { get; init; }
    [JsonPropertyName("gold")] public required long Gold { get; init; }
    [JsonPropertyName("itemCount")] public required int ItemCount { get; init; }
    [JsonPropertyName("posX")] public required double PosX { get; init; }
    [JsonPropertyName("posY")] public required double PosY { get; init; }
}
