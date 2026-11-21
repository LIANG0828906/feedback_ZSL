using MeshRepair.Core.Chunks;
using MeshRepair.Core.Editing;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;
using MeshRepair.Core.Splice;

namespace MeshRepair.Core.Versioning;

public sealed class MeshRepairRoom
{
    private readonly object _gate = new();
    private readonly Dictionary<long, MeshVersion> _versions = new();
    private readonly HashSet<ChunkId> _activeChunks = new();
    private readonly IVersionSnapshotSink? _snapshotSink;
    private long _headVersion;
    private long _commitWatermark;
    private int _nextVertexId;
    private ulong _nextFaceId;

    private MeshRepairRoom(
        ChunkLayout layout,
        BaselineInfo baseline,
        MeshVersion initialVersion,
        IVersionSnapshotSink? snapshotSink)
    {
        Layout = layout;
        Baseline = baseline;
        _snapshotSink = snapshotSink;
        _versions.Add(initialVersion.Version, initialVersion);
        _headVersion = initialVersion.Version;
        _commitWatermark = initialVersion.Edit.CommitWatermark;
        _nextVertexId = initialVersion.Chunks.Values.SelectMany(chunk => chunk.Vertices.Keys).DefaultIfEmpty(-1).Max() + 1;
        _nextFaceId = initialVersion.Chunks.Values.SelectMany(chunk => chunk.Faces).Select(face => face.FaceId).DefaultIfEmpty(0UL).Max() + 1;
    }

    public ChunkLayout Layout { get; }

    public BaselineInfo Baseline { get; }

    public long CurrentVersion
    {
        get
        {
            lock (_gate)
            {
                return _headVersion;
            }
        }
    }

    public static MeshResult<MeshRepairRoom> Create(
        MeshData mesh,
        ChunkLayout layout,
        string editor = "baseline",
        IVersionSnapshotSink? snapshotSink = null)
    {
        var positions = new Dictionary<int, Point3>();
        for (var index = 0; index < mesh.Vertices.Count; index++)
        {
            positions[index] = mesh.Vertices[index];
        }

        var faces = new List<FaceRecord>();
        for (var index = 0; index < mesh.Faces.Count; index++)
        {
            faces.Add(new FaceRecord((ulong)(index + 1), mesh.Faces[index]));
        }

        var seedChunk = ChunkSnapshot.Create(
            new ChunkId(0, 0, 0),
            1,
            positions,
            faces,
            0);
        var edit = new MeshEditInfo(
            MeshEditKind.Load,
            editor,
            Array.Empty<ChunkId>(),
            null,
            null,
            0,
            0,
            1);
        var assembled = VersionAssembler.Assemble(
            1,
            0,
            editor,
            edit,
            new Dictionary<ChunkId, ChunkSnapshot> { [seedChunk.Id] = seedChunk },
            layout);
        if (!assembled.IsSuccess)
        {
            return MeshResult<MeshRepairRoom>.Fail(assembled.Error!);
        }

        var initial = assembled.RequireValue().Version;
        var initialEdit = edit with
        {
            BoundaryManifestVersion = initial.Boundaries.Count > 0 ? 1 : null,
            BoundaryContentHash = initial.Boundaries.Values
                .OrderBy(manifest => manifest.Pair)
                .Select(manifest => manifest.ContentHash)
                .FirstOrDefault()
        };
        initial = initial.WithRecomputedPublication(
            initial.Version,
            initial.ParentVersion,
            initial.Editor,
            initialEdit,
            initial.Chunks,
            initial.Boundaries,
            initial.Assignments,
            initial.VertexCount,
            initial.FaceCount,
            initial.RevertedFromVersion);
        var room = new MeshRepairRoom(layout, BaselineInfo.FromMesh(mesh), initial, snapshotSink);
        if (snapshotSink is not null)
        {
            var saved = snapshotSink.Save(initial);
            if (!saved.IsSuccess)
            {
                return MeshResult<MeshRepairRoom>.Fail(saved.Error!);
            }
        }

        return MeshResult<MeshRepairRoom>.Ok(room);
    }

    public static MeshResult<MeshRepairRoom> Restore(
        IReadOnlyList<MeshVersion> versions,
        long headVersion,
        ChunkLayout layout,
        BaselineInfo baseline,
        IVersionSnapshotSink? snapshotSink = null)
    {
        if (versions.Count == 0)
        {
            return MeshResult<MeshRepairRoom>.Fail(
                MeshErrorCode.E_RECOVERY_INVALID,
                "no recoverable versions were supplied",
                field: "Version");
        }

        var head = versions.FirstOrDefault(version => version.Version == headVersion);
        if (head is null)
        {
            return MeshResult<MeshRepairRoom>.Fail(
                MeshErrorCode.E_RECOVERY_INVALID,
                $"head version {headVersion} is not present",
                field: "Version");
        }

        var room = new MeshRepairRoom(layout, baseline, head, snapshotSink);
        foreach (var version in versions.OrderBy(version => version.Version))
        {
            room._versions[version.Version] = version;
        }

        room._headVersion = headVersion;
        room._commitWatermark = versions.Max(version => version.Edit.CommitWatermark);
        room._nextVertexId = versions
            .SelectMany(version => version.Chunks.Values)
            .SelectMany(chunk => chunk.Vertices.Keys)
            .DefaultIfEmpty(-1)
            .Max() + 1;
        room._nextFaceId = versions
            .SelectMany(version => version.Chunks.Values)
            .SelectMany(chunk => chunk.Faces)
            .Select(face => face.FaceId)
            .DefaultIfEmpty(0UL)
            .Max() + 1;
        return MeshResult<MeshRepairRoom>.Ok(room);
    }

    public MeshResult<MeshSnapshotHandle> AcquireSnapshot()
    {
        lock (_gate)
        {
            if (!_versions.TryGetValue(_headVersion, out var version))
            {
                return MeshResult<MeshSnapshotHandle>.Fail(
                    MeshErrorCode.E_NOT_FOUND,
                    "published mesh version is unavailable",
                    field: "Version");
            }

            return MeshResult<MeshSnapshotHandle>.Ok(new MeshSnapshotHandle(version));
        }
    }

    public MeshResult<MeshVersion> GetVersion(long versionId)
    {
        lock (_gate)
        {
            return _versions.TryGetValue(versionId, out var version)
                ? MeshResult<MeshVersion>.Ok(version)
                : MeshResult<MeshVersion>.Fail(
                    MeshErrorCode.E_NOT_FOUND,
                    $"mesh version {versionId} is not available",
                    field: "Version");
        }
    }

    public IReadOnlyList<long> VersionIds
    {
        get
        {
            lock (_gate)
            {
                return _versions.Keys.Order().ToArray();
            }
        }
    }

    public MeshResult<EditSession> TryBeginEdit(
        string editor,
        IEnumerable<ChunkId> chunks,
        long? expectedVersion = null,
        CancellationToken cancellationToken = default)
    {
        var requested = chunks.Distinct().Order().ToArray();
        if (requested.Length == 0)
        {
            return MeshResult<EditSession>.Fail(
                MeshErrorCode.E_INVALID_ARGUMENT,
                "an edit must acquire at least one chunk",
                field: "Chunks");
        }

        lock (_gate)
        {
            var current = _versions[_headVersion];
            if (expectedVersion.HasValue && expectedVersion.Value != current.Version)
            {
                return MeshResult<EditSession>.Fail(
                    MeshErrorCode.E_BOUNDARY_STALE,
                    $"edit expected version {expectedVersion.Value} but current version is {current.Version}",
                    manifestVersion: current.Version);
            }

            foreach (var chunk in requested)
            {
                if (!current.Chunks.ContainsKey(chunk))
                {
                    return MeshResult<EditSession>.Fail(
                        MeshErrorCode.E_NOT_FOUND,
                        $"chunk {chunk} is not present in version {current.Version}",
                        chunk: chunk.ToString());
                }

                if (_activeChunks.Contains(chunk))
                {
                    return MeshResult<EditSession>.Fail(
                        MeshErrorCode.E_BUSY,
                        $"chunk {chunk} already has an active edit task",
                        chunk: chunk.ToString());
                }
            }

            foreach (var chunk in requested)
            {
                _activeChunks.Add(chunk);
            }

            var session = new EditSession(
                this,
                editor,
                current,
                requested,
                cancellationToken);
            return MeshResult<EditSession>.Ok(session);
        }
    }

    public MeshResult<MeshVersion> Rollback(string editor, long targetVersion)
    {
        lock (_gate)
        {
            if (!_versions.TryGetValue(targetVersion, out var target))
            {
                return MeshResult<MeshVersion>.Fail(
                    MeshErrorCode.E_NOT_FOUND,
                    $"rollback target {targetVersion} is not available",
                    field: "Version");
            }

            var current = _versions[_headVersion];
            var newVersionId = _headVersion + 1;
            var affected = target.Chunks.Keys.Order().ToArray();
            var edit = new MeshEditInfo(
                MeshEditKind.Rollback,
                editor,
                affected,
                newVersionId,
                null,
                0,
                0,
                ++_commitWatermark);
            var stable = target.Boundaries.ToDictionary(
                pair => pair.Key,
                pair => (IReadOnlyDictionary<string, int>)pair.Value.StableVertexIds);
            var assembled = VersionAssembler.Assemble(
                newVersionId,
                current.Version,
                editor,
                edit,
                target.Chunks,
                Layout,
                target.Boundaries,
                stable,
                targetVersion);
            if (!assembled.IsSuccess)
            {
                return MeshResult<MeshVersion>.Fail(assembled.Error!);
            }

            var version = assembled.RequireValue().Version;
            var finalEdit = edit with
            {
                BoundaryContentHash = version.Boundaries.Values
                    .OrderBy(manifest => manifest.Pair)
                    .Select(manifest => manifest.ContentHash)
                    .FirstOrDefault()
            };
            version = version.WithRecomputedPublication(
                version.Version,
                version.ParentVersion,
                version.Editor,
                finalEdit,
                version.Chunks,
                version.Boundaries,
                version.Assignments,
                version.VertexCount,
                version.FaceCount,
                version.RevertedFromVersion);

            var saved = _snapshotSink?.Save(version);
            if (saved is { IsSuccess: false })
            {
                return MeshResult<MeshVersion>.Fail(saved.Error!);
            }

            _versions.Add(version.Version, version);
            _headVersion = version.Version;
            return MeshResult<MeshVersion>.Ok(version);
        }
    }

    public MeshResult<SpliceReport> CreateSpliceReport(long? versionId = null)
    {
        MeshVersion? version;
        lock (_gate)
        {
            var id = versionId ?? _headVersion;
            _versions.TryGetValue(id, out version);
        }

        if (version is null)
        {
            return MeshResult<SpliceReport>.Fail(
                MeshErrorCode.E_NOT_FOUND,
                $"mesh version {versionId} is not available",
                field: "Version");
        }

        return SpliceValidation.CreateReport(this, version);
    }

    public MeshResult<IReadOnlyList<HoleLoop>> EnumerateHoleLoops(long? versionId = null)
    {
        MeshVersion? version;
        lock (_gate)
        {
            var id = versionId ?? _headVersion;
            _versions.TryGetValue(id, out version);
        }

        if (version is null)
        {
            return MeshResult<IReadOnlyList<HoleLoop>>.Fail(
                MeshErrorCode.E_NOT_FOUND,
                $"mesh version {versionId} is not available",
                field: "Version");
        }

        return MeshResult<IReadOnlyList<HoleLoop>>.Ok(HoleLoopFinder.Find(version));
    }

    internal int ReserveVertexId()
    {
        lock (_gate)
        {
            return _nextVertexId++;
        }
    }

    internal IReadOnlyList<ulong> ReserveFaceIds(int count)
    {
        lock (_gate)
        {
            var ids = new ulong[count];
            for (var index = 0; index < count; index++)
            {
                ids[index] = _nextFaceId++;
            }

            return ids;
        }
    }

    internal MeshResult<MeshVersion> Commit(EditSession session)
    {
        lock (_gate)
        {
            var current = _versions[_headVersion];
            foreach (var chunk in session.AcquiredChunks)
            {
                if (!current.Chunks.TryGetValue(chunk, out var latest)
                    || !session.BaseVersion.Chunks.TryGetValue(chunk, out var baseline)
                    || latest.ContentHash != baseline.ContentHash)
                {
                    return MeshResult<MeshVersion>.Fail(
                        MeshErrorCode.E_BOUNDARY_STALE,
                        $"chunk {chunk} changed after the edit began",
                        chunk: chunk.ToString(),
                        manifestVersion: current.Version);
                }
            }

            foreach (var claim in session.BoundaryClaims.Values)
            {
                if (!current.Boundaries.TryGetValue(claim.Pair, out var manifest)
                    || manifest.ManifestVersion != claim.ManifestVersion
                    || manifest.ContentHash != claim.ManifestHash)
                {
                    return MeshResult<MeshVersion>.Fail(
                        MeshErrorCode.E_BOUNDARY_STALE,
                        $"boundary contract changed for pair {claim.Pair}",
                        chunkPair: claim.Pair.Id,
                        manifestVersion: claim.ManifestVersion);
                }
            }

            var newVersionId = current.Version + 1;
            var chunks = current.Chunks.ToDictionary(pair => pair.Key, pair => pair.Value);
            var allPositions = current.Chunks.Values
                .SelectMany(chunk => chunk.Vertices)
                .GroupBy(pair => pair.Key)
                .ToDictionary(group => group.Key, group => group.First().Value);
            foreach (var mutable in session.MutableChunks.Values)
            {
                chunks[mutable.Id] = mutable.ToSnapshot(newVersionId, allPositions);
            }

            var stableOverrides = session.PendingStableVertices.ToDictionary(
                pair => pair.Key,
                pair => (IReadOnlyDictionary<string, int>)pair.Value);
            var edit = new MeshEditInfo(
                session.PrimaryKind,
                session.Editor,
                session.AcquiredChunks,
                current.Boundaries.Count > 0 ? newVersionId : null,
                null,
                session.WeldVertexReduction,
                session.FilledFaceCount,
                ++_commitWatermark);
            var assembled = VersionAssembler.Assemble(
                newVersionId,
                current.Version,
                session.Editor,
                edit,
                chunks,
                Layout,
                current.Boundaries,
                stableOverrides);
            if (!assembled.IsSuccess)
            {
                return MeshResult<MeshVersion>.Fail(assembled.Error!);
            }

            var version = assembled.RequireValue().Version;
            var primaryPair = session.BoundaryClaims.Values
                .OrderBy(claim => claim.Pair)
                .Select(claim => claim.Pair)
                .FirstOrDefault();
            var boundaryHash = version.Boundaries.TryGetValue(primaryPair, out var primaryManifest)
                ? primaryManifest.ContentHash
                : version.Boundaries.Values.OrderBy(manifest => manifest.Pair).Select(manifest => manifest.ContentHash).FirstOrDefault();
            var finalEdit = edit with { BoundaryContentHash = boundaryHash };
            version = version.WithRecomputedPublication(
                version.Version,
                version.ParentVersion,
                version.Editor,
                finalEdit,
                version.Chunks,
                version.Boundaries,
                version.Assignments,
                version.VertexCount,
                version.FaceCount,
                version.RevertedFromVersion);

            var saved = _snapshotSink?.Save(version);
            if (saved is { IsSuccess: false })
            {
                return MeshResult<MeshVersion>.Fail(saved.Error!);
            }

            _versions.Add(version.Version, version);
            _headVersion = version.Version;
            return MeshResult<MeshVersion>.Ok(version);
        }
    }

    internal void ReleaseChunks(IEnumerable<ChunkId> chunks)
    {
        lock (_gate)
        {
            foreach (var chunk in chunks)
            {
                _activeChunks.Remove(chunk);
            }
        }
    }
}
