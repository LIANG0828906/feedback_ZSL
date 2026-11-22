using SaveKernel.Migration;
using SaveKernel.Pipeline;

namespace SaveKernel.Tests;

[Collection("TestData")]
public sealed class MigrationChainTests
{
    private readonly TestDataFixture _data;

    public MigrationChainTests(TestDataFixture data) => _data = data;

    [Fact]
    public void Route_IsQueryable_StepByStep()
    {
        var chain = new MigrationChain();
        var route = chain.FindRoute(1, 3);
        Assert.Equal(2, route.Count);
        Assert.Equal((1, 2), (route[0].FromVersion, route[0].ToVersion));
        Assert.Equal((2, 3), (route[1].FromVersion, route[1].ToVersion));
        Assert.Single(chain.FindRoute(2, 3));
        Assert.Empty(chain.FindRoute(3, 3));
    }

    [Fact]
    public void MissingMiddleStep_Returns_E_CHAIN_WithBrokenPair()
    {
        var chain = new MigrationChain(new IMigrationStep[] { new StepV2ToV3() });
        var ex = Assert.Throws<SaveKernelException>(() => chain.FindRoute(1, 3));
        Assert.Equal(ErrorCode.E_CHAIN, ex.Code);
        Assert.Equal("1->2", ex.FieldPath);
    }

    [Fact]
    public void Migration_RecordsTraces_WithDefaultsAndSources()
    {
        var pipeline = new MigrationPipeline();
        var result = pipeline.Run(_data.Raw("small", 1), new PipelineOptions());
        var unit = Assert.Single(result.Traces, t => t.Action == TraceActions.UnitConvert);
        Assert.Equal("1->2", unit.Step);
        Assert.Equal("playtime", unit.FieldPath);
        Assert.Equal("3600s", unit.OldValue);
        Assert.Equal("3600000ms", unit.NewValue);

        var fills = result.Traces.Where(t => t.Action == TraceActions.DefaultFill).ToList();
        Assert.Equal(12 + 1, fills.Count); // 12 条耐久 + 1 条技能
        Assert.All(fills.Where(f => f.FieldPath.EndsWith(".durability")),
            f => Assert.Equal(MigrationRules.DefaultDurabilitySource, f.DefaultSource));
        var skill = Assert.Single(fills, f => f.FieldPath == "skillTree.vitality");
        Assert.Equal(MigrationRules.DefaultSkillSource, skill.DefaultSource);
        Assert.Equal("2->3", skill.Step);
    }

    [Fact]
    public void Migration_IsDeterministic_FieldByField_AndTraceByTrace()
    {
        var pipeline = new MigrationPipeline();
        var a = pipeline.Run(_data.Raw("large", 1), new PipelineOptions());
        var b = pipeline.Run(_data.Raw("large", 1), new PipelineOptions());
        Assert.Equal(a.ContentHash, b.ContentHash);
        Assert.Equal(a.Traces, b.Traces);
    }
}
