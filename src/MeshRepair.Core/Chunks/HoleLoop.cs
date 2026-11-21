using MeshRepair.Core.Model;

namespace MeshRepair.Core.Chunks;

public sealed record HoleLoop(
    string LoopId,
    IReadOnlyList<int> VertexIds,
    IReadOnlyList<EdgeKey> Edges,
    IReadOnlyList<ChunkId> Chunks)
{
    public bool SpansChunks => Chunks.Count > 1;

    public bool Touches(ChunkPair pair)
        => Chunks.Any(chunk => chunk == pair.A) && Chunks.Any(chunk => chunk == pair.B);
}
