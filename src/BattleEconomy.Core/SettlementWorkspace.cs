using System.Collections.Immutable;

namespace BattleEconomy.Core;

internal sealed class SettlementWorkspace
{
    private readonly Dictionary<int, Outpost> _outpostByTerrain;
    private readonly Dictionary<int, bool> _alive;

    private SettlementWorkspace(
        TerrainMap map,
        Dictionary<int, bool> alive,
        Dictionary<int, TerrainContribution> contributions)
    {
        Map = map;
        _alive = alive;
        Contributions = contributions;
        _outpostByTerrain = map.Outposts.ToDictionary(outpost => outpost.TerrainId);
    }

    public TerrainMap Map { get; }
    public IReadOnlyDictionary<int, bool> Alive => _alive;
    public Dictionary<int, TerrainContribution> Contributions { get; }

    public static SettlementWorkspace Initial(TerrainMap map)
    {
        var alive = map.Nodes.ToDictionary(node => node.Id, _ => true);
        var workspace = new SettlementWorkspace(map, alive, []);
        foreach (var node in map.Nodes)
        {
            workspace.Contributions[node.Id] = workspace.ComputeContribution(node.Id);
        }

        return workspace;
    }

    public static SettlementWorkspace FromSnapshot(
        TerrainMap map,
        EconomySnapshot snapshot,
        out string? cacheProblem)
    {
        cacheProblem = null;
        var alive = map.Nodes.ToDictionary(node => node.Id, _ => true);
        foreach (var destroyedId in snapshot.DestroyedTerrainIds)
        {
            if (!map.TryGetNode(destroyedId, out _))
            {
                cacheProblem = "DESTROYED_TERRAIN_MISSING";
                return RecomputeFromAlive(map, alive);
            }

            alive[destroyedId] = false;
        }

        var contributions = new Dictionary<int, TerrainContribution>();
        foreach (var node in map.Nodes)
        {
            if (!snapshot.Contributions.TryGetValue(node.Id, out var contribution))
            {
                cacheProblem = $"CONTRIBUTION_MISSING:{node.Id}";
                return RecomputeFromAlive(map, alive);
            }

            if (StableHash.Contribution(contribution) != contribution.ContentHash)
            {
                cacheProblem = $"CONTRIBUTION_HASH_MISMATCH:{node.Id}";
                return RecomputeFromAlive(map, alive);
            }

            contributions[node.Id] = contribution;
        }

        if (snapshot.Contributions.Count != map.Nodes.Length)
        {
            cacheProblem = "CONTRIBUTION_COUNT_MISMATCH";
            return RecomputeFromAlive(map, alive);
        }

        return new SettlementWorkspace(map, alive, contributions);
    }

    private static SettlementWorkspace RecomputeFromAlive(TerrainMap map, Dictionary<int, bool> alive)
    {
        var workspace = new SettlementWorkspace(map, alive, []);
        foreach (var node in map.Nodes)
        {
            workspace.Contributions[node.Id] = workspace.ComputeContribution(node.Id);
        }

        return workspace;
    }

    public SettlementWorkspace Clone()
    {
        return new SettlementWorkspace(
            Map,
            new Dictionary<int, bool>(Alive),
            new Dictionary<int, TerrainContribution>(Contributions));
    }

    public ImmutableArray<int> FindOldGraphCandidateSet(IEnumerable<int> destroyedTerrainIds)
    {
        var queue = new Queue<int>();
        var seen = new HashSet<int>();
        foreach (var terrainId in destroyedTerrainIds.Order())
        {
            if (seen.Add(terrainId))
            {
                queue.Enqueue(terrainId);
            }
        }

        while (queue.Count > 0)
        {
            var current = queue.Dequeue();
            foreach (var neighbor in Map[current].Neighbors)
            {
                if (Alive[neighbor] && seen.Add(neighbor))
                {
                    queue.Enqueue(neighbor);
                }
            }
        }

        return seen.Order().ToImmutableArray();
    }

    public IncrementalStep ApplyForAnalysis(int terrainId)
    {
        var candidates = FindOldGraphCandidateSet([terrainId]);
        _alive[terrainId] = false;
        var changed = new List<int>();
        foreach (var candidate in candidates)
        {
            var previous = Contributions[candidate];
            var updated = ComputeContribution(candidate);
            Contributions[candidate] = updated;
            if (updated.ContentHash != previous.ContentHash)
            {
                changed.Add(candidate);
            }
        }

        return new IncrementalStep(candidates, changed.Order().ToImmutableArray());
    }

    public FinalSettlement SettleMerged(IEnumerable<int> destroyedTerrainIds, bool forceConservative, string? conservativeReason)
    {
        var destroyed = destroyedTerrainIds.Order().ToArray();
        var oldContributions = new Dictionary<int, TerrainContribution>(Contributions);

        var candidates = forceConservative
            ? Map.Nodes.Select(node => node.Id).Order().ToImmutableArray()
            : FindOldGraphCandidateSet(destroyed);

        foreach (var terrainId in destroyed)
        {
            _alive[terrainId] = false;
        }

        var recomputed = 0;
        var changed = new List<int>();
        foreach (var candidate in candidates)
        {
            var updated = ComputeContribution(candidate);
            recomputed++;
            if (updated.ContentHash != oldContributions[candidate].ContentHash)
            {
                Contributions[candidate] = updated;
                changed.Add(candidate);
            }
        }

        var affected = changed.Order().ToImmutableArray();
        return new FinalSettlement(
            candidates,
            affected,
            recomputed,
            Map.Nodes.Length - candidates.Length,
            forceConservative,
            conservativeReason);
    }

    public TerrainContribution ComputeContribution(int terrainId)
    {
        var node = Map[terrainId];
        if (!Alive[terrainId])
        {
            return TerrainContribution.Create(node, true, null, null, []);
        }

        if (!node.IsHarvestable)
        {
            return TerrainContribution.Create(node, false, null, null, []);
        }

        var route = FindNearestOutpost(terrainId);
        if (route is null)
        {
            return TerrainContribution.Create(node, false, null, null, []);
        }

        return TerrainContribution.Create(
            node,
            false,
            route.Faction,
            route.OutpostId,
            route.Path);
    }

    private RouteResult? FindNearestOutpost(int startTerrainId)
    {
        var queue = new Queue<int>();
        var distance = new Dictionary<int, int> { [startTerrainId] = 0 };
        var parent = new Dictionary<int, int>();
        queue.Enqueue(startTerrainId);

        int? bestDistance = null;
        var bestOutposts = new List<Outpost>();
        while (queue.Count > 0)
        {
            var current = queue.Dequeue();
            var currentDistance = distance[current];
            if (bestDistance is not null && currentDistance > bestDistance.Value)
            {
                break;
            }

            if (_outpostByTerrain.TryGetValue(current, out var outpost))
            {
                if (bestDistance is null || currentDistance < bestDistance.Value)
                {
                    bestDistance = currentDistance;
                    bestOutposts.Clear();
                    bestOutposts.Add(outpost);
                }
                else if (currentDistance == bestDistance.Value)
                {
                    bestOutposts.Add(outpost);
                }

                continue;
            }

            foreach (var neighbor in Map[current].Neighbors)
            {
                if (!Alive[neighbor] || distance.ContainsKey(neighbor))
                {
                    continue;
                }

                distance[neighbor] = currentDistance + 1;
                parent[neighbor] = current;
                queue.Enqueue(neighbor);
            }
        }

        if (bestDistance is null || bestOutposts.Count == 0)
        {
            return null;
        }

        var chosen = bestOutposts.OrderBy(item => item.Id, StringComparer.Ordinal).First();
        var path = new List<int>();
        var cursor = chosen.TerrainId;
        while (true)
        {
            path.Add(cursor);
            if (cursor == startTerrainId)
            {
                break;
            }

            cursor = parent[cursor];
        }

        path.Reverse();
        return new RouteResult(chosen.Id, chosen.Faction, path);
    }

    private sealed record RouteResult(string OutpostId, Faction Faction, IReadOnlyList<int> Path);
}

internal sealed record IncrementalStep(
    ImmutableArray<int> Candidates,
    ImmutableArray<int> Changed);

internal sealed record FinalSettlement(
    ImmutableArray<int> Candidates,
    ImmutableArray<int> Affected,
    int RecomputedTiles,
    int ReusedTiles,
    bool ConservativeFallback,
    string? ConservativeReason);

internal static class SettlementMath
{
    public static ImmutableArray<FactionEconomy> Accumulate(
        IEnumerable<TerrainContribution> contributions,
        IEnumerable<FactionEconomy> previous,
        int economicSteps)
    {
        var yields = Enum.GetValues<Faction>()
            .ToDictionary(faction => faction, _ => 0m);
        foreach (var contribution in contributions)
        {
            if (contribution.SuppliedFaction is { } faction)
            {
                yields[faction] += contribution.YieldPerStep;
            }
        }

        return Enum.GetValues<Faction>()
            .Select(faction =>
            {
                var previousFaction = previous.Single(item => item.Faction == faction);
                return new FactionEconomy(
                    faction,
                    yields[faction],
                    previousFaction.TotalResources + (yields[faction] * economicSteps));
            })
            .ToImmutableArray();
    }
}
