using System.Collections.Immutable;
using System.Text.Json;

namespace BattleEconomy.Core;

public sealed record ScenarioSpec(
    string Name,
    int Seed,
    int Rows,
    int Columns,
    int AdjacencyDensity,
    int BatchCount,
    int DestructionsPerBatch);

public sealed record ScenarioBundle(
    ScenarioSpec Spec,
    TerrainMap Terrain,
    ImmutableArray<BatchRequest> Batches);

public static class ScenarioCatalog
{
    public static ImmutableArray<ScenarioSpec> BuiltIns =>
    [
        new("small-sparse", 1101, 10, 8, 30, 4, 2),
        new("medium-balanced", 2202, 18, 14, 55, 6, 3),
        new("large-dense", 3303, 28, 22, 78, 8, 4)
    ];
}

public static class DeterministicScenarioGenerator
{
    public static ScenarioBundle Generate(ScenarioSpec spec)
    {
        if (spec.Rows < 2 || spec.Columns < 2 || spec.Rows * spec.Columns < 4)
        {
            throw new ArgumentOutOfRangeException(nameof(spec), "scenario requires at least a 2x2 grid");
        }

        if (spec.AdjacencyDensity is < 0 or > 100)
        {
            throw new ArgumentOutOfRangeException(nameof(spec), "adjacency density must be 0..100");
        }

        if (spec.BatchCount <= 0 || spec.DestructionsPerBatch <= 0)
        {
            throw new ArgumentOutOfRangeException(nameof(spec), "batch count and batch size must be positive");
        }

        var random = new StableRandom(unchecked((ulong)(uint)spec.Seed * 0x9E3779B97F4A7C15UL + 0xD1B54A32D192ED03UL));
        var outposts = BuildOutposts(spec);
        var adjacency = BuildAdjacency(spec, outposts, random);
        var outpostTerrains = outposts.Select(item => item.TerrainId).ToHashSet();
        var nodes = ImmutableArray.CreateBuilder<TerrainNode>(spec.Rows * spec.Columns);

        for (var row = 0; row < spec.Rows; row++)
        {
            for (var column = 0; column < spec.Columns; column++)
            {
                var id = ToId(row, column, spec.Columns);
                var isOutpost = outpostTerrains.Contains(id);
                var nominalOutpost = outposts
                    .OrderBy(item => Math.Abs(item.TerrainId / spec.Columns - row) +
                                     Math.Abs(item.TerrainId % spec.Columns - column))
                    .ThenBy(item => item.Id, StringComparer.Ordinal)
                    .First();
                var yield = 0.5m + (random.NextUInt64() % 16) * 0.25m;
                var harvestable = !isOutpost && random.NextUInt64() % 100 < 78;
                nodes.Add(new TerrainNode(
                    id,
                    row,
                    column,
                    yield,
                    adjacency[id].Order().ToImmutableArray(),
                    nominalOutpost.Id,
                    harvestable));
            }
        }

        var terrain = new TerrainMap(nodes, outposts);
        var validation = terrain.Validate();
        if (validation is not null)
        {
            throw new InvalidOperationException(
                $"generated scenario is invalid at terrain {validation.TerrainId}: {validation.Message}");
        }

        var batches = BuildBatches(terrain, spec, random);
        return new ScenarioBundle(spec, terrain, batches);
    }

    public static string WriteScenario(ScenarioBundle bundle, string directory)
    {
        Directory.CreateDirectory(directory);
        var path = Path.Combine(directory, "scenario.json");
        var document = new
        {
            name = bundle.Spec.Name,
            seed = bundle.Spec.Seed,
            rows = bundle.Spec.Rows,
            columns = bundle.Spec.Columns,
            adjacencyDensity = bundle.Spec.AdjacencyDensity,
            terrains = bundle.Terrain.Nodes.Select(node => new
            {
                id = node.Id,
                row = node.Row,
                column = node.Column,
                yieldPerStep = node.YieldPerStep,
                neighbors = node.Neighbors,
                outpost = node.OutpostId,
                harvestable = node.IsHarvestable
            }).ToArray(),
            outposts = bundle.Terrain.Outposts.Select(outpost => new
            {
                id = outpost.Id,
                faction = outpost.Faction.ToString(),
                terrain = outpost.TerrainId
            }).ToArray(),
            batches = bundle.Batches.Select(batch => new
            {
                batch = batch.BatchNumber,
                economicSteps = batch.EconomicSteps,
                destructions = batch.Destructions.Select(item => new
                {
                    key = item.Key,
                    terrain = item.TerrainId
                }).ToArray()
            }).ToArray()
        };
        File.WriteAllBytes(
            path,
            JsonSerializer.SerializeToUtf8Bytes(
                document,
                new JsonSerializerOptions { WriteIndented = true }));
        return path;
    }

    private static ImmutableArray<Outpost> BuildOutposts(ScenarioSpec spec)
    {
        var placements = new[]
        {
            (Id: "A-0", Faction: Faction.Aurora, Row: 0, Column: 0),
            (Id: "B-0", Faction: Faction.Bastion, Row: spec.Rows - 1, Column: spec.Columns - 1),
            (Id: "A-1", Faction: Faction.Aurora, Row: spec.Rows / 2, Column: Math.Max(0, spec.Columns - 1)),
            (Id: "B-1", Faction: Faction.Bastion, Row: spec.Rows / 2, Column: 0)
        };
        var selected = new List<Outpost>();
        var usedTerrains = new HashSet<int>();
        var desiredCount = spec.Rows >= 8 && spec.Columns >= 8 ? 4 : 2;
        foreach (var placement in placements)
        {
            if (selected.Count >= desiredCount)
            {
                break;
            }

            var terrainId = ToId(placement.Row, placement.Column, spec.Columns);
            if (usedTerrains.Add(terrainId))
            {
                selected.Add(new Outpost(placement.Id, placement.Faction, terrainId));
            }
        }

        if (!selected.Any(item => item.Faction == Faction.Aurora) ||
            !selected.Any(item => item.Faction == Faction.Bastion))
        {
            throw new InvalidOperationException("scenario does not contain both factions");
        }

        return selected.OrderBy(item => item.Id, StringComparer.Ordinal).ToImmutableArray();
    }

    private static Dictionary<int, HashSet<int>> BuildAdjacency(
        ScenarioSpec spec,
        ImmutableArray<Outpost> outposts,
        StableRandom random)
    {
        var result = Enumerable.Range(0, spec.Rows * spec.Columns)
            .ToDictionary(id => id, _ => new HashSet<int>());

        var snake = new List<int>(spec.Rows * spec.Columns);
        for (var row = 0; row < spec.Rows; row++)
        {
            if (row % 2 == 0)
            {
                for (var column = 0; column < spec.Columns; column++)
                {
                    snake.Add(ToId(row, column, spec.Columns));
                }
            }
            else
            {
                for (var column = spec.Columns - 1; column >= 0; column--)
                {
                    snake.Add(ToId(row, column, spec.Columns));
                }
            }
        }

        for (var index = 1; index < snake.Count; index++)
        {
            AddEdge(result, snake[index - 1], snake[index]);
        }

        for (var row = 0; row < spec.Rows; row++)
        {
            for (var column = 0; column < spec.Columns; column++)
            {
                var id = ToId(row, column, spec.Columns);
                if (column + 1 < spec.Columns)
                {
                    var right = ToId(row, column + 1, spec.Columns);
                    if (random.NextUInt64() % 100UL < (ulong)spec.AdjacencyDensity)
                    {
                        AddEdge(result, id, right);
                    }
                }

                if (row + 1 < spec.Rows)
                {
                    var down = ToId(row + 1, column, spec.Columns);
                    if (random.NextUInt64() % 100UL < (ulong)spec.AdjacencyDensity)
                    {
                        AddEdge(result, id, down);
                    }
                }

                if (spec.AdjacencyDensity >= 65 && row + 1 < spec.Rows && column + 1 < spec.Columns)
                {
                    var diagonal = ToId(row + 1, column + 1, spec.Columns);
                    if (random.NextUInt64() % 100UL < (ulong)(spec.AdjacencyDensity - 60))
                    {
                        AddEdge(result, id, diagonal);
                    }
                }
            }
        }

        // Outpost nodes keep their graph degree even in very sparse scenarios.
        foreach (var outpost in outposts)
        {
            var row = outpost.TerrainId / spec.Columns;
            var column = outpost.TerrainId % spec.Columns;
            foreach (var (dr, dc) in new[] { (-1, 0), (1, 0), (0, -1), (0, 1) })
            {
                var neighborRow = row + dr;
                var neighborColumn = column + dc;
                if (neighborRow >= 0 && neighborRow < spec.Rows &&
                    neighborColumn >= 0 && neighborColumn < spec.Columns)
                {
                    AddEdge(result, outpost.TerrainId, ToId(neighborRow, neighborColumn, spec.Columns));
                }
            }
        }

        return result;
    }

    private static ImmutableArray<BatchRequest> BuildBatches(
        TerrainMap terrain,
        ScenarioSpec spec,
        StableRandom random)
    {
        var outpostTerrains = terrain.Outposts.Select(item => item.TerrainId).ToHashSet();
        var candidates = terrain.Nodes
            .Where(node => !outpostTerrains.Contains(node.Id))
            .Select(node => node.Id)
            .ToList();
        Shuffle(candidates, random);

        var required = spec.BatchCount * spec.DestructionsPerBatch;
        if (candidates.Count < required)
        {
            throw new InvalidOperationException(
                $"scenario has {candidates.Count} destructible candidates but {required} are required");
        }

        var batches = ImmutableArray.CreateBuilder<BatchRequest>(spec.BatchCount);
        var cursor = 0;
        for (var batchIndex = 0; batchIndex < spec.BatchCount; batchIndex++)
        {
            var destructions = ImmutableArray.CreateBuilder<DestructionEvent>(spec.DestructionsPerBatch);
            for (var itemIndex = 0; itemIndex < spec.DestructionsPerBatch; itemIndex++)
            {
                var terrainId = candidates[cursor++];
                destructions.Add(new DestructionEvent(
                    $"B{batchIndex + 1:D3}-D{itemIndex + 1:D3}-T{terrainId:D4}",
                    terrainId));
            }

            batches.Add(new BatchRequest
            {
                BatchNumber = batchIndex + 1,
                EconomicSteps = 1 + (batchIndex % 2),
                Destructions = destructions.ToImmutable()
            });
        }

        return batches.ToImmutable();
    }

    private static void AddEdge(Dictionary<int, HashSet<int>> adjacency, int left, int right)
    {
        if (left == right)
        {
            return;
        }

        adjacency[left].Add(right);
        adjacency[right].Add(left);
    }

    private static int ToId(int row, int column, int columns) => row * columns + column;

    private static void Shuffle<T>(IList<T> values, StableRandom random)
    {
        for (var index = values.Count - 1; index > 0; index--)
        {
            var swapIndex = (int)(random.NextUInt64() % (ulong)(index + 1));
            (values[index], values[swapIndex]) = (values[swapIndex], values[index]);
        }
    }

    private sealed class StableRandom
    {
        private ulong _state;

        public StableRandom(ulong seed)
        {
            _state = seed == 0 ? 0x9E3779B97F4A7C15UL : seed;
        }

        public ulong NextUInt64()
        {
            _state += 0x9E3779B97F4A7C15UL;
            var value = _state;
            value = (value ^ (value >> 30)) * 0xBF58476D1CE4E5B9UL;
            value = (value ^ (value >> 27)) * 0x94D049BB133111EBUL;
            return value ^ (value >> 31);
        }
    }
}
