using System.Security.Cryptography;
using System.Text.Json;

namespace TacticalField.Core;

public static class SampleGenerator
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = null,
        WriteIndented = true
    };

    public static IReadOnlyList<GeneratedSample> EnsureGenerated(string dataDir, bool force = false)
    {
        var sampleDir = Path.Combine(dataDir, "samples");
        var baselineDir = Path.Combine(dataDir, "baselines");
        Directory.CreateDirectory(sampleDir);
        Directory.CreateDirectory(baselineDir);

        var result = new List<GeneratedSample>();
        foreach (var generated in GenerateAll())
        {
            var samplePath = Path.Combine(sampleDir, $"{generated.Name}.json");
            var baselinePath = Path.Combine(baselineDir, $"{generated.Name}.json");
            if (force || !File.Exists(samplePath))
            {
                WriteNormalizedJson(samplePath, generated);
            }

            var sample = Load(samplePath);
            var run = SampleRunner.Run(sample);
            var sampleHash = HashFile(samplePath);
            var baseline = new SampleBaseline
            {
                Name = sample.Name,
                SampleHash = sampleHash,
                TraceHash = run.TraceHash,
                InitialChainHash = run.InitialView.ChainHash,
                FinalChainHash = run.FinalView.ChainHash,
                InitialFieldHashes = FieldHashes(run.InitialView),
                FinalFieldHashes = FieldHashes(run.FinalView),
                FinalVersion = checked((int)run.FinalView.Version),
                FinalSequence = checked((int)run.FinalView.BatchSequence)
            };

            if (force || !File.Exists(baselinePath))
            {
                WriteNormalizedJson(baselinePath, baseline);
            }
            else
            {
                baseline = LoadBaseline(baselinePath);
            }

            result.Add(new GeneratedSample(generated.Name, samplePath, baselinePath, sample, baseline));
        }

        return result.AsReadOnly();
    }

    public static SampleDefinition Load(string path)
    {
        var bytes = File.ReadAllBytes(path);
        var sample = JsonSerializer.Deserialize<SampleDefinition>(bytes, JsonOptions)
            ?? throw new InvalidOperationException($"Sample '{path}' is empty.");
        sample.Validate();
        return sample;
    }

    public static SampleBaseline LoadBaseline(string path)
    {
        return JsonSerializer.Deserialize<SampleBaseline>(File.ReadAllBytes(path), JsonOptions)
            ?? throw new InvalidOperationException($"Baseline '{path}' is empty.");
    }

    public static string HashFile(string path)
    {
        using var stream = File.OpenRead(path);
        return Hashing.Hex(SHA256.HashData(stream));
    }

    public static IReadOnlyList<SampleDefinition> GenerateAll()
    {
        return new[]
        {
            Generate("small_8x8", 8, 8, 0x5EED_1001UL),
            Generate("medium_12x10", 12, 10, 0x5EED_2002UL),
            Generate("large_16x12", 16, 12, 0x5EED_3003UL)
        };
    }

    private static SampleDefinition Generate(string name, int width, int height, ulong seed)
    {
        var random = new SplitMix64(seed);
        var count = width * height;
        var costs = new int[count];
        var masks = new int[count];
        var hazards = new bool[count];
        var obstacles = new bool[count];
        for (var index = 0; index < count; index++)
        {
            costs[index] = random.Next(1, 6);
            masks[index] = 1 << random.Next(0, 3);
            hazards[index] = random.Next(0, 100) < 10;
            obstacles[index] = random.Next(0, 100) < 6;
        }

        var unitAStart = Index(1, height - 2, width);
        var unitATarget = Index(width - 2, 1, width);
        var unitBStart = Index(width - 2, height - 2, width);
        var unitBTarget = Index(1, 1, width);
        ForceCorridor(masks, obstacles, width, height, unitAStart, unitATarget, 1);
        ForceCorridor(masks, obstacles, width, height, unitBStart, unitBTarget, 3);
        masks[unitAStart] |= 1;
        masks[unitATarget] |= 1;
        masks[unitBStart] |= 3;
        masks[unitBTarget] |= 3;
        obstacles[unitAStart] = obstacles[unitATarget] = false;
        obstacles[unitBStart] = obstacles[unitBTarget] = false;
        hazards[unitAStart] = hazards[unitATarget] = false;
        hazards[unitBStart] = hazards[unitBTarget] = false;

        var units = new[]
        {
            new SampleUnitDefinition
            {
                Id = "alpha",
                TraversalMask = 1,
                StartCell = unitAStart,
                Targets = new[] { unitATarget }
            },
            new SampleUnitDefinition
            {
                Id = "bravo",
                TraversalMask = 3,
                StartCell = unitBStart,
                Targets = new[] { unitBTarget }
            }
        };

        var definition = new SampleDefinition
        {
            Name = name,
            Width = width,
            Height = height,
            HazardPenalty = 5,
            BaseCosts = costs,
            PassMasks = masks,
            Hazards = hazards,
            Obstacles = obstacles,
            Units = units,
            Batches = Array.Empty<SampleBatchDefinition>()
        };

        var grid = new BattleGrid(width, height, costs, masks, hazards, obstacles);
        var alpha = definition.Units[0].ToUnitState();
        var bravo = definition.Units[1].ToUnitState();
        var alphaField = CostFieldCalculator.BuildFull(grid, alpha, unitATarget, definition.HazardPenalty);
        var bravoField = CostFieldCalculator.BuildFull(grid, bravo, unitBTarget, definition.HazardPenalty);
        var alphaPath = CostFieldCalculator.ComputePath(alphaField, unitAStart);
        var bravoPath = CostFieldCalculator.ComputePath(bravoField, unitBStart);
        var pathCells = new HashSet<int>(alphaPath.Concat(bravoPath));

        var obstaclePathCell = alphaPath.Length > 2 ? alphaPath[alphaPath.Length / 2] : unitATarget;
        var offPathOne = FindOffPathCell(pathCells, hazards, obstacles, width, height, unitAStart, unitATarget, 0);
        var offPathTwo = FindOffPathCell(pathCells, hazards, obstacles, width, height, unitBStart, unitBTarget, 11);

        definition.Batches = new[]
        {
            new SampleBatchDefinition
            {
                Index = 1,
                Changes = new[]
                {
                    ChangeHazard(offPathOne, true),
                    ChangeHazard(offPathTwo, true)
                }
            },
            new SampleBatchDefinition
            {
                Index = 2,
                Changes = new[]
                {
                    ChangeObstacle(obstaclePathCell, true),
                    ChangeHazard(offPathOne, false)
                }
            },
            new SampleBatchDefinition
            {
                Index = 3,
                Changes = new[]
                {
                    ChangeObstacle(obstaclePathCell, false),
                    ChangeHazard(offPathTwo, false),
                    ChangeUnitMask("alpha", 3)
                }
            },
            new SampleBatchDefinition
            {
                Index = 4,
                Changes = new[]
                {
                    ChangeObstacle(offPathOne, true),
                    ChangeHazard(offPathTwo, true),
                    ChangeUnitMask("bravo", 1)
                }
            }
        };

        definition.Validate();
        return definition;
    }

    private static int FindOffPathCell(
        HashSet<int> pathCells,
        bool[] hazards,
        bool[] obstacles,
        int width,
        int height,
        int start,
        int target,
        int offset)
    {
        for (var attempt = 0; attempt < width * height; attempt++)
        {
            var index = (offset + attempt) % (width * height);
            if (!pathCells.Contains(index)
                && index != start
                && index != target
                && !hazards[index]
                && !obstacles[index])
            {
                return index;
            }
        }

        return (offset + 1) % (width * height);
    }

    private static void ForceCorridor(
        int[] masks,
        bool[] obstacles,
        int width,
        int height,
        int start,
        int target,
        int mask)
    {
        var (x, y) = (start % width, start / width);
        var (targetX, targetY) = (target % width, target / width);
        while (x != targetX)
        {
            var index = Index(x, y, width);
            masks[index] |= mask;
            obstacles[index] = false;
            x += x < targetX ? 1 : -1;
        }

        while (y != targetY)
        {
            var index = Index(x, y, width);
            masks[index] |= mask;
            obstacles[index] = false;
            y += y < targetY ? 1 : -1;
        }

        var last = Index(x, y, width);
        masks[last] |= mask;
        obstacles[last] = false;
    }

    private static SampleChangeDefinition ChangeObstacle(int cell, bool value)
        => new() { Kind = "obstacle", CellIndex = cell, BoolValue = value };

    private static SampleChangeDefinition ChangeHazard(int cell, bool value)
        => new() { Kind = "hazard", CellIndex = cell, BoolValue = value };

    private static SampleChangeDefinition ChangeUnitMask(string unitId, int mask)
        => new() { Kind = "unit_mask", UnitId = unitId, CellIndex = -1, MaskValue = mask };

    private static int Index(int x, int y, int width) => y * width + x;

    private static SortedDictionary<string, string> FieldHashes(BattlefieldView view)
    {
        var result = new SortedDictionary<string, string>(StringComparer.Ordinal);
        foreach (var field in view.Fields.Values)
        {
            result[field.Key.ToString()] = field.ContentHash;
        }

        return result;
    }

    private static void WriteNormalizedJson<T>(string path, T value)
    {
        var json = JsonSerializer.Serialize(value, JsonOptions).Replace("\r\n", "\n", StringComparison.Ordinal) + "\n";
        var temp = $"{path}.tmp.{Environment.ProcessId}";
        File.WriteAllText(temp, json, new System.Text.UTF8Encoding(encoderShouldEmitUTF8Identifier: false));
        File.Move(temp, path, overwrite: true);
    }

    private sealed class SplitMix64
    {
        private ulong _state;

        public SplitMix64(ulong seed)
        {
            _state = seed;
        }

        public ulong NextUInt64()
        {
            _state += 0x9E3779B97F4A7C15UL;
            var value = _state;
            value = (value ^ (value >> 30)) * 0xBF58476D1CE4E5B9UL;
            value = (value ^ (value >> 27)) * 0x94D049BB133111EBUL;
            return value ^ (value >> 31);
        }

        public int Next(int minimumInclusive, int maximumExclusive)
            => minimumInclusive + (int)(NextUInt64() % (ulong)(maximumExclusive - minimumInclusive));
    }
}
