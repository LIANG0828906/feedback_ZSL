namespace TacticalField.Core;

public sealed record FieldUpdateResult(
    CostFieldSnapshot Field,
    int[] ImpactCells,
    int[] RecomputedCells,
    int ReusedCells);

public static class CostFieldCalculator
{
    private const long Infinity = long.MaxValue / 4;

    public static CostFieldSnapshot BuildFull(
        BattleGrid grid,
        UnitState unit,
        int target,
        int hazardPenalty,
        long fieldVersion = 1,
        long batchSequence = 0)
    {
        ValidateTarget(grid, target);
        var key = new FieldKey(unit.Id, target);
        var weights = BuildWeights(grid, unit.TraversalMask, hazardPenalty);
        var distances = new long[grid.CellCount];
        Array.Fill(distances, Infinity);
        distances[target] = 0;

        var queue = new PriorityQueue<int, (long Distance, int Index)>();
        queue.Enqueue(target, (0, target));

        while (queue.TryDequeue(out var current, out var priority))
        {
            if (priority.Distance != distances[current])
            {
                continue;
            }

            foreach (var neighbor in grid.GetNeighbors(current))
            {
                if (weights[neighbor] == int.MaxValue)
                {
                    continue;
                }

                var candidate = priority.Distance + weights[neighbor];
                if (candidate < distances[neighbor])
                {
                    distances[neighbor] = candidate;
                    queue.Enqueue(neighbor, (candidate, neighbor));
                }
            }
        }

        var next = BuildNext(graph: grid, weights, distances, target);
        var costs = NormalizeCosts(distances);
        var cellHashes = BuildCellHashes(costs, next);
        var tree = BuildTree(cellHashes);
        var dependents = BuildDependents(next);
        return new CostFieldSnapshot(
            key,
            grid.Width,
            grid.Height,
            unit.TraversalMask,
            fieldVersion,
            batchSequence,
            costs,
            next,
            cellHashes,
            tree,
            dependents);
    }

    public static FieldUpdateResult UpdateIncremental(
        CostFieldSnapshot oldField,
        BattleGrid oldGrid,
        BattleGrid newGrid,
        UnitState oldUnit,
        UnitState newUnit,
        int target,
        int hazardPenalty,
        long newFieldVersion,
        long batchSequence)
    {
        ValidateTarget(newGrid, target);
        if (oldGrid.CellCount != newGrid.CellCount || oldField.CellCount != newGrid.CellCount)
        {
            throw new ArgumentException("Grid dimensions cannot change during incremental update.");
        }

        if (oldField.Key.Target != target || !string.Equals(oldField.Key.UnitId, oldUnit.Id, StringComparison.Ordinal))
        {
            throw new ArgumentException("The old field does not match the requested unit/target.");
        }

        var cellCount = newGrid.CellCount;
        var oldWeights = BuildWeights(oldGrid, oldUnit.TraversalMask, hazardPenalty);
        var newWeights = BuildWeights(newGrid, newUnit.TraversalMask, hazardPenalty);
        var changed = new List<int>();
        for (var index = 0; index < cellCount; index++)
        {
            if (index != target && oldWeights[index] != newWeights[index])
            {
                changed.Add(index);
            }
        }

        if (changed.Count == 0)
        {
            if (oldUnit.TraversalMask == newUnit.TraversalMask)
            {
                return new FieldUpdateResult(oldField, Array.Empty<int>(), Array.Empty<int>(), cellCount);
            }

            var metadataOnlyField = new CostFieldSnapshot(
                oldField.Key,
                newGrid.Width,
                newGrid.Height,
                newUnit.TraversalMask,
                newFieldVersion,
                batchSequence,
                (long[])oldField.Costs.Clone(),
                (int[])oldField.Next.Clone(),
                (byte[])oldField.CellHashes.Clone(),
                oldField.Tree,
                CostFieldCalculator.BuildDependents(oldField.Next));
            return new FieldUpdateResult(metadataOnlyField, Array.Empty<int>(), Array.Empty<int>(), cellCount);
        }

        var dirty = FindDependentClosure(oldField, changed);
        var distances = new long[cellCount];
        var touched = new bool[cellCount];
        for (var index = 0; index < cellCount; index++)
        {
            distances[index] = dirty[index] ? Infinity : ToDistance(oldField.Costs[index]);
            touched[index] = dirty[index];
        }

        var queue = new PriorityQueue<int, (long Distance, int Index)>();
        for (var cell = 0; cell < cellCount; cell++)
        {
            if (!dirty[cell] || newWeights[cell] == int.MaxValue)
            {
                continue;
            }

            foreach (var neighbor in newGrid.GetNeighbors(cell))
            {
                if (dirty[neighbor])
                {
                    continue;
                }

                var neighborDistance = ToDistance(oldField.Costs[neighbor]);
                if (neighborDistance == Infinity)
                {
                    continue;
                }

                var candidate = neighborDistance + newWeights[cell];
                if (candidate < distances[cell])
                {
                    distances[cell] = candidate;
                    queue.Enqueue(cell, (candidate, cell));
                }
            }
        }

        while (queue.TryDequeue(out var current, out var priority))
        {
            if (priority.Distance != distances[current])
            {
                continue;
            }

            foreach (var neighbor in newGrid.GetNeighbors(current))
            {
                if (neighbor == target || newWeights[neighbor] == int.MaxValue)
                {
                    continue;
                }

                var candidate = priority.Distance + newWeights[neighbor];
                if (candidate < distances[neighbor])
                {
                    distances[neighbor] = candidate;
                    touched[neighbor] = true;
                    queue.Enqueue(neighbor, (candidate, neighbor));
                }
            }
        }

        var next = BuildNext(newGrid, newWeights, distances, target);
        var costs = NormalizeCosts(distances);
        var impact = new List<int>();
        for (var index = 0; index < cellCount; index++)
        {
            if (costs[index] != oldField.Costs[index] || next[index] != oldField.Next[index])
            {
                impact.Add(index);
                touched[index] = true;
            }
        }

        if (impact.Count == 0)
        {
            var recomputed = touched.Select((value, index) => (value, index))
                .Where(item => item.value)
                .Select(item => item.index)
                .ToArray();
            if (oldUnit.TraversalMask == newUnit.TraversalMask)
            {
                return new FieldUpdateResult(oldField, Array.Empty<int>(), recomputed, cellCount);
            }

            var metadataOnlyField = new CostFieldSnapshot(
                oldField.Key,
                newGrid.Width,
                newGrid.Height,
                newUnit.TraversalMask,
                newFieldVersion,
                batchSequence,
                (long[])oldField.Costs.Clone(),
                (int[])oldField.Next.Clone(),
                (byte[])oldField.CellHashes.Clone(),
                oldField.Tree,
                CostFieldCalculator.BuildDependents(oldField.Next));
            return new FieldUpdateResult(metadataOnlyField, Array.Empty<int>(), recomputed, cellCount);
        }

        var hashes = (byte[])oldField.CellHashes.Clone();
        var replacements = new Dictionary<int, byte[]>();
        foreach (var index in impact)
        {
            var hash = CostFieldSnapshot.CreateCellHash(index, costs[index], next[index]);
            Buffer.BlockCopy(hash, 0, hashes, index * 32, 32);
            replacements[index] = hash;
        }

        var tree = oldField.Tree.Update(replacements);
        var dependents = BuildDependents(next);
        var field = new CostFieldSnapshot(
            oldField.Key,
            newGrid.Width,
            newGrid.Height,
            newUnit.TraversalMask,
            newFieldVersion,
            batchSequence,
            costs,
            next,
            hashes,
            tree,
            dependents);

        return new FieldUpdateResult(
            field,
            impact.ToArray(),
            touched.Select((value, index) => (value, index)).Where(item => item.value).Select(item => item.index).ToArray(),
            cellCount - impact.Count);
    }

    public static int[] ComputePath(CostFieldSnapshot field, int startCell)
    {
        if (startCell < 0 || startCell >= field.CellCount)
        {
            throw new ArgumentOutOfRangeException(nameof(startCell));
        }

        if (field.GetCost(startCell) < 0)
        {
            return Array.Empty<int>();
        }

        var path = new List<int>();
        var seen = new HashSet<int>();
        var current = startCell;
        path.Add(current);
        if (!seen.Add(current))
        {
            throw new InvalidOperationException("Path cycle detected.");
        }

        while (current != field.Key.Target)
        {
            var next = field.GetNext(current);
            if (next < 0 || next >= field.CellCount || !seen.Add(next))
            {
                throw new InvalidOperationException("The cost field contains a broken path chain.");
            }

            path.Add(next);
            current = next;
            if (path.Count > field.CellCount)
            {
                throw new InvalidOperationException("The cost field path exceeded the grid size.");
            }
        }

        return path.ToArray();
    }

    public static string ComputePathHash(string unitId, int target, long cost, IReadOnlyList<int> cells)
    {
        var builder = new System.Text.StringBuilder();
        builder.Append("TF-PATH-V1|")
            .Append(unitId)
            .Append('|')
            .Append(target.ToString(System.Globalization.CultureInfo.InvariantCulture))
            .Append('|')
            .Append(cost.ToString(System.Globalization.CultureInfo.InvariantCulture))
            .Append('|');
        for (var index = 0; index < cells.Count; index++)
        {
            if (index > 0)
            {
                builder.Append(',');
            }

            builder.Append(cells[index].ToString(System.Globalization.CultureInfo.InvariantCulture));
        }

        return Hashing.HashHex(builder.ToString());
    }

    internal static int[] BuildWeights(BattleGrid grid, int unitMask, int hazardPenalty)
    {
        var weights = new int[grid.CellCount];
        for (var index = 0; index < weights.Length; index++)
        {
            weights[index] = grid.GetMovementWeight(index, unitMask, hazardPenalty);
        }

        return weights;
    }

    internal static bool HasBrokenPath(CostFieldSnapshot field)
    {
        for (var index = 0; index < field.CellCount; index++)
        {
            if (index == field.Key.Target)
            {
                if (field.GetCost(index) != 0 || field.GetNext(index) != -1)
                {
                    return true;
                }

                continue;
            }

            if (field.GetCost(index) < 0)
            {
                continue;
            }

            var next = field.GetNext(index);
            if (next < 0 || next >= field.CellCount || field.GetCost(next) < 0)
            {
                return true;
            }
        }

        return false;
    }

    private static bool[] FindDependentClosure(CostFieldSnapshot oldField, IReadOnlyList<int> changed)
    {
        var dirty = new bool[oldField.CellCount];
        var queue = new Queue<int>();
        foreach (var cell in changed)
        {
            if (!dirty[cell])
            {
                dirty[cell] = true;
                queue.Enqueue(cell);
            }
        }

        while (queue.Count > 0)
        {
            var current = queue.Dequeue();
            foreach (var dependent in oldField.GetDependents(current))
            {
                if (!dirty[dependent])
                {
                    dirty[dependent] = true;
                    queue.Enqueue(dependent);
                }
            }
        }

        return dirty;
    }

    private static int[] BuildNext(BattleGrid graph, int[] weights, long[] distances, int target)
    {
        var next = new int[graph.CellCount];
        Array.Fill(next, -1);
        for (var index = 0; index < graph.CellCount; index++)
        {
            if (index == target || weights[index] == int.MaxValue || distances[index] == Infinity)
            {
                continue;
            }

            var bestDistance = Infinity;
            var bestNeighbor = -1;
            foreach (var neighbor in graph.GetNeighbors(index))
            {
                if (distances[neighbor] < bestDistance)
                {
                    bestDistance = distances[neighbor];
                    bestNeighbor = neighbor;
                }
            }

            if (bestNeighbor >= 0 && bestDistance + weights[index] == distances[index])
            {
                next[index] = bestNeighbor;
            }
            else if (bestNeighbor >= 0)
            {
                throw new InvalidOperationException(
                    $"Incremental distance invariant failed at cell {index}: expected {distances[index]}, got {bestDistance + weights[index]}.");
            }
        }

        return next;
    }

    private static long[] NormalizeCosts(long[] distances)
    {
        var costs = new long[distances.Length];
        for (var index = 0; index < distances.Length; index++)
        {
            costs[index] = distances[index] == Infinity ? -1 : distances[index];
        }

        return costs;
    }

    private static long ToDistance(long cost) => cost < 0 ? Infinity : cost;

    private static byte[] BuildCellHashes(long[] costs, int[] next)
    {
        var hashes = new byte[costs.Length * 32];
        for (var index = 0; index < costs.Length; index++)
        {
            var hash = CostFieldSnapshot.CreateCellHash(index, costs[index], next[index]);
            Buffer.BlockCopy(hash, 0, hashes, index * 32, 32);
        }

        return hashes;
    }

    private static MerkleTree BuildTree(byte[] cellHashes)
    {
        var count = cellHashes.Length / 32;
        var leaves = new byte[count][];
        for (var index = 0; index < count; index++)
        {
            leaves[index] = cellHashes.AsSpan(index * 32, 32).ToArray();
        }

        return MerkleTree.Build(leaves);
    }

    internal static int[][] BuildDependents(int[] next)
    {
        var counts = new int[next.Length];
        for (var index = 0; index < next.Length; index++)
        {
            if (next[index] >= 0)
            {
                counts[next[index]]++;
            }
        }

        var result = new int[next.Length][];
        for (var index = 0; index < result.Length; index++)
        {
            result[index] = new int[counts[index]];
        }

        var positions = new int[next.Length];
        for (var index = 0; index < next.Length; index++)
        {
            var parent = next[index];
            if (parent >= 0)
            {
                result[parent][positions[parent]++] = index;
            }
        }

        return result;
    }

    private static void ValidateTarget(BattleGrid grid, int target)
    {
        if (target < 0 || target >= grid.CellCount)
        {
            throw new ArgumentOutOfRangeException(nameof(target));
        }
    }
}
