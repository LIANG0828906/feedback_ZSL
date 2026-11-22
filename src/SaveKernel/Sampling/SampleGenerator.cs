using SaveKernel.Export;
using SaveKernel.Hashing;
using SaveKernel.Loaders;
using SaveKernel.Model;
using SaveKernel.Pipeline;

namespace SaveKernel.Sampling;

/// <summary>
/// 本地确定性样本生成器：固定种子 + 固定版本清单。
/// 为每个存档生成 v1/v2/v3 三份物理文件与对应清单（含解析真值）。
/// </summary>
public static class SampleGenerator
{
    public const int FixedSeed = 42;
    public static readonly int[] VersionList = { 1, 2, 3 };

    private sealed record SaveSpec(string SaveId, int LevelId, long Gold, double PosX, double PosY,
        long PlaytimeMs, string[] Skills, int ItemCount);

    private static readonly SaveSpec[] Specs =
    {
        new("small", 7, 1250, 12.5, -3.25, 3_600_000, new[] { "vitality:2", "focus:1" }, 12),
        new("large", 42, 987654, -100.25, 55.5, 7_200_000,
            new[] { "vitality:5", "focus:3", "agility:2", "luck:1" }, 1500),
    };

    public static IReadOnlyList<SampleManifest> Generate(string outputDir)
    {
        Directory.CreateDirectory(outputDir);
        var manifests = new List<SampleManifest>();
        var loaders = new LoaderRegistry();
        var pipeline = new MigrationPipeline(loaders);

        foreach (var spec in Specs)
        {
            var model = BuildCurrentModel(spec);
            var truth = new GroundTruth
            {
                LevelId = spec.LevelId, Gold = spec.Gold,
                ItemCount = spec.ItemCount, PosX = spec.PosX, PosY = spec.PosY
            };
            foreach (var version in VersionList)
            {
                var (text, _) = SaveExporter.Export(model, version);
                var savePath = Path.Combine(outputDir, $"{spec.SaveId}.v{version}.sav");
                File.WriteAllText(savePath, text);

                var loaded = loaders.Load(text);
                var nativeEntries = CanonicalForm.Entries(loaded);
                var migrated = pipeline.Run(text, new PipelineOptions());
                var currentEntries = CanonicalForm.Entries(migrated.Model);

                var manifest = new SampleManifest
                {
                    SaveId = spec.SaveId,
                    Version = version,
                    Seed = FixedSeed,
                    ContentHash = CanonicalForm.HashEntries(nativeEntries),
                    CurrentHash = migrated.ContentHash,
                    Fields = nativeEntries.ToDictionary(kv => kv.Key, kv => kv.Value, StringComparer.Ordinal),
                    CurrentFields = currentEntries.ToDictionary(kv => kv.Key, kv => kv.Value, StringComparer.Ordinal),
                    Truth = truth
                };
                manifest.Save(Path.Combine(outputDir, $"{spec.SaveId}.v{version}.manifest.json"));
                manifests.Add(manifest);
            }
        }
        return manifests;
    }

    private static SaveModel BuildCurrentModel(SaveSpec spec)
    {
        var rng = new Random(FixedSeed + StableNameHash(spec.SaveId));
        var model = new SaveModel
        {
            SourceVersion = SaveModel.CurrentVersion,
            Version = SaveModel.CurrentVersion,
            LevelId = spec.LevelId,
            Gold = spec.Gold,
            PosX = spec.PosX,
            PosY = spec.PosY,
            Playtime = spec.PlaytimeMs
        };
        foreach (var skill in spec.Skills)
        {
            var parts = skill.Split(':');
            model.SkillTree[parts[0]] = int.Parse(parts[1]);
        }
        for (var i = 0; i < spec.ItemCount; i++)
        {
            model.Inventory.Add(new ItemEntry
            {
                ItemId = $"item_{rng.Next(0, 50):D3}",
                Count = rng.Next(1, 100),
                Durability = rng.Next(0, 101)
            });
        }
        return model;
    }

    /// <summary>跨进程稳定的字符串哈希（string.GetHashCode 在 .NET 中按进程随机化，不可用）。</summary>
    private static int StableNameHash(string name)
    {
        unchecked
        {
            var hash = 2166136261u;
            foreach (var c in name)
                hash = (hash ^ c) * 16777619u;
            return (int)hash;
        }
    }
}
