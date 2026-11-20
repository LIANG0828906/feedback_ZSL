namespace TacticalField.Core;

public sealed class BattleGrid
{
    private readonly int[] _baseCosts;
    private readonly int[] _passMasks;
    private readonly bool[] _hazards;
    private readonly bool[] _obstacles;
    private readonly int[][] _neighbors;

    public BattleGrid(
        int width,
        int height,
        int[] baseCosts,
        int[] passMasks,
        bool[] hazards,
        bool[] obstacles)
    {
        if (width <= 0 || height <= 0)
        {
            throw new ArgumentOutOfRangeException(nameof(width), "Grid dimensions must be positive.");
        }

        var count = checked(width * height);
        if (baseCosts.Length != count || passMasks.Length != count || hazards.Length != count || obstacles.Length != count)
        {
            throw new ArgumentException("All cell arrays must match grid dimensions.");
        }

        for (var index = 0; index < count; index++)
        {
            if (baseCosts[index] <= 0)
            {
                throw new ArgumentOutOfRangeException(nameof(baseCosts), $"Base cost at {index} must be positive.");
            }
        }

        Width = width;
        Height = height;
        _baseCosts = (int[])baseCosts.Clone();
        _passMasks = (int[])passMasks.Clone();
        _hazards = (bool[])hazards.Clone();
        _obstacles = (bool[])obstacles.Clone();
        _neighbors = BuildNeighbors(width, height);
    }

    public int Width { get; }
    public int Height { get; }
    public int CellCount => Width * Height;

    public int GetX(int index) => index % Width;
    public int GetY(int index) => index / Width;

    public int BaseCostAt(int index) => _baseCosts[ValidateIndex(index)];
    public int PassMaskAt(int index) => _passMasks[ValidateIndex(index)];
    public bool IsHazard(int index) => _hazards[ValidateIndex(index)];
    public bool IsObstacle(int index) => _obstacles[ValidateIndex(index)];

    public int GetMovementWeight(int index, int traversalMask, int hazardPenalty)
    {
        ValidateIndex(index);
        if (_obstacles[index] || (_passMasks[index] & traversalMask) == 0)
        {
            return int.MaxValue;
        }

        return checked(_baseCosts[index] + (_hazards[index] ? hazardPenalty : 0));
    }

    public int[] GetNeighbors(int index)
    {
        ValidateIndex(index);
        return _neighbors[index];
    }

    public int[] CopyBaseCosts() => (int[])_baseCosts.Clone();
    public int[] CopyPassMasks() => (int[])_passMasks.Clone();
    public bool[] CopyHazards() => (bool[])_hazards.Clone();
    public bool[] CopyObstacles() => (bool[])_obstacles.Clone();

    private int ValidateIndex(int index)
    {
        if (index < 0 || index >= CellCount)
        {
            throw new ArgumentOutOfRangeException(nameof(index));
        }

        return index;
    }

    private static int[][] BuildNeighbors(int width, int height)
    {
        var result = new int[width * height][];
        for (var y = 0; y < height; y++)
        {
            for (var x = 0; x < width; x++)
            {
                var list = new List<int>(4);
                if (y > 0)
                {
                    list.Add((y - 1) * width + x);
                }

                if (x > 0)
                {
                    list.Add(y * width + x - 1);
                }

                if (x + 1 < width)
                {
                    list.Add(y * width + x + 1);
                }

                if (y + 1 < height)
                {
                    list.Add((y + 1) * width + x);
                }

                result[y * width + x] = list.ToArray();
            }
        }

        return result;
    }
}

