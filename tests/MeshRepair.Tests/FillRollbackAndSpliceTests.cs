using MeshRepair.Core.Chunks;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Model;
using MeshRepair.Core.Splice;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Tests;

public sealed class FillRollbackAndSpliceTests
{
    [Fact]
    public void InvalidFill_IsAtomicAndReturnsLoopAndManifestVersion()
    {
        using var fixture = TestRoom.Create();
        var welded = Weld(fixture);
        var manifest = welded.Boundaries[fixture.Pair];
        var loop = fixture.Room.EnumerateHoleLoops(welded.Version).RequireValue()
            .First(candidate => candidate.SpansChunks && candidate.Touches(fixture.Pair));
        var session = fixture.Room.TryBeginEdit("filler", new[] { fixture.Pair.A, fixture.Pair.B }).RequireValue();
        var rejected = session.FillHoleAcrossChunks(
            fixture.Pair,
            loop.LoopId,
            manifest.ManifestVersion,
            new[] { new Triangle(0, 0, 1) });
        Assert.Equal(MeshErrorCode.E_FILL_FAILED, rejected.Error!.Code);
        Assert.Equal(loop.LoopId, rejected.Error.LoopId);
        Assert.Equal(manifest.ManifestVersion, rejected.Error.ManifestVersion);
        session.Cancel();
        Assert.Equal(welded.Version, fixture.Room.CurrentVersion);
    }

    [Fact]
    public void CrossChunkFill_ProducesValidSplice()
    {
        using var fixture = TestRoom.Create();
        var welded = Weld(fixture);
        var filled = Fill(fixture, welded);
        var report = fixture.Room.CreateSpliceReport(filled.Version);
        Assert.True(report.IsSuccess, report.Error?.ToString());
        Assert.Equal(filled.VertexCount, report.RequireValue().CurrentVertexCount);
        Assert.Equal(filled.FaceCount, report.RequireValue().CurrentFaceCount);
        Assert.Equal(2, report.RequireValue().FilledFaceCount);
    }

    [Fact]
    public void Rollback_PublishesNewManifestAndInvalidatesOldSessions()
    {
        using var fixture = TestRoom.Create();
        var welded = Weld(fixture);
        var oldManifest = welded.Boundaries[fixture.Pair];
        var pending = fixture.Room.TryBeginEdit("pending", new[] { fixture.Pair.A }).RequireValue();
        Assert.True(pending.RecomputeNormals(fixture.Pair.A).IsSuccess);
        var rollback = fixture.Room.Rollback("rollback", 1);
        Assert.True(rollback.IsSuccess, rollback.Error?.ToString());
        var version = rollback.RequireValue();
        Assert.Equal(welded.Version + 1, version.Version);
        Assert.Equal(1, version.RevertedFromVersion);
        Assert.Equal(version.Version, version.Boundaries[fixture.Pair].ManifestVersion);
        Assert.NotEqual(oldManifest.ContentHash, version.Boundaries[fixture.Pair].ContentHash);
        var stale = pending.Submit();
        Assert.Equal(MeshErrorCode.E_BOUNDARY_STALE, stale.Error!.Code);
    }

    [Fact]
    public void Splice_ReportsMissingBoundaryEdgeWithPairAndEdgeId()
    {
        using var fixture = TestRoom.Create();
        var original = fixture.Room.GetVersion(1).RequireValue();
        var manifest = original.Boundaries[fixture.Pair];
        var edge = manifest.Edges[0];
        var chunk = original.Chunks[fixture.Pair.A];
        var removed = chunk.Faces.First(face => ContainsEdge(face.Triangle, edge.A, edge.B));
        var alteredChunk = ChunkSnapshot.Create(
            chunk.Id,
            original.Version,
            chunk.Vertices,
            chunk.Faces.Where(face => face.FaceId != removed.FaceId),
            chunk.NormalRevision);
        var chunks = original.Chunks.ToDictionary(pair => pair.Key, pair => pair.Value);
        chunks[chunk.Id] = alteredChunk;
        var malformed = Rebuild(original, chunks);
        var result = SpliceValidation.CreateReport(fixture.Room, malformed);
        Assert.Equal(MeshErrorCode.E_SPLICE_INVALID, result.Error!.Code);
        Assert.Equal(fixture.Pair.Id, result.Error.ChunkPair);
        Assert.Contains(edge.Id, result.Error.EdgeId);
    }

    [Fact]
    public void Splice_ReportsFlippedBoundaryFace()
    {
        using var fixture = TestRoom.Create();
        var original = fixture.Room.GetVersion(1).RequireValue();
        var manifest = original.Boundaries[fixture.Pair];
        var edge = manifest.Edges[0];
        var chunk = original.Chunks[fixture.Pair.A];
        var target = chunk.Faces.First(face => ContainsEdge(face.Triangle, edge.A, edge.B));
        var flipped = new FaceRecord(
            target.FaceId,
            new Triangle(target.Triangle.A, target.Triangle.C, target.Triangle.B));
        var alteredChunk = ChunkSnapshot.Create(
            chunk.Id,
            original.Version,
            chunk.Vertices,
            chunk.Faces.Select(face => face.FaceId == target.FaceId ? flipped : face),
            chunk.NormalRevision);
        var chunks = original.Chunks.ToDictionary(pair => pair.Key, pair => pair.Value);
        chunks[chunk.Id] = alteredChunk;
        var malformed = Rebuild(original, chunks);
        var result = SpliceValidation.CreateReport(fixture.Room, malformed);
        Assert.Equal(MeshErrorCode.E_SPLICE_INVALID, result.Error!.Code);
        Assert.Equal(edge.Id, result.Error.EdgeId);
    }

    private static MeshVersion Weld(TestRoom fixture)
    {
        var version = fixture.Room.GetVersion(1).RequireValue();
        var session = fixture.Room.TryBeginEdit("welder", new[] { fixture.Pair.A, fixture.Pair.B }).RequireValue();
        Assert.True(session.WeldAcrossChunks(
            fixture.Pair,
            version.Boundaries[fixture.Pair].ManifestVersion,
            fixture.Sample.WeldFirstVertexId,
            fixture.Sample.WeldSecondVertexId).IsSuccess);
        return session.Submit().RequireValue();
    }

    private static MeshVersion Fill(TestRoom fixture, MeshVersion welded)
    {
        var loop = fixture.Room.EnumerateHoleLoops(welded.Version).RequireValue()
            .First(candidate => candidate.SpansChunks && candidate.Touches(fixture.Pair));
        var a = VersionLookup.FindVertexId(welded, fixture.Sample.HoleVertices.BottomLeft)!.Value;
        var b = VersionLookup.FindVertexId(welded, fixture.Sample.HoleVertices.BottomRight)!.Value;
        var c = VersionLookup.FindVertexId(welded, fixture.Sample.HoleVertices.TopRight)!.Value;
        var d = VersionLookup.FindVertexId(welded, fixture.Sample.HoleVertices.TopLeft)!.Value;
        var session = fixture.Room.TryBeginEdit("filler", new[] { fixture.Pair.A, fixture.Pair.B }).RequireValue();
        Assert.True(session.FillHoleAcrossChunks(
            fixture.Pair,
            loop.LoopId,
            welded.Boundaries[fixture.Pair].ManifestVersion,
            new[] { new Triangle(a, b, c), new Triangle(a, c, d) }).IsSuccess);
        return session.Submit().RequireValue();
    }

    private static MeshVersion Rebuild(
        MeshVersion original,
        IReadOnlyDictionary<ChunkId, ChunkSnapshot> chunks)
    {
        var vertices = chunks.Values.SelectMany(chunk => chunk.Vertices.Keys).Distinct().Count();
        var faces = chunks.Values.Sum(chunk => chunk.Faces.Count);
        var hash = MeshVersion.ComputeContentHash(original.Version, chunks, original.Boundaries, vertices, faces);
        return new MeshVersion(
            original.Version,
            original.ParentVersion,
            original.Editor,
            original.Edit,
            chunks,
            original.Boundaries,
            original.Assignments,
            vertices,
            faces,
            hash);
    }

    private static bool ContainsEdge(Triangle triangle, int a, int b)
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
