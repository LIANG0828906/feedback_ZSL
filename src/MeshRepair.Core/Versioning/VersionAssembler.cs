using MeshRepair.Core.Chunks;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;

namespace MeshRepair.Core.Versioning;

public sealed class AssemblyResult
{
    public AssemblyResult(MeshVersion version)
    {
        Version = version;
    }

    public MeshVersion Version { get; }
}

public static class VersionAssembler
{
    public static MeshResult<AssemblyResult> Assemble(
        long version,
        long parentVersion,
        string editor,
        MeshEditInfo edit,
        IReadOnlyDictionary<ChunkId, ChunkSnapshot> sourceChunks,
        ChunkLayout layout,
        IReadOnlyDictionary<ChunkPair, BoundaryManifest>? previousBoundaries = null,
        IReadOnlyDictionary<ChunkPair, IReadOnlyDictionary<string, int>>? stableVertexOverrides = null,
        long? revertedFromVersion = null)
    {
        var positions = new Dictionary<int, Point3>();
        var positionOwner = new Dictionary<int, ChunkId>();
        foreach (var chunk in sourceChunks.Values.OrderBy(chunk => chunk.Id))
        {
            foreach (var pair in chunk.Vertices.OrderBy(pair => pair.Key))
            {
                if (positions.TryGetValue(pair.Key, out var existing))
                {
                    if (existing.DistanceTo(pair.Value) > 1e-9)
                    {
                        return MeshResult<AssemblyResult>.Fail(
                            MeshErrorCode.E_SPLICE_INVALID,
                            $"shared vertex {pair.Key} has divergent coordinates",
                            chunk: chunk.Id.ToString(),
                            chunkPair: $"{positionOwner[pair.Key]}|{chunk.Id}");
                    }
                }
                else
                {
                    positions.Add(pair.Key, pair.Value);
                    positionOwner.Add(pair.Key, chunk.Id);
                }
            }
        }

        var allFaces = sourceChunks.Values
            .SelectMany(chunk => chunk.Faces)
            .OrderBy(face => face.FaceId)
            .ToArray();
        var seenFaceIds = new HashSet<ulong>();
        foreach (var face in allFaces)
        {
            if (!seenFaceIds.Add(face.FaceId))
            {
                return MeshResult<AssemblyResult>.Fail(
                    MeshErrorCode.E_SPLICE_INVALID,
                    $"duplicate face id {face.FaceId}",
                    edgeId: $"FACE:{face.FaceId}");
            }

            foreach (var vertexId in new[] { face.Triangle.A, face.Triangle.B, face.Triangle.C })
            {
                if (!positions.ContainsKey(vertexId))
                {
                    return MeshResult<AssemblyResult>.Fail(
                        MeshErrorCode.E_SPLICE_INVALID,
                        $"face {face.FaceId} references missing vertex {vertexId}",
                        edgeId: $"FACE:{face.FaceId}");
                }
            }
        }

        var assignments = allFaces.ToDictionary(
            face => face.FaceId,
            face => layout.AssignFace(face.FaceId, face.Triangle, positions));

        var facesByChunk = assignments.Values
            .GroupBy(assignment => assignment.Owner)
            .ToDictionary(
                group => group.Key,
                group => group.Select(assignment => allFaces.First(face => face.FaceId == assignment.FaceId)).ToArray());

        var chunks = new Dictionary<ChunkId, ChunkSnapshot>();
        foreach (var pair in facesByChunk.OrderBy(pair => pair.Key))
        {
            sourceChunks.TryGetValue(pair.Key, out var source);
            chunks.Add(
                pair.Key,
                ChunkSnapshot.Create(
                    pair.Key,
                    version,
                    positions,
                    pair.Value,
                    source?.NormalRevision ?? 0));
        }

        var boundaryResult = BuildBoundaries(
            version,
            chunks,
            layout,
            previousBoundaries,
            stableVertexOverrides);
        if (!boundaryResult.IsSuccess)
        {
            return MeshResult<AssemblyResult>.Fail(boundaryResult.Error!);
        }

        var vertexCount = chunks.Values
            .SelectMany(chunk => chunk.Vertices.Keys)
            .Distinct()
            .Count();
        var faceCount = chunks.Values.Sum(chunk => chunk.Faces.Count);
        var contentHash = MeshVersion.ComputeContentHash(
            version,
            chunks,
            boundaryResult.Value!,
            vertexCount,
            faceCount);
        var finalVersion = new MeshVersion(
            version,
            parentVersion,
            editor,
            edit,
            chunks,
            boundaryResult.Value!,
            assignments,
            vertexCount,
            faceCount,
            contentHash,
            revertedFromVersion);
        return MeshResult<AssemblyResult>.Ok(new AssemblyResult(finalVersion));
    }

    public static MeshResult<IReadOnlyDictionary<ChunkPair, BoundaryManifest>> BuildBoundaries(
        long version,
        IReadOnlyDictionary<ChunkId, ChunkSnapshot> chunks,
        ChunkLayout layout,
        IReadOnlyDictionary<ChunkPair, BoundaryManifest>? previousBoundaries,
        IReadOnlyDictionary<ChunkPair, IReadOnlyDictionary<string, int>>? stableVertexOverrides)
    {
        var pairEdgeMap = new Dictionary<ChunkPair, List<(EdgeKey Edge, ChunkId A, ChunkId B)>>();
        foreach (var chunk in chunks.Values)
        {
            foreach (var face in chunk.Faces)
            {
                for (var corner = 0; corner < 3; corner++)
                {
                    var a = face.Triangle[corner];
                    var b = face.Triangle[(corner + 1) % 3];
                    if (a == b)
                    {
                        continue;
                    }

                    var key = EdgeKey.Create(a, b);
                    var owners = new HashSet<ChunkId> { chunk.Id };
                    foreach (var other in chunks.Values)
                    {
                        if (other.Id == chunk.Id)
                        {
                            continue;
                        }

                        if (other.Faces.Any(otherFace => otherFace.Triangle.ContainsEdge(a, b)))
                        {
                            owners.Add(other.Id);
                        }
                    }

                    if (owners.Count < 2)
                    {
                        continue;
                    }

                    var ordered = owners.Order().ToArray();
                    for (var first = 0; first < ordered.Length; first++)
                    {
                        for (var second = first + 1; second < ordered.Length; second++)
                        {
                            var pair = ChunkPair.Create(ordered[first], ordered[second]);
                            if (!pairEdgeMap.TryGetValue(pair, out var edges))
                            {
                                edges = new List<(EdgeKey, ChunkId, ChunkId)>();
                                pairEdgeMap.Add(pair, edges);
                            }

                            if (!edges.Any(item => item.Edge == key))
                            {
                                edges.Add((key, ordered[first], ordered[second]));
                            }
                        }
                    }
                }
            }
        }

        var occupied = chunks.Keys.Order().ToArray();
        var neighborPairs = new HashSet<ChunkPair>(pairEdgeMap.Keys);
        foreach (var first in occupied)
        {
            foreach (var second in occupied)
            {
                if (first.CompareTo(second) < 0 && layout.AreNeighbors(first, second))
                {
                    neighborPairs.Add(ChunkPair.Create(first, second));
                }
            }
        }

        var manifests = new Dictionary<ChunkPair, BoundaryManifest>();
        foreach (var pair in neighborPairs.Order())
        {
            var chunkA = chunks[pair.A];
            var chunkB = chunks[pair.B];
            pairEdgeMap.TryGetValue(pair, out var pairEdges);
            var shared = new HashSet<int>();
            var edges = new List<BoundaryEdge>();
            foreach (var vertexId in chunkA.Vertices.Keys.Intersect(chunkB.Vertices.Keys))
            {
                shared.Add(vertexId);
            }

            foreach (var item in pairEdges ?? new List<(EdgeKey Edge, ChunkId A, ChunkId B)>())
            {
                shared.Add(item.Edge.Low);
                shared.Add(item.Edge.High);
                edges.Add(new BoundaryEdge(
                    $"BE:{item.Edge.Low}-{item.Edge.High}",
                    item.Edge.Low,
                    item.Edge.High,
                    pair.A,
                    pair.B));
            }

            if (layout.TryGetInterfacePlane(pair, out _, out _))
            {
                foreach (var vertex in chunkA.Vertices.Concat(chunkB.Vertices))
                {
                    if (layout.IsOnInterface(vertex.Value, pair))
                    {
                        shared.Add(vertex.Key);
                    }
                }
            }

            var stable = new SortedDictionary<string, int>(StringComparer.Ordinal);
            if (previousBoundaries is not null
                && previousBoundaries.TryGetValue(pair, out var previous))
            {
                foreach (var entry in previous.StableVertexIds)
                {
                    stable[entry.Key] = entry.Value;
                }
            }

            if (stableVertexOverrides is not null
                && stableVertexOverrides.TryGetValue(pair, out var overrides))
            {
                foreach (var entry in overrides)
                {
                    stable[entry.Key] = entry.Value;
                }
            }

            var validStable = stable
                .Where(entry => chunkA.Vertices.ContainsKey(entry.Value) || chunkB.Vertices.ContainsKey(entry.Value))
                .ToDictionary(entry => entry.Key, entry => entry.Value, StringComparer.Ordinal);
            var previousHash = previousBoundaries is not null
                && previousBoundaries.TryGetValue(pair, out var prior)
                    ? prior.ContentHash
                    : null;
            manifests.Add(
                pair,
                BoundaryManifest.Create(
                    pair,
                    version,
                    chunkA,
                    chunkB,
                    shared.Order().ToArray(),
                    edges,
                    validStable,
                    previousHash));
        }

        return MeshResult<IReadOnlyDictionary<ChunkPair, BoundaryManifest>>.Ok(manifests);
    }

    private static bool ContainsEdge(this Triangle triangle, int a, int b)
    {
        for (var corner = 0; corner < 3; corner++)
        {
            var first = triangle[corner];
            var second = triangle[(corner + 1) % 3];
            if ((first == a && second == b) || (first == b && second == a))
            {
                return true;
            }
        }

        return false;
    }
}
