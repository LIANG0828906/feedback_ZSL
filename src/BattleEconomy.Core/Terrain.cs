using System.Collections.Immutable;

namespace BattleEconomy.Core;

public sealed record Outpost(string Id, Faction Faction, int TerrainId);

public sealed record TerrainNode(
    int Id,
    int Row,
    int Column,
    decimal YieldPerStep,
    ImmutableArray<int> Neighbors,
    string OutpostId,
    bool IsHarvestable);

public sealed class TerrainMap
{
    private readonly Dictionary<int, TerrainNode> _byId;
    private readonly Dictionary<string, Outpost> _outposts;

    public TerrainMap(IEnumerable<TerrainNode> nodes, IEnumerable<Outpost> outposts)
    {
        Nodes = nodes
            .OrderBy(node => node.Id)
            .Select(node => node with { Neighbors = node.Neighbors.Order().ToImmutableArray() })
            .ToImmutableArray();
        Outposts = outposts
            .OrderBy(outpost => outpost.Id, StringComparer.Ordinal)
            .ToImmutableArray();
        _byId = [];
        foreach (var node in Nodes)
        {
            _byId.TryAdd(node.Id, node);
        }

        _outposts = new Dictionary<string, Outpost>(StringComparer.Ordinal);
        foreach (var outpost in Outposts)
        {
            _outposts.TryAdd(outpost.Id, outpost);
        }
    }

    public ImmutableArray<TerrainNode> Nodes { get; }
    public ImmutableArray<Outpost> Outposts { get; }

    public TerrainNode this[int id] => _byId[id];
    public bool TryGetNode(int id, out TerrainNode node) => _byId.TryGetValue(id, out node!);
    public bool TryGetOutpost(string id, out Outpost outpost) => _outposts.TryGetValue(id, out outpost!);

    public Outpost GetOutpostAt(int terrainId)
    {
        return Outposts.First(outpost => outpost.TerrainId == terrainId);
    }

    public bool IsOutpostTerrain(int terrainId)
    {
        return Outposts.Any(outpost => outpost.TerrainId == terrainId);
    }

    public TerrainValidationError? Validate()
    {
        if (Nodes.IsDefaultOrEmpty)
        {
            return new TerrainValidationError(null, "terrain map is empty", "TERRAIN_EMPTY");
        }

        var nodeIds = new HashSet<int>();
        foreach (var node in Nodes)
        {
            if (!nodeIds.Add(node.Id))
            {
                return new TerrainValidationError(node.Id, "duplicate terrain id", "DUPLICATE_ID");
            }

            if (node.Row < 0 || node.Column < 0)
            {
                return new TerrainValidationError(node.Id, "row or column is negative", "NEGATIVE_COORDINATE");
            }

            if (node.YieldPerStep <= 0m)
            {
                return new TerrainValidationError(node.Id, "yield rate must be positive", "NON_POSITIVE_YIELD");
            }

            if (string.IsNullOrWhiteSpace(node.OutpostId) || !_outposts.ContainsKey(node.OutpostId))
            {
                return new TerrainValidationError(node.Id, $"outpost reference '{node.OutpostId}' is dangling", "DANGLING_OUTPOST");
            }

            if (node.Neighbors.Distinct().Count() != node.Neighbors.Length)
            {
                return new TerrainValidationError(node.Id, "terrain has duplicate adjacency entries", "ADJACENCY_DUPLICATE");
            }

            foreach (var neighborId in node.Neighbors)
            {
                if (neighborId == node.Id)
                {
                    return new TerrainValidationError(node.Id, "terrain cannot reference itself", "ADJACENCY_SELF");
                }

                if (!_byId.TryGetValue(neighborId, out var neighbor))
                {
                    return new TerrainValidationError(node.Id, $"adjacency {neighborId} is out of range", "ADJACENCY_OUT_OF_RANGE");
                }

                if (!neighbor.Neighbors.Contains(node.Id))
                {
                    return new TerrainValidationError(
                        node.Id,
                        $"adjacency to {neighborId} is not reciprocal",
                        "ADJACENCY_NOT_RECIPROCAL");
                }
            }
        }

        var outpostIds = new HashSet<string>(StringComparer.Ordinal);
        var outpostTerrains = new HashSet<int>();
        foreach (var outpost in Outposts)
        {
            if (string.IsNullOrWhiteSpace(outpost.Id) || !outpostIds.Add(outpost.Id))
            {
                return new TerrainValidationError(outpost.TerrainId, "outpost id is empty or duplicated", "OUTPOST_ID_INVALID");
            }

            if (!_byId.ContainsKey(outpost.TerrainId))
            {
                return new TerrainValidationError(
                    outpost.TerrainId,
                    $"outpost '{outpost.Id}' references a missing terrain",
                    "OUTPOST_TERRAIN_MISSING");
            }

            if (!outpostTerrains.Add(outpost.TerrainId))
            {
                return new TerrainValidationError(
                    outpost.TerrainId,
                    "multiple outposts occupy the same terrain",
                    "OUTPOST_TERRAIN_DUPLICATE");
            }
        }

        if (!Outposts.Any(outpost => outpost.Faction == Faction.Aurora) ||
            !Outposts.Any(outpost => outpost.Faction == Faction.Bastion))
        {
            return new TerrainValidationError(null, "both factions require at least one outpost", "OUTPOST_FACTION_MISSING");
        }

        return null;
    }
}

public sealed record TerrainValidationError(int? TerrainId, string Message, string Reason);
