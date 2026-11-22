using SaveKernel.Sampling;

namespace SaveKernel.Tests;

/// <summary>共享样本数据：固定种子生成到临时目录，整个测试程序集复用。</summary>
public sealed class TestDataFixture : IDisposable
{
    public string DataDir { get; }
    public IReadOnlyList<SampleManifest> Manifests { get; }

    public TestDataFixture()
    {
        DataDir = Path.Combine(Path.GetTempPath(), "savekernel-tests-" + Guid.NewGuid().ToString("N"));
        Manifests = SampleGenerator.Generate(DataDir);
    }

    public string Raw(string saveId, int version) =>
        File.ReadAllText(Path.Combine(DataDir, $"{saveId}.v{version}.sav"));

    public SampleManifest Manifest(string saveId, int version) =>
        Manifests.First(m => m.SaveId == saveId && m.Version == version);

    public string NewCheckpointDir(string name) =>
        Path.Combine(DataDir, "ckpt", name);

    public void Dispose()
    {
        try { Directory.Delete(DataDir, recursive: true); } catch { /* 忽略清理失败 */ }
    }
}

[CollectionDefinition("TestData")]
public sealed class TestDataCollection : ICollectionFixture<TestDataFixture> { }
