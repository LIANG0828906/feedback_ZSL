using System.Collections.Immutable;
using System.Text.Json;

namespace BattleEconomy.Core;

public sealed class EconomySnapshotStore
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        WriteIndented = true
    };

    public EconomySnapshotStore(string rootDirectory)
    {
        RootDirectory = Path.GetFullPath(rootDirectory);
        SnapshotPath = Path.Combine(RootDirectory, "economy-snapshot.json");
        PendingBatchPath = Path.Combine(RootDirectory, "pending-batch.json");
    }

    public string RootDirectory { get; }
    public string SnapshotPath { get; }
    public string PendingBatchPath { get; }

    public void SaveSnapshot(EconomySnapshot snapshot)
    {
        Directory.CreateDirectory(RootDirectory);
        var dto = SnapshotFileDto.FromSnapshot(snapshot);
        AtomicWrite(SnapshotPath, JsonSerializer.SerializeToUtf8Bytes(dto, JsonOptions));
    }

    public void SavePendingBatch(RecoveryPendingBatch pending)
    {
        Directory.CreateDirectory(RootDirectory);
        var dto = PendingFileDto.FromModel(pending);
        AtomicWrite(PendingBatchPath, JsonSerializer.SerializeToUtf8Bytes(dto, JsonOptions));
    }

    public void DeletePendingBatch()
    {
        try
        {
            if (File.Exists(PendingBatchPath))
            {
                File.Delete(PendingBatchPath);
            }
        }
        catch (IOException)
        {
            // A stale intent file is harmless: recovery detects the committed watermark.
        }
        catch (UnauthorizedAccessException)
        {
            // The committed snapshot remains authoritative.
        }
    }

    public SnapshotLoadResult Load(TerrainMap map)
    {
        if (!File.Exists(SnapshotPath))
        {
            return new SnapshotLoadResult(true, null, null, null);
        }

        SnapshotFileDto? file;
        try
        {
            file = JsonSerializer.Deserialize<SnapshotFileDto>(
                File.ReadAllBytes(SnapshotPath),
                JsonOptions);
        }
        catch (Exception exception) when (exception is JsonException or IOException or UnauthorizedAccessException)
        {
            return new SnapshotLoadResult(
                false,
                new OperationError(
                    ErrorCodes.RecoveryInvalid,
                    $"snapshot cannot be read: {exception.GetType().Name}",
                    FirstBrokenBatch: 0),
                null,
                null);
        }

        if (file is null || file.SchemaVersion != 1 || file.Current is null || file.HashChain is null)
        {
            return new SnapshotLoadResult(
                false,
                new OperationError(
                    ErrorCodes.RecoveryInvalid,
                    "snapshot schema or required fields are missing",
                    FirstBrokenBatch: file?.Current?.BatchSequence is { } batch ? checked((int)batch) : 0),
                null,
                null);
        }

        if (file.HashChain.Count == 0)
        {
            return RecoveryFailure("hash chain is missing", 0);
        }

        var previousChainHash = StableHash.Genesis;
        long? previousVersion = null;
        long? previousBatch = null;
        ChainEntryDto? previousEntry = null;
        for (var chainIndex = 0; chainIndex < file.HashChain.Count; chainIndex++)
        {
            var entry = file.HashChain[chainIndex];
            var firstBroken = checked((int)Math.Clamp(entry.BatchSequence, 0, int.MaxValue));
            if (entry.Version < 0 || entry.BatchSequence < 0 || entry.CommitWatermark < 0)
            {
                return RecoveryFailure("hash chain contains a negative sequence", firstBroken);
            }

            if (chainIndex == 0 && (entry.Version != 0 || entry.BatchSequence != 0))
            {
                return RecoveryFailure("hash chain does not start at the genesis batch", firstBroken);
            }

            if (previousVersion is not null &&
                (entry.Version != previousVersion.Value + 1 || entry.BatchSequence != previousBatch!.Value + 1))
            {
                return RecoveryFailure("hash chain sequence is not contiguous", firstBroken);
            }

            if (entry.CommitWatermark != entry.BatchSequence)
            {
                return RecoveryFailure("hash chain commit watermark is inconsistent", firstBroken);
            }

            if (!IsSha256(entry.ContentHash) ||
                (!IsSha256(entry.PreviousChainHash) && entry.PreviousChainHash != StableHash.Genesis) ||
                !IsSha256(entry.ChainHash))
            {
                return RecoveryFailure("hash chain field is missing", firstBroken);
            }

            if (!string.Equals(entry.PreviousChainHash, previousChainHash, StringComparison.Ordinal))
            {
                return RecoveryFailure("hash chain previous link does not match", firstBroken);
            }

            var computedChainHash = StableHash.ChainEntry(
                entry.Version,
                entry.BatchSequence,
                entry.CommitWatermark,
                entry.ContentHash,
                entry.PreviousChainHash);
            if (!string.Equals(computedChainHash, entry.ChainHash, StringComparison.Ordinal))
            {
                return RecoveryFailure("hash chain digest does not match", firstBroken);
            }

            previousChainHash = entry.ChainHash;
            previousVersion = entry.Version;
            previousBatch = entry.BatchSequence;
            previousEntry = entry;
        }

        var current = file.Current;
        var currentBatch = checked((int)Math.Clamp(current.BatchSequence, 0, int.MaxValue));
        if (previousEntry is null ||
            current.Version != previousEntry.Version ||
            current.BatchSequence != previousEntry.BatchSequence ||
            current.CommitWatermark != previousEntry.CommitWatermark ||
            !string.Equals(current.ContentHash, previousEntry.ContentHash, StringComparison.Ordinal))
        {
            return RecoveryFailure("current snapshot does not match the hash chain head", currentBatch);
        }

        var expectedPreviousContentHash = file.HashChain.Count > 1
            ? file.HashChain[^2].ContentHash
            : StableHash.Genesis;
        if (!string.Equals(
                current.PreviousContentHash,
                expectedPreviousContentHash,
                StringComparison.Ordinal))
        {
            return RecoveryFailure("previous content hash link is broken", currentBatch);
        }

        if (current.CommitWatermark != current.BatchSequence)
        {
            return RecoveryFailure("commit watermark is inconsistent", currentBatch);
        }

        if (current.Contributions is null ||
            current.DestroyedTerrainIds is null ||
            current.CommittedDestructionKeys is null ||
            current.Factions is null)
        {
            return RecoveryFailure("snapshot state field is missing", currentBatch);
        }

        var contributionMap = new Dictionary<int, TerrainContribution>();
        foreach (var dto in current.Contributions)
        {
            if (dto is null || !map.TryGetNode(dto.TerrainId, out var node))
            {
                return RecoveryFailure("snapshot contains an unknown terrain contribution", currentBatch);
            }

            var contribution = dto.ToModel(node);
            if (!string.Equals(StableHash.Contribution(contribution), dto.ContentHash, StringComparison.Ordinal))
            {
                return RecoveryFailure($"terrain {dto.TerrainId} contribution hash does not match", currentBatch);
            }

            if (!contributionMap.TryAdd(contribution.TerrainId, contribution))
            {
                return RecoveryFailure("snapshot contains duplicate terrain contributions", currentBatch);
            }
        }

        if (contributionMap.Count != map.Nodes.Length)
        {
            return RecoveryFailure("snapshot contribution set is incomplete", currentBatch);
        }

        var destroyed = current.DestroyedTerrainIds.ToImmutableSortedSet();
        foreach (var terrainId in destroyed)
        {
            if (!map.TryGetNode(terrainId, out _) ||
                !contributionMap.TryGetValue(terrainId, out var contribution) ||
                !contribution.IsDestroyed)
            {
                return RecoveryFailure("destroyed terrain state is inconsistent", currentBatch);
            }
        }

        foreach (var contribution in contributionMap.Values)
        {
            if (contribution.IsDestroyed && !destroyed.Contains(contribution.TerrainId))
            {
                return RecoveryFailure("destroyed terrain is absent from the destroyed set", currentBatch);
            }
        }

        var factions = current.Factions.Select(dto => dto.ToModel()).ToImmutableArray();
        if (factions.Length != Enum.GetValues<Faction>().Length ||
            factions.Select(item => item.Faction).Distinct().Count() != factions.Length)
        {
            return RecoveryFailure("faction economy state is incomplete", currentBatch);
        }

        var committedKeys = current.CommittedDestructionKeys.ToImmutableHashSet(StringComparer.Ordinal);
        var recomputedContentHash = StableHash.SnapshotContent(
            current.Version,
            current.BatchSequence,
            current.EconomicStep,
            current.CommitWatermark,
            current.PreviousContentHash,
            contributionMap.Values,
            destroyed,
            committedKeys,
            factions);
        if (!string.Equals(recomputedContentHash, current.ContentHash, StringComparison.Ordinal))
        {
            return RecoveryFailure("snapshot content hash does not match", currentBatch);
        }

        var snapshot = new EconomySnapshot
        {
            Version = current.Version,
            BatchSequence = current.BatchSequence,
            EconomicStep = current.EconomicStep,
            CommitWatermark = current.CommitWatermark,
            PreviousContentHash = current.PreviousContentHash,
            ContentHash = current.ContentHash,
            Contributions = contributionMap.ToImmutableDictionary(),
            DestroyedTerrainIds = destroyed,
            CommittedDestructionKeys = committedKeys,
            Factions = factions,
            HashChain = file.HashChain.Select(entry => entry.ToModel()).ToImmutableArray()
        };

        RecoveryPendingBatch? pending = null;
        if (File.Exists(PendingBatchPath))
        {
            PendingFileDto? pendingFile;
            try
            {
                pendingFile = JsonSerializer.Deserialize<PendingFileDto>(
                    File.ReadAllBytes(PendingBatchPath),
                    JsonOptions);
            }
            catch (Exception exception) when (exception is JsonException or IOException or UnauthorizedAccessException)
            {
                return new SnapshotLoadResult(
                    false,
                    new OperationError(
                        ErrorCodes.RecoveryInvalid,
                        $"pending batch cannot be read: {exception.GetType().Name}",
                        FirstBrokenBatch: currentBatch),
                    null,
                    null);
            }

            if (pendingFile is null ||
                pendingFile.BatchNumber <= 0 ||
                pendingFile.EconomicSteps <= 0 ||
                pendingFile.Destructions is null ||
                pendingFile.Destructions.Count == 0 ||
                !IsSha256(pendingFile.ExpectedSnapshotHash))
            {
                return new SnapshotLoadResult(
                    false,
                    new OperationError(
                        ErrorCodes.RecoveryInvalid,
                        "pending batch fields are incomplete",
                        FirstBrokenBatch: checked((int)Math.Clamp(pendingFile?.BatchNumber ?? currentBatch, 0, int.MaxValue))),
                    null,
                    null);
            }

            var pendingDestructions = pendingFile.Destructions
                .Select(item => new DestructionEvent(item.Key, item.TerrainId))
                .ToImmutableArray();
            if (pendingDestructions.Any(item => string.IsNullOrWhiteSpace(item.Key)) ||
                pendingDestructions.Select(item => item.Key).Distinct(StringComparer.Ordinal).Count() != pendingDestructions.Length ||
                pendingDestructions.Any(item => !map.TryGetNode(item.TerrainId, out _)))
            {
                return new SnapshotLoadResult(
                    false,
                    new OperationError(
                        ErrorCodes.RecoveryInvalid,
                        "pending batch contains duplicate, empty, or unknown destruction references",
                        FirstBrokenBatch: checked((int)Math.Clamp(pendingFile.BatchNumber, 0, int.MaxValue))),
                    null,
                    null);
            }

            pending = new RecoveryPendingBatch(
                pendingFile.BatchNumber,
                pendingFile.EconomicSteps,
                pendingDestructions,
                pendingFile.ExpectedSnapshotHash);
        }

        return new SnapshotLoadResult(true, null, snapshot, pending);
    }

    private static SnapshotLoadResult RecoveryFailure(string message, int firstBrokenBatch)
    {
        return new SnapshotLoadResult(
            false,
            new OperationError(
                ErrorCodes.RecoveryInvalid,
                message,
                FirstBrokenBatch: firstBrokenBatch),
            null,
            null);
    }

    private static bool IsSha256(string? value)
    {
        return value is { Length: 64 } && value.All(character =>
            character is >= '0' and <= '9' or >= 'a' and <= 'f');
    }

    private static void AtomicWrite(string targetPath, ReadOnlySpan<byte> payload)
    {
        var directory = Path.GetDirectoryName(targetPath)!;
        var temporaryPath = Path.Combine(
            directory,
            $".{Path.GetFileName(targetPath)}.{Environment.ProcessId}.{Guid.NewGuid():N}.tmp");
        try
        {
            using (var stream = new FileStream(
                       temporaryPath,
                       FileMode.CreateNew,
                       FileAccess.Write,
                       FileShare.None,
                       bufferSize: 16 * 1024,
                       FileOptions.WriteThrough))
            {
                stream.Write(payload);
                stream.Flush(flushToDisk: true);
            }

            File.Move(temporaryPath, targetPath, overwrite: true);
        }
        finally
        {
            if (File.Exists(temporaryPath))
            {
                try
                {
                    File.Delete(temporaryPath);
                }
                catch (IOException)
                {
                    // Temporary orphan is ignored by recovery.
                }
            }
        }
    }

    private sealed class SnapshotFileDto
    {
        public int SchemaVersion { get; set; }
        public SnapshotStateDto? Current { get; set; }
        public List<ChainEntryDto>? HashChain { get; set; }

        public static SnapshotFileDto FromSnapshot(EconomySnapshot snapshot)
        {
            return new SnapshotFileDto
            {
                SchemaVersion = 1,
                Current = SnapshotStateDto.FromModel(snapshot),
                HashChain = snapshot.HashChain.Select(ChainEntryDto.FromModel).ToList()
            };
        }
    }

    private sealed class SnapshotStateDto
    {
        public long Version { get; set; }
        public long BatchSequence { get; set; }
        public long EconomicStep { get; set; }
        public long CommitWatermark { get; set; }
        public string PreviousContentHash { get; set; } = string.Empty;
        public string ContentHash { get; set; } = string.Empty;
        public List<ContributionDto>? Contributions { get; set; }
        public List<int>? DestroyedTerrainIds { get; set; }
        public List<string>? CommittedDestructionKeys { get; set; }
        public List<FactionDto>? Factions { get; set; }

        public static SnapshotStateDto FromModel(EconomySnapshot snapshot)
        {
            return new SnapshotStateDto
            {
                Version = snapshot.Version,
                BatchSequence = snapshot.BatchSequence,
                EconomicStep = snapshot.EconomicStep,
                CommitWatermark = snapshot.CommitWatermark,
                PreviousContentHash = snapshot.PreviousContentHash,
                ContentHash = snapshot.ContentHash,
                Contributions = snapshot.Contributions.Values
                    .OrderBy(item => item.TerrainId)
                    .Select(ContributionDto.FromModel)
                    .ToList(),
                DestroyedTerrainIds = snapshot.DestroyedTerrainIds.Order().ToList(),
                CommittedDestructionKeys = snapshot.CommittedDestructionKeys
                    .Order(StringComparer.Ordinal)
                    .ToList(),
                Factions = snapshot.Factions.OrderBy(item => item.Faction).Select(FactionDto.FromModel).ToList()
            };
        }
    }

    private sealed class ContributionDto
    {
        public int TerrainId { get; set; }
        public bool IsDestroyed { get; set; }
        public bool IsHarvestable { get; set; }
        public string NominalOutpostId { get; set; } = string.Empty;
        public int? SuppliedFaction { get; set; }
        public string? SuppliedOutpostId { get; set; }
        public decimal YieldPerStep { get; set; }
        public List<int>? CollectionPath { get; set; }
        public string ContentHash { get; set; } = string.Empty;

        public static ContributionDto FromModel(TerrainContribution contribution)
        {
            return new ContributionDto
            {
                TerrainId = contribution.TerrainId,
                IsDestroyed = contribution.IsDestroyed,
                IsHarvestable = contribution.IsHarvestable,
                NominalOutpostId = contribution.NominalOutpostId,
                SuppliedFaction = contribution.SuppliedFaction is { } faction ? (int)faction : null,
                SuppliedOutpostId = contribution.SuppliedOutpostId,
                YieldPerStep = contribution.YieldPerStep,
                CollectionPath = contribution.CollectionPath.ToList(),
                ContentHash = contribution.ContentHash
            };
        }

        public TerrainContribution ToModel(TerrainNode node)
        {
            return new TerrainContribution
            {
                TerrainId = TerrainId,
                IsDestroyed = IsDestroyed,
                IsHarvestable = IsHarvestable,
                NominalOutpostId = NominalOutpostId,
                SuppliedFaction = SuppliedFaction is { } faction ? (Faction)faction : null,
                SuppliedOutpostId = SuppliedOutpostId,
                YieldPerStep = YieldPerStep,
                CollectionPath = (CollectionPath ?? []).ToImmutableArray(),
                ContentHash = ContentHash
            };
        }
    }

    private sealed class FactionDto
    {
        public int Faction { get; set; }
        public decimal YieldPerStep { get; set; }
        public decimal TotalResources { get; set; }

        public FactionEconomy ToModel() => new((Faction)Faction, YieldPerStep, TotalResources);

        public static FactionDto FromModel(FactionEconomy economy)
        {
            return new FactionDto
            {
                Faction = (int)economy.Faction,
                YieldPerStep = economy.YieldPerStep,
                TotalResources = economy.TotalResources
            };
        }
    }

    private sealed class ChainEntryDto
    {
        public long Version { get; set; }
        public long BatchSequence { get; set; }
        public long CommitWatermark { get; set; }
        public string ContentHash { get; set; } = string.Empty;
        public string PreviousChainHash { get; set; } = string.Empty;
        public string ChainHash { get; set; } = string.Empty;

        public static ChainEntryDto FromModel(SnapshotChainEntry entry)
        {
            return new ChainEntryDto
            {
                Version = entry.Version,
                BatchSequence = entry.BatchSequence,
                CommitWatermark = entry.CommitWatermark,
                ContentHash = entry.ContentHash,
                PreviousChainHash = entry.PreviousChainHash,
                ChainHash = entry.ChainHash
            };
        }

        public SnapshotChainEntry ToModel()
        {
            return new SnapshotChainEntry(
                Version,
                BatchSequence,
                CommitWatermark,
                ContentHash,
                PreviousChainHash,
                ChainHash);
        }
    }

    private sealed class PendingFileDto
    {
        public long BatchNumber { get; set; }
        public int EconomicSteps { get; set; }
        public string ExpectedSnapshotHash { get; set; } = string.Empty;
        public List<DestructionDto>? Destructions { get; set; }

        public static PendingFileDto FromModel(RecoveryPendingBatch pending)
        {
            return new PendingFileDto
            {
                BatchNumber = pending.BatchNumber,
                EconomicSteps = pending.EconomicSteps,
                ExpectedSnapshotHash = pending.ExpectedSnapshotHash,
                Destructions = pending.Destructions
                    .Select(item => new DestructionDto { Key = item.Key, TerrainId = item.TerrainId })
                    .ToList()
            };
        }
    }

    private sealed class DestructionDto
    {
        public string Key { get; set; } = string.Empty;
        public int TerrainId { get; set; }
    }
}

public sealed record SnapshotLoadResult(
    bool Success,
    OperationError? Error,
    EconomySnapshot? Snapshot,
    RecoveryPendingBatch? PendingBatch);
