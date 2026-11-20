namespace TacticalField.Core;

internal sealed class SnapshotDocument
{
    public string? Magic { get; set; }
    public int FormatVersion { get; set; }
    public string? IntegrityHash { get; set; }
    public SnapshotStateDto? State { get; set; }
    public ChainLinkDto[]? History { get; set; }
    public CacheEntryDto[]? Cache { get; set; }
}

internal sealed class SnapshotStateDto
{
    public long Version { get; set; }
    public long BatchSequence { get; set; }
    public int HazardPenalty { get; set; }
    public GridDto? Grid { get; set; }
    public UnitDto[]? Units { get; set; }
    public FieldDto[]? Fields { get; set; }
}

internal sealed class GridDto
{
    public int Width { get; set; }
    public int Height { get; set; }
    public int[]? BaseCosts { get; set; }
    public int[]? PassMasks { get; set; }
    public bool[]? Hazards { get; set; }
    public bool[]? Obstacles { get; set; }
}

internal sealed class UnitDto
{
    public string? Id { get; set; }
    public int TraversalMask { get; set; }
    public int StartCell { get; set; }
    public int[]? Targets { get; set; }
}

internal sealed class FieldDto
{
    public string? UnitId { get; set; }
    public int Target { get; set; }
    public int Width { get; set; }
    public int Height { get; set; }
    public int UnitMask { get; set; }
    public long FieldVersion { get; set; }
    public long BatchSequence { get; set; }
    public long[]? Costs { get; set; }
    public int[]? Next { get; set; }
    public string? CellHashesHex { get; set; }
    public string? ContentHash { get; set; }
}

internal sealed class ChainLinkDto
{
    public long BatchSequence { get; set; }
    public long PublicationVersion { get; set; }
    public string? PreviousChainHash { get; set; }
    public string? StateHash { get; set; }
    public string? ChainHash { get; set; }
}

internal sealed class CacheEntryDto
{
    public string? UnitId { get; set; }
    public int Target { get; set; }
    public int StartCell { get; set; }
    public long FieldVersion { get; set; }
    public string? FieldHash { get; set; }
    public string? PathHash { get; set; }
    public int[]? Cells { get; set; }
    public int[]? Dependencies { get; set; }
}

internal sealed record LoadedSnapshot(
    BattlefieldView View,
    IReadOnlyList<ChainLink> History,
    IReadOnlyDictionary<PathCacheKey, PathCacheEntry> Cache,
    int HazardPenalty);


