namespace MeshRepair.Core.Model;

public readonly record struct Triangle(int A, int B, int C)
{
    public int this[int index] => index switch
    {
        0 => A,
        1 => B,
        2 => C,
        _ => throw new ArgumentOutOfRangeException(nameof(index))
    };

    public bool IsDegenerate => A == B || B == C || A == C;

    public IReadOnlyList<int> Vertices => new[] { A, B, C };

    public string CanonicalKey()
    {
        Span<int> values = stackalloc int[] { A, B, C };
        values.Sort();
        return string.Join(",", values.ToArray());
    }

    public string DirectedKey() => $"{A},{B},{C}";
}
