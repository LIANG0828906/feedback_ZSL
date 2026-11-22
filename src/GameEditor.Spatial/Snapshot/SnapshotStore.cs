using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace GameEditor.Spatial;

public sealed class SnapshotPayload
{
    public string Format { get; set; } = SceneFormats.Snapshot;
    public string ContentHash { get; set; } = string.Empty;
    public string StructureHash { get; set; } = string.Empty;
    public long StructureVersion { get; set; }
    public string ParameterVersion { get; set; } = string.Empty;
    public long Revision { get; set; }
    public long TransformVersionClock { get; set; }
    public string SnapshotHash { get; set; } = string.Empty;
    public List<SnapshotNode> Nodes { get; set; } = [];
    public List<SnapshotEntry> Entries { get; set; } = [];
    public List<SnapshotIndexNode> IndexNodes { get; set; } = [];
}

public sealed class SnapshotNode
{
    public string NodeId { get; set; } = string.Empty;
    public float[] WorldMatrix { get; set; } = new float[16];
    public float[] BoundsMin { get; set; } = new float[3];
    public float[] BoundsMax { get; set; } = new float[3];
    public bool BoundsEmpty { get; set; }
    public long TransformVersion { get; set; }
    public long BoundsVersion { get; set; }
    public long CacheVersion { get; set; }
    public bool IsReady { get; set; }
    public string? Failure { get; set; }
}

public sealed class SnapshotEntry
{
    public string EntryId { get; set; } = string.Empty;
    public string GeometryId { get; set; } = string.Empty;
    public string NodeId { get; set; } = string.Empty;
    public string Kind { get; set; } = string.Empty;
    public float[] LocalBoundsMin { get; set; } = new float[3];
    public float[] LocalBoundsMax { get; set; } = new float[3];
    public bool LocalBoundsEmpty { get; set; }
    public float[] WorldBoundsMin { get; set; } = new float[3];
    public float[] WorldBoundsMax { get; set; } = new float[3];
    public bool WorldBoundsEmpty { get; set; }
    public long NodeTransformVersion { get; set; }
    public long CacheVersion { get; set; }
    public bool IsReady { get; set; }
    public string? Failure { get; set; }
}

public sealed class SnapshotIndexNode
{
    public string NodeId { get; set; } = string.Empty;
    public float[] BoundsMin { get; set; } = new float[3];
    public float[] BoundsMax { get; set; } = new float[3];
    public bool BoundsEmpty { get; set; }
    public long CacheVersion { get; set; }
    public bool IsReady { get; set; }
}

public sealed record SnapshotLoadResult(
    string Path,
    string SnapshotHash,
    long Revision,
    RuntimeSnapshot Restored);

public sealed record SnapshotOpenResult(
    SpatialRuntime Runtime,
    bool UsedSnapshot,
    DomainError? SnapshotError,
    SnapshotLoadResult? Load);

public static class SnapshotStore
{
    private static readonly SemaphoreSlim WriteGate = new(1, 1);
    private static readonly JsonSerializerOptions SerializerOptions = new(SceneJsonCodec.JsonOptions)
    {
        DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull
    };

    public static Result<SnapshotPayload> Save(
        SpatialRuntime runtime,
        string path,
        CancellationToken cancellationToken = default)
    {
        if (!runtime.IsInitialized)
        {
            return Result<SnapshotPayload>.Fail(ErrorCode.E_SNAP, "Cannot save an uninitialized runtime.", "$.snapshot");
        }

        var snapshot = runtime.CaptureSnapshot();
        var payload = ToPayload(snapshot);
        payload.SnapshotHash = ComputeSnapshotHash(payload);
        byte[] bytes;
        try
        {
            bytes = Encoding.UTF8.GetBytes(
                JsonSerializer.Serialize(payload, SerializerOptions) + "\n");
        }
        catch (Exception exception)
        {
            return Result<SnapshotPayload>.Fail(ErrorCode.E_SNAP, exception.Message, path);
        }

        var gateEntered = false;
        string? tempPath = null;
        try
        {
            WriteGate.Wait(cancellationToken);
            gateEntered = true;
            cancellationToken.ThrowIfCancellationRequested();
            var fullPath = Path.GetFullPath(path);
            var directory = Path.GetDirectoryName(fullPath)!;
            Directory.CreateDirectory(directory);
            var lockPath = fullPath + ".lock";
            using var lockStream = new FileStream(
                lockPath,
                FileMode.OpenOrCreate,
                FileAccess.ReadWrite,
                FileShare.None);
            tempPath = Path.Combine(
                directory,
                $".{Path.GetFileName(fullPath)}.{Environment.ProcessId}.{Environment.CurrentManagedThreadId}.tmp");
            using (var stream = new FileStream(
                       tempPath,
                       FileMode.Create,
                       FileAccess.Write,
                       FileShare.None,
                       4096,
                       FileOptions.WriteThrough))
            {
                stream.Write(bytes);
                stream.Flush(flushToDisk: true);
            }

            File.Move(tempPath, fullPath, overwrite: true);
            tempPath = null;
            return Result<SnapshotPayload>.Ok(payload);
        }
        catch (OperationCanceledException)
        {
            return Result<SnapshotPayload>.Fail(ErrorCode.E_CANCELED, "Snapshot save was canceled.", path);
        }
        catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
        {
            return Result<SnapshotPayload>.Fail(ErrorCode.E_SNAP, exception.Message, path);
        }
        finally
        {
            if (tempPath is not null)
            {
                try
                {
                    File.Delete(tempPath);
                }
                catch
                {
                    // A leftover temp file is never considered a valid snapshot.
                }
            }

            if (gateEntered)
            {
                WriteGate.Release();
            }
        }
    }

    public static Result<SnapshotLoadResult> LoadInto(
        SpatialRuntime runtime,
        string path,
        CancellationToken cancellationToken = default)
    {
        if (runtime.IsInitialized)
        {
            return Result<SnapshotLoadResult>.Fail(
                ErrorCode.E_SNAP,
                "Snapshot load requires an empty runtime shell.",
                path);
        }

        SnapshotPayload? payload;
        try
        {
            cancellationToken.ThrowIfCancellationRequested();
            if (!File.Exists(path))
            {
                return Result<SnapshotLoadResult>.Fail(ErrorCode.E_SNAP, "Snapshot file does not exist.", path);
            }

            using var stream = new FileStream(
                path,
                FileMode.Open,
                FileAccess.Read,
                FileShare.ReadWrite | FileShare.Delete);
            using var reader = new StreamReader(stream, Encoding.UTF8, detectEncodingFromByteOrderMarks: true);
            var json = reader.ReadToEnd();
            payload = JsonSerializer.Deserialize<SnapshotPayload>(json, SerializerOptions);
        }
        catch (OperationCanceledException)
        {
            return Result<SnapshotLoadResult>.Fail(ErrorCode.E_CANCELED, "Snapshot load was canceled.", path);
        }
        catch (Exception exception) when (exception is IOException or UnauthorizedAccessException or JsonException)
        {
            return Result<SnapshotLoadResult>.Fail(ErrorCode.E_SNAP, $"Snapshot cannot be read: {exception.Message}", path);
        }

        if (payload is null ||
            payload.Format != SceneFormats.Snapshot ||
            payload.ContentHash != runtime.ContentHash ||
            payload.StructureHash != runtime.StructureHash ||
            payload.StructureVersion != runtime.StructureVersion ||
            payload.ParameterVersion != runtime.ParameterVersion)
        {
            return Result<SnapshotLoadResult>.Fail(
                ErrorCode.E_SNAP,
                "Snapshot metadata does not match the current scene or parameter version.",
                path);
        }

        var expectedHash = ComputeSnapshotHash(payload);
        if (!string.Equals(expectedHash, payload.SnapshotHash, StringComparison.Ordinal))
        {
            return Result<SnapshotLoadResult>.Fail(ErrorCode.E_SNAP, "Snapshot hash mismatch.", path);
        }

        var conversion = FromPayload(runtime, payload);
        if (!conversion.IsSuccess)
        {
            return Result<SnapshotLoadResult>.Fail(
                ErrorCode.E_SNAP,
                conversion.Error!.Message,
                conversion.Error.Path);
        }

        var state = conversion.Value!;
        var restored = runtime.RestoreSnapshotState(
            payload,
            state.Nodes,
            state.Entries,
            state.IndexNodes,
            payload.Revision,
            payload.TransformVersionClock);
        return restored.IsSuccess
            ? Result<SnapshotLoadResult>.Ok(new SnapshotLoadResult(
                path,
                payload.SnapshotHash,
                payload.Revision,
                restored.Value!))
            : Result<SnapshotLoadResult>.Fail(ErrorCode.E_SNAP, restored.Error!.Message, restored.Error.Path);
    }

    public static Result<SnapshotPayload> SaveForScene(
        SceneDocument scene,
        SpatialRuntime runtime,
        string path,
        CancellationToken cancellationToken = default)
    {
        if (!string.Equals(scene.Declaration.ContentHash, runtime.ContentHash, StringComparison.Ordinal))
        {
            return Result<SnapshotPayload>.Fail(
                ErrorCode.E_SNAP,
                "Runtime scene content differs from the supplied scene document.",
                path);
        }

        return Save(runtime, path, cancellationToken);
    }

    public static Result<SnapshotOpenResult> LoadOrRebuild(
        SceneDocument scene,
        SpatialRuntimeOptions? options,
        string path,
        CancellationToken cancellationToken = default)
    {
        var shell = SpatialRuntime.CreateShell(scene, options);
        if (!shell.IsSuccess)
        {
            return Result<SnapshotOpenResult>.Fail(
                shell.Error!.Code,
                shell.Error.Message,
                shell.Error.Path);
        }

        var load = LoadInto(shell.Value!, path, cancellationToken);
        if (load.IsSuccess)
        {
            return Result<SnapshotOpenResult>.Ok(new SnapshotOpenResult(
                shell.Value!,
                true,
                null,
                load.Value));
        }

        if (load.Error!.Code == ErrorCode.E_CANCELED)
        {
            return Result<SnapshotOpenResult>.Fail(
                ErrorCode.E_CANCELED,
                load.Error.Message,
                load.Error.Path);
        }

        var rebuild = shell.Value!.FullRebuild(cancellationToken);
        if (!rebuild.IsSuccess)
        {
            return Result<SnapshotOpenResult>.Fail(
                rebuild.Error!.Code,
                rebuild.Error.Message,
                rebuild.Error.Path);
        }

        return Result<SnapshotOpenResult>.Ok(new SnapshotOpenResult(
            shell.Value!,
            false,
            load.Error,
            null));
    }

    private static SnapshotPayload ToPayload(RuntimeSnapshot snapshot)
    {
        return new SnapshotPayload
        {
            ContentHash = snapshot.ContentHash,
            StructureHash = snapshot.StructureHash,
            StructureVersion = snapshot.StructureVersion,
            ParameterVersion = snapshot.ParameterVersion,
            Revision = snapshot.Revision,
            TransformVersionClock = snapshot.Nodes.Count == 0
                ? 0
                : snapshot.Nodes.Max(node => node.TransformVersion),
            Nodes = snapshot.Nodes.Select(node => new SnapshotNode
            {
                NodeId = node.NodeId,
                WorldMatrix = SpatialMath.MatrixToRowMajor(node.WorldTransform),
                BoundsMin = EmptyBoundsArray(node.WorldBounds, true),
                BoundsMax = EmptyBoundsArray(node.WorldBounds, false),
                BoundsEmpty = node.WorldBounds.IsEmpty,
                TransformVersion = node.TransformVersion,
                BoundsVersion = node.BoundsVersion,
                CacheVersion = node.CacheVersion,
                IsReady = node.IsReady,
                Failure = node.Failure
            }).OrderBy(node => node.NodeId, StringComparer.Ordinal).ToList(),
            Entries = snapshot.Entries.Select(entry => new SnapshotEntry
            {
                EntryId = entry.EntryId,
                GeometryId = entry.GeometryId,
                NodeId = entry.NodeId,
                Kind = entry.Kind.ToString(),
                LocalBoundsMin = EmptyBoundsArray(entry.LocalBounds, true),
                LocalBoundsMax = EmptyBoundsArray(entry.LocalBounds, false),
                LocalBoundsEmpty = entry.LocalBounds.IsEmpty,
                WorldBoundsMin = EmptyBoundsArray(entry.WorldBounds, true),
                WorldBoundsMax = EmptyBoundsArray(entry.WorldBounds, false),
                WorldBoundsEmpty = entry.WorldBounds.IsEmpty,
                NodeTransformVersion = entry.NodeTransformVersion,
                CacheVersion = entry.CacheVersion,
                IsReady = entry.IsReady,
                Failure = entry.Failure
            }).OrderBy(entry => entry.GeometryId, StringComparer.Ordinal).ToList(),
            IndexNodes = snapshot.IndexNodes.Select(node => new SnapshotIndexNode
            {
                NodeId = node.NodeId,
                BoundsMin = EmptyBoundsArray(node.AggregateBounds, true),
                BoundsMax = EmptyBoundsArray(node.AggregateBounds, false),
                BoundsEmpty = node.AggregateBounds.IsEmpty,
                CacheVersion = node.CacheVersion,
                IsReady = node.IsReady
            }).OrderBy(node => node.NodeId, StringComparer.Ordinal).ToList()
        };
    }

    private static Result<RestoredState> FromPayload(SpatialRuntime runtime, SnapshotPayload payload)
    {
        if (payload.Nodes.Count != runtime.CaptureShellNodeCount() ||
            payload.Entries.Count != runtime.CaptureShellGeometryCount())
        {
            return Result<RestoredState>.Fail(ErrorCode.E_SNAP, "Snapshot entry counts are stale.", "$.snapshot");
        }

        var nodes = new Dictionary<string, NodeDerived>(StringComparer.Ordinal);
        foreach (var node in payload.Nodes)
        {
            if (!nodes.TryAdd(node.NodeId, new NodeDerived(
                    node.NodeId,
                    SpatialMath.MatrixFromRowMajor(node.WorldMatrix),
                    BoundsFromArrays(node.BoundsMin, node.BoundsMax, node.BoundsEmpty),
                    node.TransformVersion,
                    node.BoundsVersion,
                    node.CacheVersion,
                    node.IsReady,
                    node.Failure)))
            {
                return Result<RestoredState>.Fail(ErrorCode.E_SNAP, "Snapshot contains duplicate node ids.", "$.nodes");
            }
        }

        var entries = new Dictionary<string, GeometryEntry>(StringComparer.Ordinal);
        foreach (var entry in payload.Entries)
        {
            if (!Enum.TryParse<GeometryKind>(entry.Kind, ignoreCase: false, out var kind))
            {
                return Result<RestoredState>.Fail(ErrorCode.E_SNAP, "Snapshot entry has an unknown geometry kind.", "$.entries");
            }

            if (!entries.TryAdd(entry.GeometryId, new GeometryEntry(
                    entry.EntryId,
                    entry.GeometryId,
                    entry.NodeId,
                    kind,
                    BoundsFromArrays(entry.LocalBoundsMin, entry.LocalBoundsMax, entry.LocalBoundsEmpty),
                    BoundsFromArrays(entry.WorldBoundsMin, entry.WorldBoundsMax, entry.WorldBoundsEmpty),
                    entry.NodeTransformVersion,
                    entry.CacheVersion,
                    entry.IsReady,
                    entry.Failure)))
            {
                return Result<RestoredState>.Fail(ErrorCode.E_SNAP, "Snapshot contains duplicate geometry ids.", "$.entries");
            }
        }

        var indexNodes = new Dictionary<string, IndexNode>(StringComparer.Ordinal);
        foreach (var node in payload.IndexNodes)
        {
            if (!indexNodes.TryAdd(node.NodeId, new IndexNode(
                    node.NodeId,
                    BoundsFromArrays(node.BoundsMin, node.BoundsMax, node.BoundsEmpty),
                    node.CacheVersion,
                    node.IsReady)))
            {
                return Result<RestoredState>.Fail(ErrorCode.E_SNAP, "Snapshot contains duplicate index node ids.", "$.indexNodes");
            }
        }

        return Result<RestoredState>.Ok(new RestoredState(nodes, entries, indexNodes));
    }

    private static Bounds3 BoundsFromArrays(IReadOnlyList<float> min, IReadOnlyList<float> max, bool empty)
    {
        if (empty)
        {
            return Bounds3.Empty;
        }

        if (min.Count != 3 || max.Count != 3)
        {
            return Bounds3.Empty;
        }

        var minVector = SpatialMath.VectorFromArray(min);
        var maxVector = SpatialMath.VectorFromArray(max);
        return minVector.X > maxVector.X ? Bounds3.Empty : new Bounds3(minVector, maxVector);
    }

    private static float[] EmptyBoundsArray(Bounds3 bounds, bool min)
    {
        if (bounds.IsEmpty)
        {
            return [0, 0, 0];
        }

        return min ? bounds.ToMinArray() : bounds.ToMaxArray();
    }

    private static string ComputeSnapshotHash(SnapshotPayload payload)
    {
        var clone = new SnapshotPayload
        {
            Format = payload.Format,
            ContentHash = payload.ContentHash,
            StructureHash = payload.StructureHash,
            StructureVersion = payload.StructureVersion,
            ParameterVersion = payload.ParameterVersion,
            Revision = payload.Revision,
            TransformVersionClock = payload.TransformVersionClock,
            SnapshotHash = string.Empty,
            Nodes = payload.Nodes,
            Entries = payload.Entries,
            IndexNodes = payload.IndexNodes
        };
        var json = JsonSerializer.Serialize(clone, SerializerOptions);
        return Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(json))).ToLowerInvariant();
    }

    private sealed record RestoredState(
        Dictionary<string, NodeDerived> Nodes,
        Dictionary<string, GeometryEntry> Entries,
        Dictionary<string, IndexNode> IndexNodes);
}
