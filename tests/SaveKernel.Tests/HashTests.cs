using SaveKernel.Hashing;
using SaveKernel.Loaders;
using SaveKernel.Pipeline;

namespace SaveKernel.Tests;

[Collection("TestData")]
public sealed class HashTests
{
    private readonly TestDataFixture _data;
    private readonly MigrationPipeline _pipeline = new();

    public HashTests(TestDataFixture data) => _data = data;

    [Theory]
    [InlineData("small", 1)]
    [InlineData("small", 3)]
    [InlineData("large", 2)]
    public void Hash_MatchesManifest_AfterLoadAndMigration(string saveId, int version)
    {
        var manifest = _data.Manifest(saveId, version);
        var result = _pipeline.Run(_data.Raw(saveId, version), new PipelineOptions(), manifest);
        Assert.Equal(manifest.CurrentHash, result.ContentHash);
    }

    [Fact]
    public void Hash_IsIndependentOfPhysicalFieldOrder()
    {
        var raw = _data.Raw("small", 1);
        var lines = raw.Split('\n', StringSplitOptions.RemoveEmptyEntries).ToList();
        // 打乱非首行（version 必须保持首行以便嗅探）
        var tail = lines.Skip(1).Reverse();
        var shuffled = lines[0] + "\n" + string.Join('\n', tail) + "\n";
        var a = new LoaderRegistry().Load(raw);
        var b = new LoaderRegistry().Load(shuffled);
        Assert.Equal(CanonicalForm.ComputeHash(a), CanonicalForm.ComputeHash(b));
    }

    [Fact]
    public void ItemReadOrderChange_IsDetected()
    {
        var manifest = _data.Manifest("small", 3);
        var raw = _data.Raw("small", 3);
        // 交换 JSON 中前两个道具条目（语义顺序变化）
        var doc = System.Text.Json.JsonDocument.Parse(raw);
        var items = doc.RootElement.GetProperty("inventory").EnumerateArray()
            .Select(e => e.GetRawText()).ToList();
        (items[0], items[1]) = (items[1], items[0]);
        var modified = "{ \"version\": 3, \"levelId\": 7, \"gold\": 1250, " +
            "\"position\": { \"x\": 12.5, \"y\": -3.25 }, \"playtimeMs\": 3600000, " +
            "\"skillTree\": { \"vitality\": 2, \"focus\": 1 }, " +
            "\"inventory\": [" + string.Join(',', items) + "] }";
        var ex = Assert.Throws<SaveKernelException>(
            () => _pipeline.Run(modified, new PipelineOptions(), manifest));
        Assert.Equal(ErrorCode.E_HASH, ex.Code);
        Assert.NotNull(ex.FieldPath);
        Assert.StartsWith("inventory.", ex.FieldPath);
    }

    [Fact]
    public void TamperedValue_Returns_E_HASH_WithFirstDiffPathAndValues()
    {
        var manifest = _data.Manifest("small", 1);
        var raw = _data.Raw("small", 1).Replace("gold=1250", "gold=1251");
        var ex = Assert.Throws<SaveKernelException>(
            () => _pipeline.Run(raw, new PipelineOptions(), manifest));
        Assert.Equal(ErrorCode.E_HASH, ex.Code);
        Assert.Equal("gold", ex.FieldPath);
        Assert.Equal("1250", ex.ExpectedValue);
        Assert.Equal("1251", ex.ActualValue);
    }

    [Fact]
    public void HashAlgorithmParameters_ArePartOfDigest()
    {
        // 量化参数变化（如 DoubleFormat）会改变摘要输入 -> 可识别
        var model = new LoaderRegistry().Load(_data.Raw("small", 3));
        var entries = CanonicalForm.Entries(model);
        var hash = CanonicalForm.HashEntries(entries);
        var tampered = entries.Select(kv =>
            kv.Key == "position.x" ? new KeyValuePair<string, string>(kv.Key, "12.50") : kv);
        Assert.NotEqual(hash, CanonicalForm.HashEntries(tampered));
    }
}
