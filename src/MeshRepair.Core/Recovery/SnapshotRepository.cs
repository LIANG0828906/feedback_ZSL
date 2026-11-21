using System.Globalization;
using System.Text.Json;
using MeshRepair.Core.Chunks;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Core.Recovery;

public sealed record RecoveryResult(
    long HeadVersion,
    IReadOnlyList<MeshVersion> Versions,
    ChunkLayout Layout,
    BaselineInfo Baseline,
    IReadOnlyList<long> SkippedSnapshots,
    IReadOnlyList<ChunkId> RecomputedBlocks,
    IReadOnlyList<ChunkId> VerifiedBlocks);

public sealed class SnapshotRepository : IVersionSnapshotSink
{
    private readonly object _gate = new();
    private readonly string _directory;
    private readonly ChunkLayout _layout;
    private readonly BaselineInfo _baseline;

    public SnapshotRepository(string directory, ChunkLayout layout, BaselineInfo baseline)
    {
        _directory = directory;
        _layout = layout;
        _baseline = baseline;
    }

    public string DirectoryPath => _directory;

    public static MeshResult<SnapshotRepository> LoadFromDirectory(string directory)
    {
        if (!System.IO.Directory.Exists(directory))
        {
            return MeshResult<SnapshotRepository>.Fail(
                MeshErrorCode.E_RECOVERY_INVALID,
                "snapshot directory does not exist",
                field: "SnapshotDirectory");
        }

        var layout = new ChunkLayout(1, 1, 1, new Bounds3(Point3.Origin, Point3.Origin));
        var baseline = new BaselineInfo(0, 0, string.Empty, new Dictionary<ulong, Point3>());
        return MeshResult<SnapshotRepository>.Ok(new SnapshotRepository(directory, layout, baseline));
    }

    public MeshResult Save(MeshVersion version)
    {
        lock (_gate)
        {
            try
            {
                System.IO.Directory.CreateDirectory(_directory);
                string? previousHash = null;
                if (version.Version > 1)
                {
                    var previousPath = PathFor(version.Version - 1);
                    if (!File.Exists(previousPath))
                    {
                        return MeshResult.Fail(
                            MeshErrorCode.E_RECOVERY_INVALID,
                            $"previous snapshot version {version.Version - 1} is missing",
                            field: "PreviousSnapshotHash",
                            snapshot: version.Version.ToString(CultureInfo.InvariantCulture));
                    }

                    var previous = ReadDocument(previousPath);
                    var previousValue = previous.IsSuccess ? previous.RequireValue() : null;
                    if (previousValue is null
                        || previousValue.Version != version.Version - 1
                        || previousValue.ComputeSelfHash() != previousValue.SelfHash
                        || !previousValue.ToVersion().IsSuccess)
                    {
                        return MeshResult.Fail(
                            MeshErrorCode.E_RECOVERY_INVALID,
                            $"previous snapshot version {version.Version - 1} is not valid",
                            field: "PreviousSnapshotHash",
                            snapshot: (version.Version - 1).ToString(CultureInfo.InvariantCulture));
                    }

                    previousHash = previous.RequireValue().SelfHash;
                }

                var document = SnapshotDocument.FromVersion(version, _layout, _baseline, previousHash);
                document.SelfHash = document.ComputeSelfHash();
                var json = JsonSerializer.Serialize(document, SnapshotJson.Options);
                var finalPath = PathFor(version.Version);
                var temporaryPath = finalPath + ".tmp";
                using (var stream = new FileStream(
                           temporaryPath,
                           FileMode.Create,
                           FileAccess.Write,
                           FileShare.None,
                           4096,
                           FileOptions.WriteThrough))
                {
                    var bytes = System.Text.Encoding.UTF8.GetBytes(json);
                    stream.Write(bytes);
                    stream.Flush(flushToDisk: true);
                }

                File.Move(temporaryPath, finalPath, overwrite: true);
                return MeshResult.Ok();
            }
            catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_IO,
                    $"failed to save snapshot: {exception.Message}",
                    field: "Snapshot",
                    snapshot: version.Version.ToString(CultureInfo.InvariantCulture));
            }
        }
    }

    public MeshResult<RecoveryResult> LoadLatest()
    {
        lock (_gate)
        {
            if (!System.IO.Directory.Exists(_directory))
            {
                return MeshResult<RecoveryResult>.Fail(
                    MeshErrorCode.E_RECOVERY_INVALID,
                    "snapshot directory does not exist",
                    field: "SnapshotDirectory");
            }

            var files = System.IO.Directory
                .EnumerateFiles(_directory, "snapshot-*.json", SearchOption.TopDirectoryOnly)
                .Select(path => (Path: path, Version: ParseVersion(path)))
                .Where(entry => entry.Version > 0)
                .OrderBy(entry => entry.Version)
                .ToArray();
            if (files.Length == 0)
            {
                return MeshResult<RecoveryResult>.Fail(
                    MeshErrorCode.E_RECOVERY_INVALID,
                    "no committed snapshots were found",
                    field: "SnapshotDirectory");
            }

            var candidates = new Dictionary<long, (SnapshotDocument Document, MeshVersion Version, ChunkLayout Layout, BaselineInfo Baseline)>();
            MeshError? firstFailure = null;
            var failures = new Dictionary<long, MeshError>();
            foreach (var file in files)
            {
                var parsed = TryParseDocument(file.Path);
                if (!parsed.IsSuccess)
                {
                    failures[file.Version] = parsed.Error!;
                    firstFailure ??= parsed.Error;
                    continue;
                }

                var document = parsed.RequireValue();
                if (document.ComputeSelfHash() != document.SelfHash)
                {
                    var error = new MeshError(
                        MeshErrorCode.E_RECOVERY_INVALID,
                        "snapshot self hash does not match",
                        Field: "SelfHash",
                        Snapshot: file.Version.ToString(CultureInfo.InvariantCulture));
                    failures[file.Version] = error;
                    firstFailure ??= error;
                    continue;
                }

                var converted = document.ToVersion();
                if (!converted.IsSuccess)
                {
                    failures[file.Version] = converted.Error!;
                    firstFailure ??= converted.Error;
                    continue;
                }

                var value = converted.RequireValue();
                candidates[file.Version] = (document, value.Version, value.Layout, value.Baseline);
            }

            var maxVersion = files.Max(file => file.Version);
            for (var start = maxVersion; start >= 1; start--)
            {
                if (!candidates.ContainsKey(start))
                {
                    continue;
                }

                var chain = new List<(SnapshotDocument Document, MeshVersion Version, ChunkLayout Layout, BaselineInfo Baseline)>();
                var chainValid = true;
                for (var versionId = 1; versionId <= start; versionId++)
                {
                    if (!candidates.TryGetValue(versionId, out var candidate))
                    {
                        chainValid = false;
                        break;
                    }

                    if (versionId > 1)
                    {
                        var previous = candidates[versionId - 1].Document;
                        if (candidate.Document.PreviousSnapshotHash != previous.SelfHash)
                        {
                            failures.TryAdd(
                                versionId,
                                new MeshError(
                                    MeshErrorCode.E_RECOVERY_INVALID,
                                    $"snapshot chain is broken at version {versionId}",
                                    Field: "PreviousSnapshotHash",
                                    Snapshot: versionId.ToString(CultureInfo.InvariantCulture)));
                            chainValid = false;
                            break;
                        }
                    }

                    if (candidate.Layout.DivisionsX != candidates[start].Layout.DivisionsX
                        || candidate.Layout.DivisionsY != candidates[start].Layout.DivisionsY
                        || candidate.Layout.DivisionsZ != candidates[start].Layout.DivisionsZ
                        || candidate.Layout.Bounds.Min != candidates[start].Layout.Bounds.Min
                        || candidate.Layout.Bounds.Max != candidates[start].Layout.Bounds.Max
                        || candidate.Layout.InterfaceTolerance != candidates[start].Layout.InterfaceTolerance
                        || candidate.Baseline.ContentHash != candidates[start].Baseline.ContentHash)
                    {
                        chainValid = false;
                        break;
                    }

                    chain.Add(candidate);
                }

                if (!chainValid)
                {
                    continue;
                }

                var skipped = files
                    .Select(file => file.Version)
                    .Where(versionId => versionId > start)
                    .Order()
                    .ToArray();
                var verified = chain
                    .SelectMany(candidate => candidate.Version.Chunks.Keys)
                    .Distinct()
                    .Order()
                    .ToArray();
                return MeshResult<RecoveryResult>.Ok(
                    new RecoveryResult(
                        start,
                        chain.Select(candidate => candidate.Version).OrderBy(version => version.Version).ToArray(),
                        chain[^1].Layout,
                        chain[^1].Baseline,
                        skipped,
                        Array.Empty<ChunkId>(),
                        verified));
            }

            return MeshResult<RecoveryResult>.Fail(
                firstFailure ?? new MeshError(
                    MeshErrorCode.E_RECOVERY_INVALID,
                    "no complete and valid snapshot chain was found",
                    Field: "PreviousSnapshotHash"));
        }
    }

    private static MeshResult<SnapshotDocument> TryParseDocument(string path)
    {
        try
        {
            var json = File.ReadAllText(path);
            var document = JsonSerializer.Deserialize<SnapshotDocument>(json, SnapshotJson.Options);
            return document is null
                ? MeshResult<SnapshotDocument>.Fail(
                    MeshErrorCode.E_RECOVERY_INVALID,
                    "snapshot document is empty",
                    field: "Document")
                : MeshResult<SnapshotDocument>.Ok(document);
        }
        catch (Exception exception) when (exception is IOException or UnauthorizedAccessException or JsonException)
        {
            return MeshResult<SnapshotDocument>.Fail(
                MeshErrorCode.E_RECOVERY_INVALID,
                $"snapshot cannot be parsed: {exception.Message}",
                field: "Document");
        }
    }

    private static MeshResult<SnapshotDocument> ReadDocument(string path)
        => TryParseDocument(path);

    private string PathFor(long version)
        => Path.Combine(_directory, $"snapshot-{version.ToString("D10", CultureInfo.InvariantCulture)}.json");

    private static long ParseVersion(string path)
    {
        var name = Path.GetFileNameWithoutExtension(path);
        const string prefix = "snapshot-";
        if (!name.StartsWith(prefix, StringComparison.Ordinal))
        {
            return 0;
        }

        return long.TryParse(name[prefix.Length..], NumberStyles.None, CultureInfo.InvariantCulture, out var version)
            ? version
            : 0;
    }
}
