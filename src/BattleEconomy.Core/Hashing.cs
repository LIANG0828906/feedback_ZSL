using System.Globalization;
using System.Security.Cryptography;
using System.Text;

namespace BattleEconomy.Core;

public static class StableHash
{
    public const string Genesis = "GENESIS";

    public static string Sha256Hex(string canonicalText)
    {
        var bytes = SHA256.HashData(Encoding.UTF8.GetBytes(canonicalText));
        return Convert.ToHexString(bytes).ToLowerInvariant();
    }

    public static string Decimal(decimal value)
    {
        return value.ToString("0.############################", CultureInfo.InvariantCulture);
    }

    public static string Bool(bool value) => value ? "1" : "0";

    public static string Contribution(TerrainContribution contribution)
    {
        var builder = new StringBuilder(256);
        builder.Append("terrain-contribution/v1|")
            .Append(contribution.TerrainId).Append('|')
            .Append(Bool(contribution.IsDestroyed)).Append('|')
            .Append(Bool(contribution.IsHarvestable)).Append('|')
            .Append(contribution.NominalOutpostId).Append('|')
            .Append(contribution.SuppliedFaction?.ToString() ?? "NONE").Append('|')
            .Append(contribution.SuppliedOutpostId ?? "NONE").Append('|')
            .Append(Decimal(contribution.YieldPerStep)).Append('|');

        foreach (var terrainId in contribution.CollectionPath)
        {
            builder.Append(terrainId).Append(',');
        }

        return Sha256Hex(builder.ToString());
    }

    public static string SnapshotContent(
        long version,
        long batchSequence,
        long economicStep,
        long commitWatermark,
        string previousContentHash,
        IEnumerable<TerrainContribution> contributions,
        IEnumerable<int> destroyedTerrainIds,
        IEnumerable<string> committedDestructionKeys,
        IEnumerable<FactionEconomy> factions)
    {
        var builder = new StringBuilder(4096);
        builder.Append("economy-snapshot/v1|")
            .Append(version).Append('|')
            .Append(batchSequence).Append('|')
            .Append(economicStep).Append('|')
            .Append(commitWatermark).Append('|')
            .Append(previousContentHash).Append('|');

        foreach (var contribution in contributions.OrderBy(item => item.TerrainId))
        {
            builder.Append(contribution.TerrainId).Append(':').Append(contribution.ContentHash).Append(';');
        }

        builder.Append('|');
        foreach (var terrainId in destroyedTerrainIds.Order())
        {
            builder.Append(terrainId).Append(',');
        }

        builder.Append('|');
        foreach (var key in committedDestructionKeys.Order(StringComparer.Ordinal))
        {
            builder.Append(key).Append(',');
        }

        builder.Append('|');
        foreach (var faction in factions.OrderBy(item => item.Faction))
        {
            builder.Append(faction.Faction).Append(':')
                .Append(Decimal(faction.YieldPerStep)).Append(':')
                .Append(Decimal(faction.TotalResources)).Append(';');
        }

        return Sha256Hex(builder.ToString());
    }

    public static string ChainEntry(
        long version,
        long batchSequence,
        long commitWatermark,
        string contentHash,
        string previousChainHash)
    {
        return Sha256Hex(
            $"economy-chain/v1|{version}|{batchSequence}|{commitWatermark}|{contentHash}|{previousChainHash}");
    }
}
