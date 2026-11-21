using System.Collections.Immutable;

namespace BattleEconomy.Core;

public sealed record TerrainContribution
{
    public required int TerrainId { get; init; }
    public required bool IsDestroyed { get; init; }
    public required bool IsHarvestable { get; init; }
    public required string NominalOutpostId { get; init; }
    public Faction? SuppliedFaction { get; init; }
    public string? SuppliedOutpostId { get; init; }
    public required decimal YieldPerStep { get; init; }
    public ImmutableArray<int> CollectionPath { get; init; } = [];
    public required string ContentHash { get; init; }

    public static TerrainContribution Create(
        TerrainNode node,
        bool isDestroyed,
        Faction? suppliedFaction,
        string? suppliedOutpostId,
        IEnumerable<int> collectionPath)
    {
        var contribution = new TerrainContribution
        {
            TerrainId = node.Id,
            IsDestroyed = isDestroyed,
            IsHarvestable = node.IsHarvestable,
            NominalOutpostId = node.OutpostId,
            SuppliedFaction = suppliedFaction,
            SuppliedOutpostId = suppliedOutpostId,
            YieldPerStep = suppliedFaction is null ? 0m : node.YieldPerStep,
            CollectionPath = collectionPath.ToImmutableArray(),
            ContentHash = string.Empty
        };

        return contribution with { ContentHash = StableHash.Contribution(contribution) };
    }
}

public sealed class EconomySnapshot
{
    public required long Version { get; init; }
    public required long BatchSequence { get; init; }
    public required long EconomicStep { get; init; }
    public required long CommitWatermark { get; init; }
    public required string PreviousContentHash { get; init; }
    public required string ContentHash { get; init; }
    public required ImmutableDictionary<int, TerrainContribution> Contributions { get; init; }
    public required ImmutableSortedSet<int> DestroyedTerrainIds { get; init; }
    public required ImmutableHashSet<string> CommittedDestructionKeys { get; init; }
    public required ImmutableArray<FactionEconomy> Factions { get; init; }
    public required ImmutableArray<SnapshotChainEntry> HashChain { get; init; }

    public FactionEconomy Faction(Faction faction)
    {
        return Factions.Single(item => item.Faction == faction);
    }
}
