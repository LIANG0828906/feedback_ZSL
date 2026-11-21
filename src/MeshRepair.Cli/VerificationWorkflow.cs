using System.Globalization;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using MeshRepair.Core.Binary;
using MeshRepair.Core.Chunks;
using MeshRepair.Core.Editing;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;
using MeshRepair.Core.Recovery;
using MeshRepair.Core.Sample;
using MeshRepair.Core.Splice;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Cli;

public sealed class WorkflowChecks
{
    public bool BinaryRoundTrip { get; set; }

    public bool StaleHandleRejected { get; set; }

    public bool StaleBoundaryRejected { get; set; }

    public bool FillFailureAtomic { get; set; }

    public bool BusyChunkRejected { get; set; }

    public bool RepeatSubmitRejected { get; set; }

    public bool SnapshotIsolationDuringStagedEdit { get; set; }

    public bool DisjointSessionsMerged { get; set; }

    public bool RollbackInvalidatedSession { get; set; }

    public bool RollbackRecomputedBoundary { get; set; }

    public bool FinalSpliceValid { get; set; }

    public bool RecoveryMatchedPublished { get; set; }
}

public sealed class WorkflowSummary
{
    public int Seed { get; set; }

    public long HeadVersion { get; set; }

    public string HeadContentHash { get; set; } = string.Empty;

    public string TraceSha256 { get; set; } = string.Empty;

    public string VersionsSha256 { get; set; } = string.Empty;

    public long CurrentVertexCount { get; set; }

    public long CurrentFaceCount { get; set; }

    public int WeldVertexReduction { get; set; }

    public int FilledFaceCount { get; set; }

    public NormalDeviationStats NormalDeviation { get; set; } = new(0, 0, 0, 0);

    public RecoverySummary Recovery { get; set; } = new();

    public WorkflowChecks Checks { get; set; } = new();

    public string[] VersionLog { get; set; } = Array.Empty<string>();
}

public sealed class RecoverySummary
{
    public long RecoveredHead { get; set; }

    public long[] SkippedSnapshots { get; set; } = Array.Empty<long>();

    public string[] RecomputedBlocks { get; set; } = Array.Empty<string>();

    public int VerifiedBlockCount { get; set; }

    public bool PartialTemporaryFileIgnored { get; set; }
}

public sealed record WorkflowOutcome(
    WorkflowSummary Summary,
    string TraceText,
    string VersionsText,
    string BaselineText);

public static class VerificationWorkflow
{
    public static WorkflowOutcome Run(string outputDirectory, int seed)
    {
        Directory.CreateDirectory(outputDirectory);
        var snapshotDirectory = Path.Combine(outputDirectory, "snapshots");
        if (Directory.Exists(snapshotDirectory))
        {
            Directory.Delete(snapshotDirectory, recursive: true);
        }

        Directory.CreateDirectory(snapshotDirectory);
        var trace = new TraceWriter();
        var checks = new WorkflowChecks();
        var sample = MeshSampleGenerator.Generate(seed);
        var meshPath = Path.Combine(outputDirectory, "scan.mesh");
        var written = Require(MeshBinaryFormat.Write(meshPath, sample.Mesh, atomic: true));
        var loaded = Require(MeshBinaryFormat.Load(meshPath));
        checks.BinaryRoundTrip = loaded.ContentHash == sample.Mesh.ContentHash
            && loaded.Header.VertexCount == (uint)sample.Mesh.Vertices.Count
            && loaded.Header.FaceCount == (uint)sample.Mesh.Faces.Count
            && written.FileHash == loaded.FileHash;

        var mesh = loaded.Mesh;
        var layout = ChunkLayout.FromMesh(mesh, divisionsX: 2);
        var baseline = BaselineInfo.FromMesh(mesh);
        var repository = new SnapshotRepository(snapshotDirectory, layout, baseline);
        var room = Require(MeshRepairRoom.Create(mesh, layout, "baseline-loader", repository));
        trace.Publish(Require(room.GetVersion(room.CurrentVersion)));

        var pair = ChunkPair.Create(new ChunkId(0, 0, 0), new ChunkId(1, 0, 0));
        var initial = Require(room.GetVersion(1));
        var initialManifest = initial.Boundaries[pair];

        using (var handle = Require(room.AcquireSnapshot()))
        {
            var read = Require(handle.Read());
            checks.StaleHandleRejected = read.Version == 1;
            handle.Release();
            var stale = handle.Read();
            checks.StaleHandleRejected &= stale.Error?.Code == MeshErrorCode.E_STALE_HANDLE;
            if (stale.Error is not null)
            {
                trace.Reject("query-after-release", stale.Error);
            }
        }

        using (var badBoundary = Require(room.TryBeginEdit(
                   "repairer-stale",
                   new[] { pair.A, pair.B },
                   expectedVersion: 1)))
        {
            var rejected = badBoundary.WeldAcrossChunks(
                pair,
                expectedManifestVersion: 0,
                sample.WeldFirstVertexId,
                sample.WeldSecondVertexId);
            checks.StaleBoundaryRejected = rejected.Error?.Code == MeshErrorCode.E_BOUNDARY_STALE
                && rejected.Error.ChunkPair == pair.Id;
            if (rejected.Error is not null)
            {
                trace.Reject("weld-with-stale-boundary", rejected.Error);
            }
        }

        var weld = Require(room.TryBeginEdit(
            "repairer-alice",
            new[] { pair.A, pair.B },
            expectedVersion: 1));
        Require(weld.WeldAcrossChunks(
            pair,
            initialManifest.ManifestVersion,
            sample.WeldFirstVertexId,
            sample.WeldSecondVertexId));
        var welded = Require(weld.Submit());
        trace.Publish(welded);
        var weldedManifest = welded.Boundaries[pair];
        checks.StaleBoundaryRejected &= weldedManifest.StableVertexIds.Count > 0;

        var loopV2 = Require(room.EnumerateHoleLoops(welded.Version))
            .First(loop => loop.SpansChunks && loop.Touches(pair));
        var badFill = Require(room.TryBeginEdit(
            "repairer-bob",
            new[] { pair.A, pair.B },
            expectedVersion: welded.Version));
        var failedFill = badFill.FillHoleAcrossChunks(
            pair,
            loopV2.LoopId,
            weldedManifest.ManifestVersion,
            new[] { new Triangle(sample.WeldFirstVertexId, sample.WeldFirstVertexId, sample.WeldSecondVertexId) });
        checks.FillFailureAtomic = failedFill.Error?.Code == MeshErrorCode.E_FILL_FAILED
            && failedFill.Error.LoopId == loopV2.LoopId
            && room.CurrentVersion == welded.Version;
        if (failedFill.Error is not null)
        {
            trace.Reject("fill-invalid-face", failedFill.Error);
        }

        badFill.Cancel();

        var fill = Require(room.TryBeginEdit(
            "repairer-bob",
            new[] { pair.A, pair.B },
            expectedVersion: welded.Version));
        var holeA = FindRequiredVertex(welded, sample.HoleVertices.BottomLeft);
        var holeB = FindRequiredVertex(welded, sample.HoleVertices.BottomRight);
        var holeC = FindRequiredVertex(welded, sample.HoleVertices.TopRight);
        var holeD = FindRequiredVertex(welded, sample.HoleVertices.TopLeft);
        Require(fill.FillHoleAcrossChunks(
            pair,
            loopV2.LoopId,
            weldedManifest.ManifestVersion,
            new[]
            {
                new Triangle(holeA, holeB, holeC),
                new Triangle(holeA, holeC, holeD)
            }));
        var filled = Require(fill.Submit());
        trace.Publish(filled);

        var delete = Require(room.TryBeginEdit(
            "repairer-carol",
            new[] { new ChunkId(0, 0, 0) },
            expectedVersion: filled.Version));
        var deleted = Require(delete.DeleteSmallComponents(new ChunkId(0, 0, 0), maximumFaces: 1));
        if (deleted <= 0)
        {
            throw new InvalidOperationException("fragment deletion did not remove the isolated fragment");
        }

        var deletedVersion = Require(delete.Submit());
        trace.Publish(deletedVersion);

        var concurrentA = Require(room.TryBeginEdit(
            "repairer-dana",
            new[] { new ChunkId(0, 0, 0) },
            expectedVersion: deletedVersion.Version));
        var concurrentB = Require(room.TryBeginEdit(
            "repairer-eli",
            new[] { new ChunkId(1, 0, 0) },
            expectedVersion: deletedVersion.Version));
        var busy = room.TryBeginEdit(
            "repairer-frank",
            new[] { new ChunkId(0, 0, 0) },
            expectedVersion: deletedVersion.Version);
        checks.BusyChunkRejected = busy.Error?.Code == MeshErrorCode.E_BUSY;
        if (busy.Error is not null)
        {
            trace.Reject("busy-chunk", busy.Error);
        }

        Require(concurrentA.RecomputeNormals(new ChunkId(0, 0, 0)));
        Require(concurrentB.RecomputeNormals(new ChunkId(1, 0, 0)));
        using (var isolated = Require(room.AcquireSnapshot()))
        {
            var stagedRead = Require(isolated.Read());
            checks.SnapshotIsolationDuringStagedEdit = stagedRead.Version == deletedVersion.Version
                && stagedRead.ContentHash == deletedVersion.ContentHash
                && stagedRead.Chunks.Values.All(chunk =>
                    chunk.ContentHash == deletedVersion.Chunks[chunk.Id].ContentHash);
            trace.Note(
                "snapshot-isolation",
                stagedRead.Version,
                checks.SnapshotIsolationDuringStagedEdit ? "published-version-only" : "partial-state-observed");
        }

        var normalA = Require(concurrentA.Submit());
        trace.Publish(normalA);
        var repeated = concurrentA.Submit();
        checks.RepeatSubmitRejected = repeated.Error?.Code == MeshErrorCode.E_BUSY;
        if (repeated.Error is not null)
        {
            trace.Reject("repeat-submit", repeated.Error);
        }

        var normalB = Require(concurrentB.Submit());
        trace.Publish(normalB);
        checks.DisjointSessionsMerged = normalB.ParentVersion == normalA.Version
            && normalB.Chunks[new ChunkId(1, 0, 0)].ContentHash != deletedVersion.Chunks[new ChunkId(1, 0, 0)].ContentHash
            && normalB.Chunks[new ChunkId(0, 0, 0)].ContentHash == normalA.Chunks[new ChunkId(0, 0, 0)].ContentHash;

        var staleSession = Require(room.TryBeginEdit(
            "repairer-gina",
            new[] { pair.A, pair.B },
            expectedVersion: normalB.Version));
        Require(staleSession.RecomputeNormals(new ChunkId(0, 0, 0)));
        var rolledBack = Require(room.Rollback("repairer-hank", targetVersion: 1));
        trace.Publish(rolledBack);
        var staleCommit = staleSession.Submit();
        checks.RollbackInvalidatedSession = staleCommit.Error?.Code == MeshErrorCode.E_BOUNDARY_STALE;
        if (staleCommit.Error is not null)
        {
            trace.Reject("commit-after-rollback", staleCommit.Error);
        }

        var rollbackManifest = rolledBack.Boundaries[pair];
        var reWeld = Require(room.TryBeginEdit(
            "repairer-alice",
            new[] { pair.A, pair.B },
            expectedVersion: rolledBack.Version));
        Require(reWeld.WeldAcrossChunks(
            pair,
            rollbackManifest.ManifestVersion,
            sample.WeldFirstVertexId,
            sample.WeldSecondVertexId));
        var reWelded = Require(reWeld.Submit());
        trace.Publish(reWelded);
        var reWeldedManifest = reWelded.Boundaries[pair];

        var loopV8 = Require(room.EnumerateHoleLoops(reWelded.Version))
            .First(loop => loop.SpansChunks && loop.Touches(pair));
        var reFill = Require(room.TryBeginEdit(
            "repairer-bob",
            new[] { pair.A, pair.B },
            expectedVersion: reWelded.Version));
        var reA = FindRequiredVertex(reWelded, sample.HoleVertices.BottomLeft);
        var reB = FindRequiredVertex(reWelded, sample.HoleVertices.BottomRight);
        var reC = FindRequiredVertex(reWelded, sample.HoleVertices.TopRight);
        var reD = FindRequiredVertex(reWelded, sample.HoleVertices.TopLeft);
        Require(reFill.FillHoleAcrossChunks(
            pair,
            loopV8.LoopId,
            reWeldedManifest.ManifestVersion,
            new[]
            {
                new Triangle(reA, reB, reC),
                new Triangle(reA, reC, reD)
            }));
        var finalVersion = Require(reFill.Submit());
        trace.Publish(finalVersion);
        checks.RollbackRecomputedBoundary = reWeldedManifest.ManifestVersion == reWelded.Version
            && initialManifest.ContentHash != rollbackManifest.ContentHash
            && ReconcilableBoundaryVersion(reWeldedManifest, reWelded.Version);

        var splice = Require(room.CreateSpliceReport(finalVersion.Version));
        checks.FinalSpliceValid = splice.Version == finalVersion.Version
            && splice.CurrentVertexCount == finalVersion.VertexCount
            && splice.CurrentFaceCount == finalVersion.FaceCount;
        trace.Note(
            "splice",
            finalVersion.Version,
            $"vertices={splice.CurrentVertexCount};faces={splice.CurrentFaceCount};boundaryEdges={splice.BoundaryEdgeCount}");

        var partialTemp = Path.Combine(
            snapshotDirectory,
            $"snapshot-{(finalVersion.Version + 1).ToString("D10", CultureInfo.InvariantCulture)}.json.tmp");
        File.WriteAllText(partialTemp, "{\"partial\":true}\n", new UTF8Encoding(false));
        var recovery = Require(repository.LoadLatest());
        var recoveredRoom = Require(MeshRepairRoom.Restore(
            recovery.Versions,
            recovery.HeadVersion,
            recovery.Layout,
            recovery.Baseline,
            repository));
        var recoveredSplice = Require(recoveredRoom.CreateSpliceReport(recovery.HeadVersion));
        checks.RecoveryMatchedPublished = recovery.HeadVersion == finalVersion.Version
            && recoveredSplice.VersionContentHash == finalVersion.ContentHash
            && recovery.RecomputedBlocks.Count == 0
            && File.Exists(partialTemp);
        trace.Note(
            "recovery",
            recovery.HeadVersion,
            $"skippedTemp={(finalVersion.Version + 1).ToString(CultureInfo.InvariantCulture)};recomputed=0");

        var versionsText = BuildVersionsJson(room, finalVersion.Version);
        var traceText = trace.ToJsonLines();
        var baselineText = BuildBaselineJson(
            seed,
            written,
            mesh,
            layout,
            initial.Boundaries.Values);
        WriteUtf8(Path.Combine(outputDirectory, "trace.jsonl"), traceText);
        WriteUtf8(Path.Combine(outputDirectory, "versions.json"), versionsText);
        WriteUtf8(Path.Combine(outputDirectory, "baseline.json"), baselineText);
        WriteUtf8(
            Path.Combine(outputDirectory, "summary.json"),
            JsonSerializer.Serialize(
                new WorkflowSummary
                {
                    Seed = seed,
                    HeadVersion = finalVersion.Version,
                    HeadContentHash = finalVersion.ContentHash,
                    TraceSha256 = HashUtil.Sha256Hex(traceText),
                    VersionsSha256 = HashUtil.Sha256Hex(versionsText),
                    CurrentVertexCount = splice.CurrentVertexCount,
                    CurrentFaceCount = splice.CurrentFaceCount,
                    WeldVertexReduction = splice.WeldedVertexReduction,
                    FilledFaceCount = splice.FilledFaceCount,
                    NormalDeviation = splice.NormalDeviation,
                    Recovery = new RecoverySummary
                    {
                        RecoveredHead = recovery.HeadVersion,
                        SkippedSnapshots = recovery.SkippedSnapshots.ToArray(),
                        RecomputedBlocks = recovery.RecomputedBlocks.Select(chunk => chunk.ToString()).ToArray(),
                        VerifiedBlockCount = recovery.VerifiedBlocks.Count,
                        PartialTemporaryFileIgnored = File.Exists(partialTemp)
                    },
                    Checks = checks,
                    VersionLog = recovery.Versions.Select(version => version.Edit.Kind.ToString()).ToArray()
                },
                SnapshotJson.Options));

        var summary = JsonSerializer.Deserialize<WorkflowSummary>(
            File.ReadAllText(Path.Combine(outputDirectory, "summary.json")),
            SnapshotJson.Options) ?? throw new InvalidOperationException("summary serialization failed");
        return new WorkflowOutcome(summary, traceText, versionsText, baselineText);
    }

    private static bool ReconcilableBoundaryVersion(BoundaryManifest manifest, long version)
        => manifest.ManifestVersion == version
            && manifest.ChunkAVersion == version
            && manifest.ChunkBVersion == version;

    private static string BuildBaselineJson(
        int seed,
        MeshBinaryWriteResult written,
        MeshData mesh,
        ChunkLayout layout,
        IEnumerable<BoundaryManifest> boundaries)
    {
        var value = new
        {
            seed,
            format = "MRM1",
            meshVersion = mesh.Version,
            vertexCount = mesh.Vertices.Count,
            faceCount = mesh.Faces.Count,
            coordinateType = mesh.CoordinateType.ToString(),
            indexWidth = mesh.IndexWidth.ToString(),
            contentHash = mesh.ContentHash,
            fileHash = written.FileHash,
            layout = new
            {
                x = layout.DivisionsX,
                y = layout.DivisionsY,
                z = layout.DivisionsZ
            },
            boundaries = boundaries
                .OrderBy(manifest => manifest.Pair)
                .Select(manifest => new
                {
                    pair = manifest.Pair.Id,
                    manifestVersion = manifest.ManifestVersion,
                    contentHash = manifest.ContentHash,
                    sharedVertices = manifest.SharedVertexIds.Count,
                    boundaryEdges = manifest.Edges.Count
                })
                .ToArray()
        };
        return JsonSerializer.Serialize(value, SnapshotJson.Options);
    }

    private static string BuildVersionsJson(MeshRepairRoom room, long headVersion)
    {
        var versions = room.VersionIds
            .Where(version => version <= headVersion)
            .Select(version => Require(room.GetVersion(version)))
            .Select(version => new
            {
                version = version.Version,
                parent = version.ParentVersion,
                revertedFrom = version.RevertedFromVersion,
                editor = version.Editor,
                operation = version.Edit.Kind.ToString(),
                blocks = version.Edit.AffectedBlocks.Select(chunk => chunk.ToString()).Order().ToArray(),
                boundaryManifestVersion = version.Edit.BoundaryManifestVersion,
                boundaryContentHash = version.Edit.BoundaryContentHash,
                contentHash = version.ContentHash,
                commitWatermark = version.Edit.CommitWatermark,
                vertexCount = version.VertexCount,
                faceCount = version.FaceCount,
                chunks = version.Chunks.Values
                    .OrderBy(chunk => chunk.Id)
                    .Select(chunk => new
                    {
                        chunk = chunk.Id.ToString(),
                        hash = chunk.ContentHash,
                        vertices = chunk.Vertices.Count,
                        faces = chunk.Faces.Count,
                        normalRevision = chunk.NormalRevision
                    })
                    .ToArray(),
                boundaries = version.Boundaries.Values
                    .OrderBy(manifest => manifest.Pair)
                    .Select(manifest => new
                    {
                        pair = manifest.Pair.Id,
                        version = manifest.ManifestVersion,
                        hash = manifest.ContentHash,
                        previous = manifest.PreviousContentHash
                    })
                    .ToArray()
            })
            .ToArray();
        return JsonSerializer.Serialize(versions, SnapshotJson.Options);
    }

    private static T Require<T>(MeshResult<T> result)
        => result.IsSuccess
            ? result.RequireValue()
            : throw new InvalidOperationException(result.Error!.ToString());

    private static void Require(MeshResult result)
    {
        if (!result.IsSuccess)
        {
            throw new InvalidOperationException(result.Error!.ToString());
        }
    }

    private static int FindRequiredVertex(MeshVersion version, Point3 position)
        => VersionLookup.FindVertexId(version, position)
            ?? throw new InvalidOperationException($"vertex at {position.CanonicalString()} is missing");

    private static void WriteUtf8(string path, string value)
    {
        var directory = Path.GetDirectoryName(Path.GetFullPath(path));
        if (!string.IsNullOrEmpty(directory))
        {
            Directory.CreateDirectory(directory);
        }

        File.WriteAllText(path, value, new UTF8Encoding(false));
    }
}
