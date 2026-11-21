using MeshRepair.Core.Chunks;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Core.Editing;

public sealed record BoundaryClaim(ChunkPair Pair, long ManifestVersion, string ManifestHash);

public sealed class EditSession : IDisposable
{
    private const int Open = 0;
    private const int Submitting = 1;
    private const int Completed = 2;
    private const int Cancelled = 3;
    private const int Failed = 4;

    private readonly MeshRepairRoom _room;
    private readonly CancellationToken _cancellationToken;
    private readonly object _sessionGate = new();
    private int _state;

    internal EditSession(
        MeshRepairRoom room,
        string editor,
        MeshVersion baseVersion,
        IReadOnlyList<ChunkId> acquiredChunks,
        CancellationToken cancellationToken)
    {
        _room = room;
        Editor = editor;
        BaseVersion = baseVersion;
        AcquiredChunks = acquiredChunks;
        _cancellationToken = cancellationToken;
        MutableChunks = acquiredChunks.ToDictionary(
            chunk => chunk,
            chunk => new MutableChunk(baseVersion.Chunks[chunk]));
    }

    public string Editor { get; }

    public MeshVersion BaseVersion { get; }

    public IReadOnlyList<ChunkId> AcquiredChunks { get; }

    public MeshEditKind PrimaryKind { get; private set; } = MeshEditKind.Load;

    public int WeldVertexReduction { get; private set; }

    public int FilledFaceCount { get; private set; }

    internal IReadOnlyDictionary<ChunkId, MutableChunk> MutableChunks { get; }

    internal Dictionary<ChunkPair, BoundaryClaim> BoundaryClaims { get; } = new();

    internal Dictionary<ChunkPair, Dictionary<string, int>> PendingStableVertices { get; } = new();

    public MeshResult Checkpoint()
    {
        if (Volatile.Read(ref _state) != Open)
        {
            return MeshResult.Fail(MeshErrorCode.E_CANCELLED, "edit session is no longer open");
        }

        return _cancellationToken.IsCancellationRequested
            ? MeshResult.Fail(MeshErrorCode.E_CANCELLED, "edit was cancelled at a chunk boundary checkpoint")
            : MeshResult.Ok();
    }

    public MeshResult<MeshVersion> Submit()
    {
        if (Interlocked.CompareExchange(ref _state, Submitting, Open) != Open)
        {
            return MeshResult<MeshVersion>.Fail(
                MeshErrorCode.E_BUSY,
                "edit session has already been submitted or cancelled",
                chunk: string.Join(",", AcquiredChunks.Select(chunk => chunk.ToString())));
        }

        if (_cancellationToken.IsCancellationRequested)
        {
            Interlocked.Exchange(ref _state, Cancelled);
            _room.ReleaseChunks(AcquiredChunks);
            return MeshResult<MeshVersion>.Fail(
                MeshErrorCode.E_CANCELLED,
                "edit was cancelled before publication");
        }

        var committed = _room.Commit(this);
        Interlocked.Exchange(ref _state, committed.IsSuccess ? Completed : Failed);
        _room.ReleaseChunks(AcquiredChunks);
        return committed;
    }

    public void Cancel()
    {
        if (Interlocked.CompareExchange(ref _state, Cancelled, Open) != Open)
        {
            return;
        }

        _room.ReleaseChunks(AcquiredChunks);
    }

    public void Dispose() => Cancel();

    public MeshResult WeldAcrossChunks(
        ChunkPair pair,
        long expectedManifestVersion,
        int firstVertex,
        int secondVertex)
    {
        var checkpoint = Checkpoint();
        if (!checkpoint.IsSuccess)
        {
            return checkpoint;
        }

        lock (_sessionGate)
        {
            var state = EnsureOpen();
            if (!state.IsSuccess)
            {
                return state;
            }

            if (!AcquiredChunks.Contains(pair.A) || !AcquiredChunks.Contains(pair.B))
            {
                return StalePair(pair, expectedManifestVersion, "both chunks must be acquired for a cross-chunk weld");
            }

            var manifestResult = RequireManifest(pair, expectedManifestVersion);
            if (!manifestResult.IsSuccess)
            {
                return MeshResult.Fail(manifestResult.Error!);
            }

            if (firstVertex == secondVertex)
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_INVALID_ARGUMENT,
                    "weld source vertices must be distinct",
                    chunkPair: pair.Id,
                    manifestVersion: expectedManifestVersion);
            }

            var positions = CollectPositions();
            if (!positions.TryGetValue(firstVertex, out var first) || !positions.TryGetValue(secondVertex, out var second))
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_INVALID_ARGUMENT,
                    "weld source vertex does not exist",
                    chunkPair: pair.Id,
                    manifestVersion: expectedManifestVersion);
            }

            if (ReferencesOutsideAcquired(new[] { firstVertex, secondVertex }, out var externalChunk))
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_BOUNDARY_STALE,
                    $"weld source vertex is still referenced by chunk {externalChunk}",
                    chunk: externalChunk.ToString(),
                    chunkPair: pair.Id,
                    manifestVersion: expectedManifestVersion);
            }

            var newVertexId = _room.ReserveVertexId();
            var welded = new Point3(
                (first.X + second.X) / 2.0,
                (first.Y + second.Y) / 2.0,
                (first.Z + second.Z) / 2.0);
            var stableKey = HashUtil.StableId(
                "BV",
                new[]
                {
                    pair.Id,
                    BaseVersion.Version.ToString(System.Globalization.CultureInfo.InvariantCulture),
                    Math.Min(firstVertex, secondVertex).ToString(System.Globalization.CultureInfo.InvariantCulture),
                    Math.Max(firstVertex, secondVertex).ToString(System.Globalization.CultureInfo.InvariantCulture),
                    welded.CanonicalString()
                });

            foreach (var mutable in MutableChunks.Values)
            {
                var touched = false;
                foreach (var face in mutable.Faces.Values.ToArray())
                {
                    var triangle = face.Triangle;
                    var changed = false;
                    if (triangle.A == firstVertex || triangle.A == secondVertex)
                    {
                        triangle = triangle with { A = newVertexId };
                        changed = true;
                    }

                    if (triangle.B == firstVertex || triangle.B == secondVertex)
                    {
                        triangle = triangle with { B = newVertexId };
                        changed = true;
                    }

                    if (triangle.C == firstVertex || triangle.C == secondVertex)
                    {
                        triangle = triangle with { C = newVertexId };
                        changed = true;
                    }

                    if (changed)
                    {
                        mutable.Faces[face.FaceId] = face with { Triangle = triangle };
                        touched = true;
                    }
                }

                if (touched)
                {
                    mutable.Vertices[newVertexId] = welded;
                }

                mutable.Vertices.Remove(firstVertex);
                mutable.Vertices.Remove(secondVertex);
                mutable.RemoveUnreferencedVertices();
            }

            if (!PendingStableVertices.TryGetValue(pair, out var stable))
            {
                stable = new Dictionary<string, int>(StringComparer.Ordinal);
                PendingStableVertices.Add(pair, stable);
            }

            stable[stableKey] = newVertexId;
            AddClaim(pair, expectedManifestVersion, manifestResult.RequireValue().ContentHash);
            WeldVertexReduction += 1;
            PrimaryKind = MeshEditKind.Weld;
            var after = Checkpoint();
            return after.IsSuccess ? MeshResult.Ok() : after;
        }
    }

    public MeshResult WeldWithinChunk(ChunkId chunk, int keepVertex, int removeVertex)
    {
        var checkpoint = Checkpoint();
        if (!checkpoint.IsSuccess)
        {
            return checkpoint;
        }

        lock (_sessionGate)
        {
            var state = EnsureOpen();
            if (!state.IsSuccess)
            {
                return state;
            }

            if (!MutableChunks.TryGetValue(chunk, out var mutable))
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_NOT_FOUND,
                    $"chunk {chunk} is not acquired by this edit",
                    chunk: chunk.ToString());
            }

            if (keepVertex == removeVertex)
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_INVALID_ARGUMENT,
                    "weld vertices must be distinct",
                    chunk: chunk.ToString());
            }

            if (ReferencesOutsideChunk(new[] { removeVertex }, chunk, out var externalChunk))
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_BOUNDARY_STALE,
                    $"vertex {removeVertex} is shared with chunk {externalChunk}",
                    chunk: externalChunk.ToString());
            }

            var positions = CollectPositions();
            if (!positions.ContainsKey(keepVertex) || !positions.ContainsKey(removeVertex))
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_INVALID_ARGUMENT,
                    "weld vertex does not exist",
                    chunk: chunk.ToString());
            }

            mutable.Vertices[keepVertex] = positions[keepVertex];
            foreach (var face in mutable.Faces.Values.ToArray())
            {
                var triangle = face.Triangle;
                if (triangle.A == removeVertex)
                {
                    triangle = triangle with { A = keepVertex };
                }

                if (triangle.B == removeVertex)
                {
                    triangle = triangle with { B = keepVertex };
                }

                if (triangle.C == removeVertex)
                {
                    triangle = triangle with { C = keepVertex };
                }

                mutable.Faces[face.FaceId] = face with { Triangle = triangle };
            }

            mutable.Vertices.Remove(removeVertex);
            mutable.RemoveUnreferencedVertices();
            WeldVertexReduction += 1;
            PrimaryKind = MeshEditKind.Weld;
            var after = Checkpoint();
            return after.IsSuccess ? MeshResult.Ok() : after;
        }
    }

    public MeshResult FillHoleAcrossChunks(
        ChunkPair pair,
        string loopId,
        long expectedManifestVersion,
        IReadOnlyList<Triangle> fillFaces)
    {
        var checkpoint = Checkpoint();
        if (!checkpoint.IsSuccess)
        {
            return checkpoint;
        }

        lock (_sessionGate)
        {
            var state = EnsureOpen();
            if (!state.IsSuccess)
            {
                return state;
            }

            if (!AcquiredChunks.Contains(pair.A) || !AcquiredChunks.Contains(pair.B))
            {
                return FillFailed(pair, loopId, expectedManifestVersion, "both endpoint chunks must be acquired");
            }

            var manifestResult = RequireManifest(pair, expectedManifestVersion);
            if (!manifestResult.IsSuccess)
            {
                return FillFailed(pair, loopId, expectedManifestVersion, manifestResult.Error!.Message);
            }

            var loop = HoleLoopFinder.Find(BaseVersion)
                .FirstOrDefault(candidate => candidate.LoopId == loopId);
            if (loop is null || !loop.Touches(pair))
            {
                return FillFailed(pair, loopId, expectedManifestVersion, "hole loop does not span the requested chunk pair");
            }

            if (fillFaces.Count == 0)
            {
                return FillFailed(pair, loopId, expectedManifestVersion, "fill produced no faces");
            }

            var positions = CollectPositions();
            var existingFaceKeys = BaseVersion.Chunks.Values
                .SelectMany(chunk => chunk.Faces)
                .Select(face => face.Triangle.CanonicalKey())
                .ToHashSet(StringComparer.Ordinal);
            var fillKeys = new HashSet<string>(StringComparer.Ordinal);
            foreach (var face in fillFaces)
            {
                if (face.IsDegenerate)
                {
                    return FillFailed(pair, loopId, expectedManifestVersion, "fill contains a degenerate face");
                }

                if (!positions.ContainsKey(face.A)
                    || !positions.ContainsKey(face.B)
                    || !positions.ContainsKey(face.C))
                {
                    return FillFailed(pair, loopId, expectedManifestVersion, "fill references a missing vertex");
                }

                var key = face.CanonicalKey();
                if (!fillKeys.Add(key))
                {
                    return FillFailed(pair, loopId, expectedManifestVersion, "fill contains duplicate faces");
                }

                if (existingFaceKeys.Contains(key))
                {
                    return FillFailed(pair, loopId, expectedManifestVersion, "fill duplicates an existing face");
                }
            }

            var faceIds = _room.ReserveFaceIds(fillFaces.Count);
            var planned = new List<(ChunkId Owner, FaceRecord Face)>();
            var ownerSet = new HashSet<ChunkId>();
            for (var index = 0; index < fillFaces.Count; index++)
            {
                var owner = _room.Layout.AssignFace(faceIds[index], fillFaces[index], positions).Owner;
                if (owner != pair.A && owner != pair.B)
                {
                    return FillFailed(
                        pair,
                        loopId,
                        expectedManifestVersion,
                        $"fill face owner {owner} is outside the negotiated pair");
                }

                ownerSet.Add(owner);
                planned.Add((owner, new FaceRecord(faceIds[index], fillFaces[index])));
            }

            if (!ownerSet.Contains(pair.A) || !ownerSet.Contains(pair.B))
            {
                return FillFailed(
                    pair,
                    loopId,
                    expectedManifestVersion,
                    "fill did not produce geometry for both endpoint chunks");
            }

            foreach (var item in planned)
            {
                var mutable = MutableChunks[item.Owner];
                var triangle = item.Face.Triangle;
                foreach (var vertexId in new[] { triangle.A, triangle.B, triangle.C })
                {
                    mutable.Vertices[vertexId] = positions[vertexId];
                }

                mutable.Faces.Add(item.Face.FaceId, item.Face);
            }

            AddClaim(pair, expectedManifestVersion, manifestResult.RequireValue().ContentHash);
            FilledFaceCount += fillFaces.Count;
            PrimaryKind = MeshEditKind.FillHole;
            var after = Checkpoint();
            return after.IsSuccess ? MeshResult.Ok() : after;
        }
    }

    public MeshResult FillHoleWithinChunk(
        ChunkId chunk,
        string loopId,
        IReadOnlyList<Triangle> fillFaces)
    {
        var checkpoint = Checkpoint();
        if (!checkpoint.IsSuccess)
        {
            return checkpoint;
        }

        lock (_sessionGate)
        {
            var state = EnsureOpen();
            if (!state.IsSuccess)
            {
                return state;
            }

            if (!MutableChunks.TryGetValue(chunk, out var mutable))
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_NOT_FOUND,
                    $"chunk {chunk} is not acquired by this edit",
                    chunk: chunk.ToString());
            }

            var loop = HoleLoopFinder.Find(BaseVersion)
                .FirstOrDefault(candidate => candidate.LoopId == loopId);
            if (loop is null || !loop.Chunks.Contains(chunk))
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_FILL_FAILED,
                    "hole loop does not belong to the requested chunk",
                    chunk: chunk.ToString(),
                    loopId: loopId);
            }

            if (loop.SpansChunks)
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_FILL_FAILED,
                    "loop spans chunks and requires boundary negotiation",
                    chunk: chunk.ToString(),
                    loopId: loopId);
            }

            var positions = CollectPositions();
            var existingFaceKeys = BaseVersion.Chunks.Values
                .SelectMany(baseChunk => baseChunk.Faces)
                .Select(face => face.Triangle.CanonicalKey())
                .ToHashSet(StringComparer.Ordinal);
            var fillKeys = new HashSet<string>(StringComparer.Ordinal);
            foreach (var face in fillFaces)
            {
                if (face.IsDegenerate || !positions.ContainsKey(face.A)
                    || !positions.ContainsKey(face.B)
                    || !positions.ContainsKey(face.C))
                {
                    return MeshResult.Fail(
                        MeshErrorCode.E_FILL_FAILED,
                        "fill contains an invalid face",
                        chunk: chunk.ToString(),
                        loopId: loopId);
                }

                var key = face.CanonicalKey();
                if (!fillKeys.Add(key) || existingFaceKeys.Contains(key))
                {
                    return MeshResult.Fail(
                        MeshErrorCode.E_FILL_FAILED,
                        "fill contains a duplicate face",
                        chunk: chunk.ToString(),
                        loopId: loopId);
                }
            }

            var ids = _room.ReserveFaceIds(fillFaces.Count);
            for (var index = 0; index < fillFaces.Count; index++)
            {
                var face = fillFaces[index];
                var owner = _room.Layout.AssignFace(ids[index], face, positions).Owner;
                if (owner != chunk)
                {
                    return MeshResult.Fail(
                        MeshErrorCode.E_FILL_FAILED,
                        $"fill face owner {owner} differs from chunk {chunk}",
                        chunk: chunk.ToString(),
                        loopId: loopId);
                }

                foreach (var vertexId in new[] { face.A, face.B, face.C })
                {
                    mutable.Vertices[vertexId] = positions[vertexId];
                }

                mutable.Faces.Add(ids[index], new FaceRecord(ids[index], face));
            }

            FilledFaceCount += fillFaces.Count;
            PrimaryKind = MeshEditKind.FillHole;
            var after = Checkpoint();
            return after.IsSuccess ? MeshResult.Ok() : after;
        }
    }

    public MeshResult<int> DeleteSmallComponents(ChunkId chunk, int maximumFaces)
    {
        var checkpoint = Checkpoint();
        if (!checkpoint.IsSuccess)
        {
            return MeshResult<int>.Fail(checkpoint.Error!);
        }

        lock (_sessionGate)
        {
            var state = EnsureOpen();
            if (!state.IsSuccess)
            {
                return MeshResult<int>.Fail(state.Error!);
            }

            if (!MutableChunks.TryGetValue(chunk, out var mutable))
            {
                return MeshResult<int>.Fail(
                    MeshErrorCode.E_NOT_FOUND,
                    $"chunk {chunk} is not acquired by this edit",
                    chunk: chunk.ToString());
            }

            if (maximumFaces < 0)
            {
                return MeshResult<int>.Fail(
                    MeshErrorCode.E_INVALID_ARGUMENT,
                    "maximum fragment face count cannot be negative",
                    chunk: chunk.ToString());
            }

            var components = FindComponents(mutable.Faces.Values).ToArray();
            var remove = new HashSet<ulong>();
            foreach (var component in components)
            {
                if (component.Count <= maximumFaces)
                {
                    remove.UnionWith(component.Select(face => face.FaceId));
                }
            }

            foreach (var faceId in remove)
            {
                mutable.Faces.Remove(faceId);
            }

            mutable.RemoveUnreferencedVertices();
            PrimaryKind = MeshEditKind.DeleteFragments;
            var after = Checkpoint();
            return after.IsSuccess
                ? MeshResult<int>.Ok(remove.Count)
                : MeshResult<int>.Fail(after.Error!);
        }
    }

    public MeshResult RecomputeNormals(ChunkId chunk)
    {
        var checkpoint = Checkpoint();
        if (!checkpoint.IsSuccess)
        {
            return checkpoint;
        }

        lock (_sessionGate)
        {
            var state = EnsureOpen();
            if (!state.IsSuccess)
            {
                return state;
            }

            if (!MutableChunks.TryGetValue(chunk, out var mutable))
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_NOT_FOUND,
                    $"chunk {chunk} is not acquired by this edit",
                    chunk: chunk.ToString());
            }

            mutable.NormalRevision += 1;
            PrimaryKind = MeshEditKind.RecomputeNormals;
            var after = Checkpoint();
            return after;
        }
    }

    private MeshResult EnsureOpen()
        => Volatile.Read(ref _state) == Open
            ? MeshResult.Ok()
            : MeshResult.Fail(
                MeshErrorCode.E_CANCELLED,
                "edit session is no longer open");

    private MeshResult<BoundaryManifest> RequireManifest(ChunkPair pair, long expectedVersion)
    {
        if (!BaseVersion.Boundaries.TryGetValue(pair, out var manifest))
        {
            return MeshResult<BoundaryManifest>.Fail(
                MeshErrorCode.E_BOUNDARY_STALE,
                $"boundary manifest is missing for pair {pair}",
                chunkPair: pair.Id,
                manifestVersion: expectedVersion);
        }

        if (manifest.ManifestVersion != expectedVersion)
        {
            return MeshResult<BoundaryManifest>.Fail(
                MeshErrorCode.E_BOUNDARY_STALE,
                $"boundary manifest version {manifest.ManifestVersion} differs from requested {expectedVersion}",
                chunkPair: pair.Id,
                manifestVersion: expectedVersion);
        }

        if (!BaseVersion.Chunks.TryGetValue(pair.A, out var chunkA)
            || !BaseVersion.Chunks.TryGetValue(pair.B, out var chunkB)
            || !manifest.MatchesChunks(chunkA, chunkB))
        {
            return MeshResult<BoundaryManifest>.Fail(
                MeshErrorCode.E_BOUNDARY_STALE,
                "one side of the boundary is based on a different chunk version",
                chunkPair: pair.Id,
                manifestVersion: expectedVersion);
        }

        return MeshResult<BoundaryManifest>.Ok(manifest);
    }

    private void AddClaim(ChunkPair pair, long version, string hash)
    {
        var claim = new BoundaryClaim(pair, version, hash);
        if (BoundaryClaims.TryGetValue(pair, out var existing)
            && (existing.ManifestVersion != version || existing.ManifestHash != hash))
        {
            throw new InvalidOperationException("an edit session cannot mix boundary contract versions");
        }

        BoundaryClaims[pair] = claim;
    }

    private Dictionary<int, Point3> CollectPositions()
    {
        var positions = BaseVersion.Chunks.Values
            .SelectMany(chunk => chunk.Vertices)
            .GroupBy(pair => pair.Key)
            .ToDictionary(group => group.Key, group => group.First().Value);
        foreach (var mutable in MutableChunks.Values)
        {
            foreach (var pair in mutable.Vertices)
            {
                positions[pair.Key] = pair.Value;
            }
        }

        return positions;
    }

    private bool ReferencesOutsideAcquired(IEnumerable<int> vertexIds, out ChunkId chunk)
    {
        var ids = vertexIds.ToHashSet();
        foreach (var baseChunk in BaseVersion.Chunks.Values)
        {
            if (AcquiredChunks.Contains(baseChunk.Id))
            {
                continue;
            }

            if (baseChunk.Faces.Any(face =>
                    ids.Contains(face.Triangle.A)
                    || ids.Contains(face.Triangle.B)
                    || ids.Contains(face.Triangle.C)))
            {
                chunk = baseChunk.Id;
                return true;
            }
        }

        chunk = default;
        return false;
    }

    private bool ReferencesOutsideChunk(IEnumerable<int> vertexIds, ChunkId allowedChunk, out ChunkId chunk)
    {
        var ids = vertexIds.ToHashSet();
        foreach (var baseChunk in BaseVersion.Chunks.Values)
        {
            if (baseChunk.Id == allowedChunk)
            {
                continue;
            }

            if (baseChunk.Faces.Any(face =>
                    ids.Contains(face.Triangle.A)
                    || ids.Contains(face.Triangle.B)
                    || ids.Contains(face.Triangle.C)))
            {
                chunk = baseChunk.Id;
                return true;
            }
        }

        chunk = default;
        return false;
    }

    private static MeshResult StalePair(ChunkPair pair, long version, string message)
        => MeshResult.Fail(
            MeshErrorCode.E_BOUNDARY_STALE,
            message,
            chunkPair: pair.Id,
            manifestVersion: version);

    private static MeshResult FillFailed(ChunkPair pair, string loopId, long version, string message)
        => MeshResult.Fail(
            MeshErrorCode.E_FILL_FAILED,
            message,
            chunkPair: pair.Id,
            manifestVersion: version,
            loopId: loopId);

    private static IEnumerable<IReadOnlyList<FaceRecord>> FindComponents(IEnumerable<FaceRecord> faces)
    {
        var faceArray = faces.OrderBy(face => face.FaceId).ToArray();
        var byVertex = new Dictionary<int, List<int>>();
        for (var index = 0; index < faceArray.Length; index++)
        {
            var face = faceArray[index].Triangle;
            foreach (var vertex in new[] { face.A, face.B, face.C })
            {
                if (!byVertex.TryGetValue(vertex, out var indices))
                {
                    indices = new List<int>();
                    byVertex.Add(vertex, indices);
                }

                indices.Add(index);
            }
        }

        var visited = new bool[faceArray.Length];
        for (var start = 0; start < faceArray.Length; start++)
        {
            if (visited[start])
            {
                continue;
            }

            var component = new List<FaceRecord>();
            var queue = new Queue<int>();
            queue.Enqueue(start);
            visited[start] = true;
            while (queue.Count > 0)
            {
                var index = queue.Dequeue();
                component.Add(faceArray[index]);
                var triangle = faceArray[index].Triangle;
                foreach (var vertex in new[] { triangle.A, triangle.B, triangle.C })
                {
                    foreach (var neighbor in byVertex[vertex])
                    {
                        if (!visited[neighbor])
                        {
                            visited[neighbor] = true;
                            queue.Enqueue(neighbor);
                        }
                    }
                }
            }

            yield return component;
        }
    }
}
