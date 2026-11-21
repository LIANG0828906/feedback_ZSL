using MeshRepair.Core.Chunks;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Core.Splice;

public static class SpliceValidation
{
    public static MeshResult<SpliceReport> CreateReport(MeshRepairRoom room, MeshVersion version)
    {
        var positions = new Dictionary<int, Point3>();
        var faces = new List<(ChunkId Chunk, FaceRecord Face)>();
        foreach (var chunk in version.Chunks.Values.OrderBy(chunk => chunk.Id))
        {
            foreach (var vertex in chunk.Vertices)
            {
                if (positions.TryGetValue(vertex.Key, out var existing)
                    && existing.DistanceTo(vertex.Value) > 1e-9)
                {
                    return Invalid(
                        $"shared vertex {vertex.Key} has divergent coordinates",
                        version,
                        chunkPair: $"{chunk.Id}",
                        edgeId: $"V:{vertex.Key}");
                }

                positions[vertex.Key] = vertex.Value;
            }

            faces.AddRange(chunk.Faces.Select(face => (chunk.Id, face)));
        }

        var duplicateFace = faces
            .GroupBy(entry => entry.Face.Triangle.CanonicalKey(), StringComparer.Ordinal)
            .FirstOrDefault(group => group.Count() > 1);
        if (duplicateFace is not null)
        {
            var chunks = duplicateFace.Select(entry => entry.Chunk).Distinct().Order().ToArray();
            var pair = chunks.Length > 1 ? ChunkPair.Create(chunks[0], chunks[1]).Id : chunks[0].ToString();
            return Invalid(
                "duplicate fill face detected during splice",
                version,
                chunkPair: pair,
                edgeId: $"FACE:{duplicateFace.First().Face.FaceId}");
        }

        var faceArray = faces.Select(entry => entry.Face.Triangle).ToArray();
        var faceChunk = faces
            .Select((entry, index) => (entry.Face.FaceId, entry.Chunk))
            .ToDictionary(entry => entry.FaceId, entry => entry.Chunk);
        var positionList = new Point3[positions.Keys.DefaultIfEmpty(0).Max() + 1];
        foreach (var position in positions)
        {
            positionList[position.Key] = position.Value;
        }

        var topology = MeshTopology.Build(positionList, faceArray, index => faces[index].Face.FaceId);

        foreach (var manifest in version.Boundaries.Values.OrderBy(manifest => manifest.Pair))
        {
            foreach (var boundaryEdge in manifest.Edges)
            {
                var key = EdgeKey.Create(boundaryEdge.A, boundaryEdge.B);
                if (!topology.Edges.TryGetValue(key, out var edge))
                {
                    return Invalid(
                        "boundary edge is missing after splice",
                        version,
                        chunkPair: manifest.Pair.Id,
                        edgeId: boundaryEdge.Id,
                        manifestVersion: manifest.ManifestVersion);
                }

                if (edge.Incidences.Count != 2)
                {
                    return Invalid(
                        $"boundary edge is referenced by {edge.Incidences.Count} faces instead of 2",
                        version,
                        chunkPair: manifest.Pair.Id,
                        edgeId: boundaryEdge.Id,
                        manifestVersion: manifest.ManifestVersion);
                }

                var incidences = edge.Incidences
                    .Select(incidence => (Incidence: incidence, Chunk: faceChunk[incidence.FaceId]))
                    .ToArray();
                var sides = incidences.Select(entry => entry.Chunk).Distinct().Order().ToArray();
                if (sides.Length != 2 || sides[0] != manifest.Pair.A || sides[1] != manifest.Pair.B)
                {
                    return Invalid(
                        "boundary edge is not referenced by exactly one face from each side",
                        version,
                        chunkPair: manifest.Pair.Id,
                        edgeId: boundaryEdge.Id,
                        manifestVersion: manifest.ManifestVersion);
                }

                var lowToHigh = incidences.Count(entry => entry.Incidence.Forward);
                if (lowToHigh != 1)
                {
                    return Invalid(
                        "boundary edge has folded or inconsistent winding",
                        version,
                        chunkPair: manifest.Pair.Id,
                        edgeId: boundaryEdge.Id,
                        manifestVersion: manifest.ManifestVersion);
                }
            }
        }

        var openEdgeCount = 0;
        foreach (var edge in topology.Edges.Values)
        {
            if (edge.Incidences.Count != 1)
            {
                continue;
            }

            openEdgeCount++;
            var owner = faceChunk[edge.Incidences[0].FaceId];
            var edgeId = $"BE:{edge.Key.Low}-{edge.Key.High}";
            foreach (var manifest in version.Boundaries.Values)
            {
                if (!manifest.Pair.Contains(owner))
                {
                    continue;
                }

                var shared = manifest.SharedVertexIds.ToHashSet();
                if (shared.Contains(edge.Key.Low) && shared.Contains(edge.Key.High))
                {
                    return Invalid(
                        "open crack detected on a negotiated chunk boundary",
                        version,
                        chunkPair: manifest.Pair.Id,
                        edgeId: edgeId,
                        manifestVersion: manifest.ManifestVersion);
                }
            }
        }

        if (version.VertexCount != positions.Count || version.FaceCount != faceArray.Length)
        {
            return Invalid(
                "splice vertex or face count is not conserved",
                version,
                edgeId: "COUNTS");
        }

        var cumulative = AccumulateEdits(room, version);
        var normalDeviation = MeshGeometry.ComputeNormalDeviation(version, room.Baseline);
        return MeshResult<SpliceReport>.Ok(
            new SpliceReport(
                version.Version,
                room.Baseline.VertexCount,
                positions.Count,
                room.Baseline.FaceCount,
                faceArray.Length,
                cumulative.WeldReduction,
                cumulative.FilledFaces,
                version.Boundaries.Values.Sum(manifest => manifest.Edges.Count),
                topology.NonManifoldEdges.Count(),
                openEdgeCount,
                normalDeviation,
                version.ContentHash));
    }

    private static (int WeldReduction, int FilledFaces) AccumulateEdits(
        MeshRepairRoom room,
        MeshVersion version)
    {
        var weld = 0;
        var filled = 0;
        var visited = new HashSet<long>();
        var current = version;
        while (current is not null && visited.Add(current.Version))
        {
            weld += current.Edit.WeldVertexReduction;
            filled += current.Edit.FilledFaceCount;
            if (current.Edit.Kind == MeshEditKind.Rollback && current.RevertedFromVersion.HasValue)
            {
                current = room.GetVersion(current.RevertedFromVersion.Value).Value;
            }
            else if (current.ParentVersion > 0)
            {
                current = room.GetVersion(current.ParentVersion).Value;
            }
            else
            {
                current = null;
            }
        }

        return (weld, filled);
    }

    private static MeshResult<SpliceReport> Invalid(
        string message,
        MeshVersion version,
        string? chunkPair = null,
        string? edgeId = null,
        long? manifestVersion = null)
        => MeshResult<SpliceReport>.Fail(
            MeshErrorCode.E_SPLICE_INVALID,
            message,
            chunkPair: chunkPair,
            edgeId: edgeId,
            manifestVersion: manifestVersion,
            snapshot: version.Version.ToString(System.Globalization.CultureInfo.InvariantCulture));
}
