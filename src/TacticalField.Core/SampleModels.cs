namespace TacticalField.Core;

public sealed class SampleDefinition
{
    public string Name { get; set; } = string.Empty;
    public int Width { get; set; }
    public int Height { get; set; }
    public int HazardPenalty { get; set; } = 5;
    public int[] BaseCosts { get; set; } = Array.Empty<int>();
    public int[] PassMasks { get; set; } = Array.Empty<int>();
    public bool[] Hazards { get; set; } = Array.Empty<bool>();
    public bool[] Obstacles { get; set; } = Array.Empty<bool>();
    public SampleUnitDefinition[] Units { get; set; } = Array.Empty<SampleUnitDefinition>();
    public SampleBatchDefinition[] Batches { get; set; } = Array.Empty<SampleBatchDefinition>();

    public BattlefieldDefinition ToBattlefieldDefinition()
    {
        var grid = new BattleGrid(Width, Height, BaseCosts, PassMasks, Hazards, Obstacles);
        var units = Units.Select(unit => unit.ToUnitState());
        return new BattlefieldDefinition(grid, units);
    }

    public void Validate()
    {
        if (string.IsNullOrWhiteSpace(Name))
        {
            throw new InvalidOperationException("Sample name is required.");
        }

        _ = ToBattlefieldDefinition();
        foreach (var batch in Batches)
        {
            if (batch.Changes is null || batch.Changes.Length == 0)
            {
                throw new InvalidOperationException($"Sample batch {batch.Index} has no changes.");
            }

            foreach (var change in batch.Changes)
            {
                _ = change.ToBattleChange();
            }
        }
    }
}

public sealed class SampleUnitDefinition
{
    public string Id { get; set; } = string.Empty;
    public int TraversalMask { get; set; }
    public int StartCell { get; set; }
    public int[] Targets { get; set; } = Array.Empty<int>();

    public UnitState ToUnitState() => new(Id, TraversalMask, StartCell, Targets);
}

public sealed class SampleBatchDefinition
{
    public int Index { get; set; }
    public SampleChangeDefinition[] Changes { get; set; } = Array.Empty<SampleChangeDefinition>();
}

public sealed class SampleChangeDefinition
{
    public string Kind { get; set; } = string.Empty;
    public int CellIndex { get; set; }
    public bool BoolValue { get; set; }
    public string UnitId { get; set; } = string.Empty;
    public int MaskValue { get; set; }

    public BattleChange ToBattleChange()
    {
        return Kind switch
        {
            "obstacle" => BattleChange.SetObstacle(CellIndex, BoolValue),
            "hazard" => BattleChange.SetHazard(CellIndex, BoolValue),
            "unit_mask" => BattleChange.SetUnitMask(UnitId, MaskValue),
            _ => throw new InvalidOperationException($"Unknown sample change kind '{Kind}'.")
        };
    }
}

public sealed class SampleBaseline
{
    public string Name { get; set; } = string.Empty;
    public string SampleHash { get; set; } = string.Empty;
    public string TraceHash { get; set; } = string.Empty;
    public string InitialChainHash { get; set; } = string.Empty;
    public string FinalChainHash { get; set; } = string.Empty;
    public SortedDictionary<string, string> InitialFieldHashes { get; set; } = new(StringComparer.Ordinal);
    public SortedDictionary<string, string> FinalFieldHashes { get; set; } = new(StringComparer.Ordinal);
    public int FinalVersion { get; set; }
    public int FinalSequence { get; set; }
}

public sealed class GeneratedSample
{
    public GeneratedSample(string name, string samplePath, string baselinePath, SampleDefinition definition, SampleBaseline baseline)
    {
        Name = name;
        SamplePath = samplePath;
        BaselinePath = baselinePath;
        Definition = definition;
        Baseline = baseline;
    }

    public string Name { get; }
    public string SamplePath { get; }
    public string BaselinePath { get; }
    public SampleDefinition Definition { get; }
    public SampleBaseline Baseline { get; }
}

public sealed class SampleRunResult
{
    public SampleRunResult(
        string trace,
        string traceHash,
        BattlefieldView initialView,
        BattlefieldView finalView,
        IReadOnlyList<BatchResult> batches,
        int cacheHits,
        int cacheMisses)
    {
        Trace = trace;
        TraceHash = traceHash;
        InitialView = initialView;
        FinalView = finalView;
        Batches = batches;
        CacheHits = cacheHits;
        CacheMisses = cacheMisses;
    }

    public string Trace { get; }
    public string TraceHash { get; }
    public BattlefieldView InitialView { get; }
    public BattlefieldView FinalView { get; }
    public IReadOnlyList<BatchResult> Batches { get; }
    public int CacheHits { get; }
    public int CacheMisses { get; }
}

