using System.Text.Json;
using System.Text.Json.Serialization;
using MeshRepair.Core.Chunks;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Core.Recovery;

public sealed class SnapshotLayoutDto
{
    public int DivisionsX { get; set; }

    public int DivisionsY { get; set; }

    public int DivisionsZ { get; set; }

    public double MinX { get; set; }

    public double MinY { get; set; }

    public double MinZ { get; set; }

    public double MaxX { get; set; }

    public double MaxY { get; set; }

    public double MaxZ { get; set; }

    public double InterfaceTolerance { get; set; }

    public ChunkLayout ToLayout()
        => new(
            DivisionsX,
            DivisionsY,
            DivisionsZ,
            new Bounds3(new Point3(MinX, MinY, MinZ), new Point3(MaxX, MaxY, MaxZ)),
            InterfaceTolerance);

    public static SnapshotLayoutDto FromLayout(ChunkLayout layout)
        => new()
        {
            DivisionsX = layout.DivisionsX,
            DivisionsY = layout.DivisionsY,
            DivisionsZ = layout.DivisionsZ,
            MinX = layout.Bounds.Min.X,
            MinY = layout.Bounds.Min.Y,
            MinZ = layout.Bounds.Min.Z,
            MaxX = layout.Bounds.Max.X,
            MaxY = layout.Bounds.Max.Y,
            MaxZ = layout.Bounds.Max.Z,
            InterfaceTolerance = layout.InterfaceTolerance
        };
}

public sealed class SnapshotNormalDto
{
    public ulong FaceId { get; set; }

    public double X { get; set; }

    public double Y { get; set; }

    public double Z { get; set; }
}

public sealed class SnapshotBaselineDto
{
    public int VertexCount { get; set; }

    public int FaceCount { get; set; }

    public string ContentHash { get; set; } = string.Empty;

    public List<SnapshotNormalDto> FaceNormals { get; set; } = new();

    public BaselineInfo ToBaseline()
        => new(
            VertexCount,
            FaceCount,
            ContentHash,
            FaceNormals.ToDictionary(
                normal => normal.FaceId,
                normal => new Point3(normal.X, normal.Y, normal.Z)));

    public static SnapshotBaselineDto FromBaseline(BaselineInfo baseline)
        => new()
        {
            VertexCount = baseline.VertexCount,
            FaceCount = baseline.FaceCount,
            ContentHash = baseline.ContentHash,
            FaceNormals = baseline.FaceNormals
                .OrderBy(pair => pair.Key)
                .Select(pair => new SnapshotNormalDto
                {
                    FaceId = pair.Key,
                    X = pair.Value.X,
                    Y = pair.Value.Y,
                    Z = pair.Value.Z
                })
                .ToList()
        };
}

public sealed class SnapshotVertexDto
{
    public int Id { get; set; }

    public double X { get; set; }

    public double Y { get; set; }

    public double Z { get; set; }
}

public sealed class SnapshotFaceDto
{
    public ulong Id { get; set; }

    public int A { get; set; }

    public int B { get; set; }

    public int C { get; set; }
}

public sealed class SnapshotChunkDto
{
    public int X { get; set; }

    public int Y { get; set; }

    public int Z { get; set; }

    public long Version { get; set; }

    public int NormalRevision { get; set; }

    public string ContentHash { get; set; } = string.Empty;

    public List<SnapshotVertexDto> Vertices { get; set; } = new();

    public List<SnapshotFaceDto> Faces { get; set; } = new();
}

public sealed class SnapshotBoundaryEdgeDto
{
    public string Id { get; set; } = string.Empty;

    public int A { get; set; }

    public int B { get; set; }

    public int ChunkAX { get; set; }

    public int ChunkAY { get; set; }

    public int ChunkAZ { get; set; }

    public int ChunkBX { get; set; }

    public int ChunkBY { get; set; }

    public int ChunkBZ { get; set; }
}

public sealed class SnapshotStableVertexDto
{
    public string StableId { get; set; } = string.Empty;

    public int VertexId { get; set; }
}

public sealed class SnapshotBoundaryDto
{
    public int AX { get; set; }

    public int AY { get; set; }

    public int AZ { get; set; }

    public int BX { get; set; }

    public int BY { get; set; }

    public int BZ { get; set; }

    public long ManifestVersion { get; set; }

    public long ChunkAVersion { get; set; }

    public long ChunkBVersion { get; set; }

    public string ChunkAContentHash { get; set; } = string.Empty;

    public string ChunkBContentHash { get; set; } = string.Empty;

    public string? PreviousContentHash { get; set; }

    public string ContentHash { get; set; } = string.Empty;

    public List<int> SharedVertexIds { get; set; } = new();

    public List<SnapshotBoundaryEdgeDto> Edges { get; set; } = new();

    public List<SnapshotStableVertexDto> StableVertexIds { get; set; } = new();
}

public sealed class SnapshotEditDto
{
    public string Kind { get; set; } = string.Empty;

    public string Editor { get; set; } = string.Empty;

    public List<SnapshotChunkIdDto> AffectedBlocks { get; set; } = new();

    public long? BoundaryManifestVersion { get; set; }

    public string? BoundaryContentHash { get; set; }

    public int WeldVertexReduction { get; set; }

    public int FilledFaceCount { get; set; }

    public long CommitWatermark { get; set; }
}

public sealed class SnapshotChunkIdDto
{
    public int X { get; set; }

    public int Y { get; set; }

    public int Z { get; set; }
}

public sealed class SnapshotDocument
{
    public string Schema { get; set; } = "mesh-repair-snapshot-v1";

    public long Version { get; set; }

    public long ParentVersion { get; set; }

    public long? RevertedFromVersion { get; set; }

    public string Editor { get; set; } = string.Empty;

    public string ContentHash { get; set; } = string.Empty;

    public int VertexCount { get; set; }

    public int FaceCount { get; set; }

    public SnapshotLayoutDto Layout { get; set; } = new();

    public SnapshotBaselineDto Baseline { get; set; } = new();

    public SnapshotEditDto Edit { get; set; } = new();

    public List<SnapshotChunkDto> Chunks { get; set; } = new();

    public List<SnapshotBoundaryDto> Boundaries { get; set; } = new();

    public string? PreviousSnapshotHash { get; set; }

    public string SelfHash { get; set; } = string.Empty;

    public static SnapshotDocument FromVersion(
        MeshVersion version,
        ChunkLayout layout,
        BaselineInfo baseline,
        string? previousSnapshotHash)
        => new()
        {
            Version = version.Version,
            ParentVersion = version.ParentVersion,
            RevertedFromVersion = version.RevertedFromVersion,
            Editor = version.Editor,
            ContentHash = version.ContentHash,
            VertexCount = version.VertexCount,
            FaceCount = version.FaceCount,
            Layout = SnapshotLayoutDto.FromLayout(layout),
            Baseline = SnapshotBaselineDto.FromBaseline(baseline),
            Edit = new SnapshotEditDto
            {
                Kind = version.Edit.Kind.ToString(),
                Editor = version.Edit.Editor,
                AffectedBlocks = version.Edit.AffectedBlocks
                    .Order()
                    .Select(chunk => new SnapshotChunkIdDto { X = chunk.X, Y = chunk.Y, Z = chunk.Z })
                    .ToList(),
                BoundaryManifestVersion = version.Edit.BoundaryManifestVersion,
                BoundaryContentHash = version.Edit.BoundaryContentHash,
                WeldVertexReduction = version.Edit.WeldVertexReduction,
                FilledFaceCount = version.Edit.FilledFaceCount,
                CommitWatermark = version.Edit.CommitWatermark
            },
            Chunks = version.Chunks.Values
                .OrderBy(chunk => chunk.Id)
                .Select(chunk => new SnapshotChunkDto
                {
                    X = chunk.Id.X,
                    Y = chunk.Id.Y,
                    Z = chunk.Id.Z,
                    Version = chunk.Version,
                    NormalRevision = chunk.NormalRevision,
                    ContentHash = chunk.ContentHash,
                    Vertices = chunk.Vertices
                        .OrderBy(pair => pair.Key)
                        .Select(pair => new SnapshotVertexDto
                        {
                            Id = pair.Key,
                            X = pair.Value.X,
                            Y = pair.Value.Y,
                            Z = pair.Value.Z
                        })
                        .ToList(),
                    Faces = chunk.Faces
                        .OrderBy(face => face.FaceId)
                        .Select(face => new SnapshotFaceDto
                        {
                            Id = face.FaceId,
                            A = face.Triangle.A,
                            B = face.Triangle.B,
                            C = face.Triangle.C
                        })
                        .ToList()
                })
                .ToList(),
            Boundaries = version.Boundaries.Values
                .OrderBy(manifest => manifest.Pair)
                .Select(manifest => new SnapshotBoundaryDto
                {
                    AX = manifest.Pair.A.X,
                    AY = manifest.Pair.A.Y,
                    AZ = manifest.Pair.A.Z,
                    BX = manifest.Pair.B.X,
                    BY = manifest.Pair.B.Y,
                    BZ = manifest.Pair.B.Z,
                    ManifestVersion = manifest.ManifestVersion,
                    ChunkAVersion = manifest.ChunkAVersion,
                    ChunkBVersion = manifest.ChunkBVersion,
                    ChunkAContentHash = manifest.ChunkAContentHash,
                    ChunkBContentHash = manifest.ChunkBContentHash,
                    PreviousContentHash = manifest.PreviousContentHash,
                    ContentHash = manifest.ContentHash,
                    SharedVertexIds = manifest.SharedVertexIds.Order().ToList(),
                    Edges = manifest.Edges
                        .OrderBy(edge => edge.A)
                        .ThenBy(edge => edge.B)
                        .Select(edge => new SnapshotBoundaryEdgeDto
                        {
                            Id = edge.Id,
                            A = edge.A,
                            B = edge.B,
                            ChunkAX = edge.ChunkA.X,
                            ChunkAY = edge.ChunkA.Y,
                            ChunkAZ = edge.ChunkA.Z,
                            ChunkBX = edge.ChunkB.X,
                            ChunkBY = edge.ChunkB.Y,
                            ChunkBZ = edge.ChunkB.Z
                        })
                        .ToList(),
                    StableVertexIds = manifest.StableVertexIds
                        .OrderBy(pair => pair.Key, StringComparer.Ordinal)
                        .Select(pair => new SnapshotStableVertexDto
                        {
                            StableId = pair.Key,
                            VertexId = pair.Value
                        })
                        .ToList()
                })
                .ToList(),
            PreviousSnapshotHash = previousSnapshotHash
        };

    public MeshResult<(MeshVersion Version, ChunkLayout Layout, BaselineInfo Baseline)> ToVersion()
    {
        if (Version <= 0)
        {
            return RecoveryInvalid("snapshot version field is missing or invalid", field: "Version");
        }

        if (!Enum.TryParse<MeshEditKind>(Edit.Kind, out var editKind))
        {
            return RecoveryInvalid("snapshot edit kind is invalid", field: "Edit.Kind");
        }

        ChunkLayout layout;
        BaselineInfo baseline;
        try
        {
            layout = Layout.ToLayout();
            baseline = Baseline.ToBaseline();
        }
        catch (Exception exception) when (exception is ArgumentException or FormatException)
        {
            return RecoveryInvalid($"snapshot configuration is invalid: {exception.Message}", field: "Layout");
        }

        var chunks = new Dictionary<ChunkId, ChunkSnapshot>();
        var positions = new Dictionary<int, Point3>();
        var allFaces = new List<(ChunkId Chunk, FaceRecord Face)>();
        var faceIds = new HashSet<ulong>();
        foreach (var chunkDto in Chunks.OrderBy(chunk => new ChunkId(chunk.X, chunk.Y, chunk.Z)))
        {
            var chunkId = new ChunkId(chunkDto.X, chunkDto.Y, chunkDto.Z);
            if (chunkDto.Version != Version)
            {
                return RecoveryInvalid(
                    $"chunk {chunkId} version {chunkDto.Version} differs from snapshot version {Version}",
                    chunk: chunkId.ToString(),
                    snapshot: Version);
            }

            var vertices = new Dictionary<int, Point3>();
            foreach (var vertex in chunkDto.Vertices.OrderBy(vertex => vertex.Id))
            {
                var point = new Point3(vertex.X, vertex.Y, vertex.Z);
                if (!point.IsFinite)
                {
                    return RecoveryInvalid("snapshot vertex is not finite", chunk: chunkId.ToString(), field: $"Vertices[{vertex.Id}]");
                }

                vertices[vertex.Id] = point;
                if (positions.TryGetValue(vertex.Id, out var existing)
                    && existing.DistanceTo(point) > 1e-9)
                {
                    return RecoveryInvalid(
                        $"shared vertex {vertex.Id} has divergent coordinates",
                        chunk: chunkId.ToString(),
                        field: $"Vertices[{vertex.Id}]");
                }

                positions[vertex.Id] = point;
            }

            var faces = new List<FaceRecord>();
            foreach (var faceDto in chunkDto.Faces.OrderBy(face => face.Id))
            {
                var triangle = new Triangle(faceDto.A, faceDto.B, faceDto.C);
                foreach (var vertexId in new[] { triangle.A, triangle.B, triangle.C })
                {
                    if (!vertices.ContainsKey(vertexId))
                    {
                        return RecoveryInvalid(
                            $"face {faceDto.Id} references missing vertex {vertexId}",
                            chunk: chunkId.ToString(),
                            field: $"Faces[{faceDto.Id}]");
                    }
                }

                if (!faceIds.Add(faceDto.Id))
                {
                    return RecoveryInvalid(
                        $"face id {faceDto.Id} appears in more than one chunk",
                        chunk: chunkId.ToString(),
                        field: $"Faces[{faceDto.Id}]");
                }

                var record = new FaceRecord(faceDto.Id, triangle);
                faces.Add(record);
                allFaces.Add((chunkId, record));
            }

            var hash = ChunkSnapshot.ComputeContentHash(chunkId, vertices, faces, chunkDto.NormalRevision);
            if (hash != chunkDto.ContentHash)
            {
                return RecoveryInvalid(
                    $"chunk {chunkId} content hash does not match",
                    chunk: chunkId.ToString(),
                    snapshot: Version);
            }

            chunks.Add(
                chunkId,
                new ChunkSnapshot(
                    chunkId,
                    chunkDto.Version,
                    vertices,
                    faces,
                    chunkDto.NormalRevision,
                    chunkDto.ContentHash));
        }

        var assignments = new Dictionary<ulong, FaceAssignment>();
        foreach (var entry in allFaces)
        {
            var assignment = layout.AssignFace(entry.Face.FaceId, entry.Face.Triangle, positions);
            if (assignment.Owner != entry.Chunk)
            {
                return RecoveryInvalid(
                    $"face {entry.Face.FaceId} is stored in chunk {entry.Chunk} but belongs to {assignment.Owner}",
                    chunk: entry.Chunk.ToString(),
                    snapshot: Version);
            }

            assignments[entry.Face.FaceId] = assignment;
        }

        var boundaries = new Dictionary<ChunkPair, BoundaryManifest>();
        foreach (var boundaryDto in Boundaries.OrderBy(boundary => boundary.AX)
                     .ThenBy(boundary => boundary.AY)
                     .ThenBy(boundary => boundary.AZ)
                     .ThenBy(boundary => boundary.BX)
                     .ThenBy(boundary => boundary.BY)
                     .ThenBy(boundary => boundary.BZ))
        {
            var pair = ChunkPair.Create(
                new ChunkId(boundaryDto.AX, boundaryDto.AY, boundaryDto.AZ),
                new ChunkId(boundaryDto.BX, boundaryDto.BY, boundaryDto.BZ));
            if (!chunks.TryGetValue(pair.A, out var chunkA) || !chunks.TryGetValue(pair.B, out var chunkB))
            {
                return RecoveryInvalid(
                    $"boundary pair {pair} references a missing chunk",
                    chunkPair: pair.Id,
                    snapshot: Version);
            }

            var edges = boundaryDto.Edges
                .Select(edge => new BoundaryEdge(
                    edge.Id,
                    edge.A,
                    edge.B,
                    new ChunkId(edge.ChunkAX, edge.ChunkAY, edge.ChunkAZ),
                    new ChunkId(edge.ChunkBX, edge.ChunkBY, edge.ChunkBZ)))
                .ToArray();
            var stable = boundaryDto.StableVertexIds
                .OrderBy(entry => entry.StableId, StringComparer.Ordinal)
                .ToDictionary(entry => entry.StableId, entry => entry.VertexId, StringComparer.Ordinal);
            var manifest = BoundaryManifest.Create(
                pair,
                boundaryDto.ManifestVersion,
                chunkA,
                chunkB,
                boundaryDto.SharedVertexIds,
                edges,
                stable,
                boundaryDto.PreviousContentHash);
            if (manifest.ContentHash != boundaryDto.ContentHash)
            {
                return RecoveryInvalid(
                    $"boundary manifest hash does not match for pair {pair}",
                    chunk: chunkA.Id.ToString(),
                    chunkPair: pair.Id,
                    snapshot: Version);
            }

            if (manifest.ManifestVersion != Version
                || manifest.ChunkAVersion != Version
                || manifest.ChunkBVersion != Version)
            {
                return RecoveryInvalid(
                    "boundary manifest version chain does not match the snapshot version",
                    chunkPair: pair.Id,
                    manifestVersion: manifest.ManifestVersion,
                    snapshot: Version);
            }

            boundaries.Add(pair, manifest);
        }

        var vertexCount = chunks.Values.SelectMany(chunk => chunk.Vertices.Keys).Distinct().Count();
        var faceCount = chunks.Values.Sum(chunk => chunk.Faces.Count);
        if (vertexCount != VertexCount || faceCount != FaceCount)
        {
            return RecoveryInvalid(
                "snapshot vertex or face count does not match chunk contents",
                field: "Counts",
                snapshot: Version);
        }

        var contentHash = MeshVersion.ComputeContentHash(Version, chunks, boundaries, vertexCount, faceCount);
        if (contentHash != ContentHash)
        {
            return RecoveryInvalid(
                "snapshot version content hash does not match",
                field: "ContentHash",
                snapshot: Version);
        }

        var edit = new MeshEditInfo(
            editKind,
            Edit.Editor,
            Edit.AffectedBlocks
                .Select(chunk => new ChunkId(chunk.X, chunk.Y, chunk.Z))
                .Order()
                .ToArray(),
            Edit.BoundaryManifestVersion,
            Edit.BoundaryContentHash,
            Edit.WeldVertexReduction,
            Edit.FilledFaceCount,
            Edit.CommitWatermark);
        var version = new MeshVersion(
            Version,
            ParentVersion,
            Editor,
            edit,
            chunks,
            boundaries,
            assignments,
            vertexCount,
            faceCount,
            ContentHash,
            RevertedFromVersion);
        return MeshResult<(MeshVersion, ChunkLayout, BaselineInfo)>.Ok((version, layout, baseline));
    }

    public string ComputeSelfHash()
    {
        var original = SelfHash;
        SelfHash = string.Empty;
        var json = JsonSerializer.Serialize(this, SnapshotJson.Options);
        SelfHash = original;
        return HashUtil.Sha256Hex(json);
    }

    private static MeshResult<(MeshVersion, ChunkLayout, BaselineInfo)> RecoveryInvalid(
        string message,
        string? field = null,
        string? chunk = null,
        string? chunkPair = null,
        long? manifestVersion = null,
        long? snapshot = null)
        => MeshResult<(MeshVersion, ChunkLayout, BaselineInfo)>.Fail(
            MeshErrorCode.E_RECOVERY_INVALID,
            message,
            field: field,
            chunk: chunk,
            chunkPair: chunkPair,
            manifestVersion: manifestVersion,
            snapshot: snapshot?.ToString(System.Globalization.CultureInfo.InvariantCulture));
}

public static class SnapshotJson
{
    public static readonly JsonSerializerOptions Options = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        WriteIndented = false,
        DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull
    };
}
