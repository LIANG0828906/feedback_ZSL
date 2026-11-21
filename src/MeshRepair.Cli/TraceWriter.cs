using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using MeshRepair.Core.Chunks;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Cli;

public sealed class TraceBoundary
{
    public string Pair { get; set; } = string.Empty;

    public long Version { get; set; }

    public string Hash { get; set; } = string.Empty;
}

public sealed class TraceRecord
{
    public long Seq { get; set; }

    public string Event { get; set; } = string.Empty;

    public long? Version { get; set; }

    public long? Parent { get; set; }

    public string? Editor { get; set; }

    public string? Operation { get; set; }

    public string[]? Blocks { get; set; }

    public long? BoundaryManifestVersion { get; set; }

    public string? BoundaryContentHash { get; set; }

    public List<TraceBoundary>? Boundaries { get; set; }

    public string? ContentHash { get; set; }

    public long? CommitWatermark { get; set; }

    public string? ErrorCode { get; set; }

    public string? Field { get; set; }

    public string? Chunk { get; set; }

    public string? ChunkPair { get; set; }

    public long? ExpectedManifestVersion { get; set; }

    public string? LoopId { get; set; }

    public string? EdgeId { get; set; }

    public string? Detail { get; set; }
}

public sealed class TraceWriter
{
    private readonly List<TraceRecord> _records = new();
    private long _sequence;

    public void Publish(MeshVersion version)
    {
        var boundaries = version.Boundaries.Values
            .OrderBy(manifest => manifest.Pair)
            .Select(manifest => new TraceBoundary
            {
                Pair = manifest.Pair.Id,
                Version = manifest.ManifestVersion,
                Hash = manifest.ContentHash
            })
            .ToList();
        var primary = boundaries.FirstOrDefault();
        _records.Add(new TraceRecord
        {
            Seq = ++_sequence,
            Event = "publish",
            Version = version.Version,
            Parent = version.ParentVersion,
            Editor = version.Editor,
            Operation = version.Edit.Kind.ToString(),
            Blocks = version.Edit.AffectedBlocks.Select(chunk => chunk.ToString()).Order().ToArray(),
            BoundaryManifestVersion = version.Edit.BoundaryManifestVersion ?? primary?.Version,
            BoundaryContentHash = version.Edit.BoundaryContentHash ?? primary?.Hash,
            Boundaries = boundaries,
            ContentHash = version.ContentHash,
            CommitWatermark = version.Edit.CommitWatermark
        });
    }

    public void Reject(string phase, MeshError error)
    {
        _records.Add(new TraceRecord
        {
            Seq = ++_sequence,
            Event = "rejected",
            Operation = phase,
            ErrorCode = error.BusinessCode,
            Field = error.Field,
            Chunk = error.Chunk,
            ChunkPair = error.ChunkPair,
            ExpectedManifestVersion = error.ManifestVersion,
            LoopId = error.LoopId,
            EdgeId = error.EdgeId,
            Detail = error.Message
        });
    }

    public void Note(string phase, long? version = null, string? detail = null)
    {
        _records.Add(new TraceRecord
        {
            Seq = ++_sequence,
            Event = "check",
            Version = version,
            Operation = phase,
            Detail = detail
        });
    }

    public string ToJsonLines()
    {
        var builder = new StringBuilder();
        foreach (var record in _records)
        {
            builder.Append(JsonSerializer.Serialize(record, JsonOptions));
            builder.Append('\n');
        }

        return builder.ToString();
    }

    public IReadOnlyList<TraceRecord> Records => _records;

    public static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        WriteIndented = false,
        DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull
    };
}
