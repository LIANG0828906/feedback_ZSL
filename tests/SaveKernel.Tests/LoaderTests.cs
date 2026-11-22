using SaveKernel.Loaders;

namespace SaveKernel.Tests;

[Collection("TestData")]
public sealed class LoaderTests
{
    private readonly TestDataFixture _data;
    private readonly LoaderRegistry _loaders = new();

    public LoaderTests(TestDataFixture data) => _data = data;

    [Theory]
    [InlineData("small", 1)]
    [InlineData("small", 2)]
    [InlineData("small", 3)]
    [InlineData("large", 1)]
    [InlineData("large", 2)]
    [InlineData("large", 3)]
    public void Load_ThreeVersions_MapsToInternalModel_WithSourceVersion(string saveId, int version)
    {
        var model = _loaders.Load(_data.Raw(saveId, version));
        var truth = _data.Manifest(saveId, version).Truth;
        Assert.Equal(version, model.SourceVersion);
        Assert.Equal(version, model.Version);
        Assert.Equal(truth.LevelId, model.LevelId);
        Assert.Equal(truth.Gold, model.Gold);
        Assert.Equal(truth.ItemCount, model.Inventory.Count);
        Assert.Equal(truth.PosX, model.PosX);
        Assert.Equal(truth.PosY, model.PosY);
    }

    [Fact]
    public void Load_V1_KeepsSecondsAndNoDurability()
    {
        var model = _loaders.Load(_data.Raw("small", 1));
        Assert.Equal(3600, model.Playtime); // 秒
        Assert.All(model.Inventory, i => Assert.Null(i.Durability));
        Assert.Empty(model.SkillTree);
    }

    [Fact]
    public void Load_UnknownVersion_Returns_E_VER()
    {
        var ex = Assert.Throws<SaveKernelException>(() => _loaders.Load("version=9\ngold=1\n"));
        Assert.Equal(ErrorCode.E_VER, ex.Code);
        Assert.Equal("version", ex.FieldPath);
    }

    [Fact]
    public void Load_MissingRequiredField_Returns_E_VER_WithPath()
    {
        var raw = _data.Raw("small", 1).Replace("gold=1250\n", "");
        var ex = Assert.Throws<SaveKernelException>(() => _loaders.Load(raw));
        Assert.Equal(ErrorCode.E_VER, ex.Code);
        Assert.Equal("gold", ex.FieldPath);
    }

    [Fact]
    public void Load_CorruptJsonContainer_Returns_E_FORMAT()
    {
        var ex = Assert.Throws<SaveKernelException>(() => _loaders.Load("{ \"version\": 3, \"gold\": "));
        Assert.Equal(ErrorCode.E_FORMAT, ex.Code);
    }

    [Fact]
    public void Load_MalformedKeyValueLine_Returns_E_FORMAT()
    {
        var raw = _data.Raw("small", 1).Replace("gold=1250", "gold:1250");
        var ex = Assert.Throws<SaveKernelException>(() => _loaders.Load(raw));
        Assert.Equal(ErrorCode.E_FORMAT, ex.Code);
    }

    [Fact]
    public void Load_UndecodableNumber_Returns_E_DECODE()
    {
        var raw = _data.Raw("small", 1).Replace("gold=1250", "gold=abc");
        var ex = Assert.Throws<SaveKernelException>(() => _loaders.Load(raw));
        Assert.Equal(ErrorCode.E_DECODE, ex.Code);
        Assert.Equal("gold", ex.FieldPath);
    }

    [Fact]
    public void Load_DuplicateKey_Returns_E_FORMAT_NotSilentSkip()
    {
        var raw = _data.Raw("small", 1) + "gold=9999\n";
        var ex = Assert.Throws<SaveKernelException>(() => _loaders.Load(raw));
        Assert.Equal(ErrorCode.E_FORMAT, ex.Code);
    }
}
