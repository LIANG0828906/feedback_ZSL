using System.Text.Json;
using SaveKernel.Migration;
using SaveKernel.Model;

namespace SaveKernel.Checkpoint;

public sealed record WindowSnapshot(
    int Index,
    int Start,
    int End,
    List<ItemEntry> Items,
    List<ConversionTrace> Traces,
    string WindowHash);

public sealed record CheckpointManifest(
    string InputHash,
    string RuleVersion,
    int SourceVersion,
    int ChunkSize,
    int TotalWindows,
    int CommittedWindows,
    bool Completed,
    string? FinalContentHash);

/// <summary>
/// 检查点存储：每个提交窗口原子（临时文件+改名）且串行写入本地目录。
/// 重启加载时校验输入哈希与迁移规则版本，不匹配则整体作废。
/// </summary>
public sealed class CheckpointStore
{
    private static readonly JsonSerializerOptions JsonOpts = new() { WriteIndented = false };
    private readonly object _writeLock = new();
    private int _nextCommitIndex;

    public string RootDir { get; }

    public CheckpointStore(string rootDir)
    {
        RootDir = rootDir;
        Directory.CreateDirectory(WindowsDir);
    }

    private string WindowsDir => Path.Combine(RootDir, "windows");
    private string ManifestPath => Path.Combine(RootDir, "manifest.json");
    private string WindowPath(int index) => Path.Combine(WindowsDir, $"w{index:D6}.json");

    public void WriteManifest(CheckpointManifest manifest)
    {
        lock (_writeLock)
            AtomicWrite(ManifestPath, JsonSerializer.Serialize(manifest, JsonOpts));
    }

    public CheckpointManifest? ReadManifest()
    {
        if (!File.Exists(ManifestPath)) return null;
        try
        {
            return JsonSerializer.Deserialize<CheckpointManifest>(File.ReadAllText(ManifestPath), JsonOpts);
        }
        catch (JsonException)
        {
            return null;
        }
    }

    /// <summary>串行提交窗口：强制按窗口下标顺序写入，写入加锁不交错。</summary>
    public void CommitWindow(WindowSnapshot window)
    {
        lock (_writeLock)
        {
            if (window.Index != _nextCommitIndex)
                throw new SaveKernelException(
                    ErrorCode.E_CHECKPOINT,
                    $"窗口提交乱序: 期望 {_nextCommitIndex}，实际 {window.Index}",
                    windowIndex: window.Index);
            AtomicWrite(WindowPath(window.Index), JsonSerializer.Serialize(window, JsonOpts));
            _nextCommitIndex++;
        }
    }

    public WindowSnapshot? ReadWindow(int index)
    {
        var path = WindowPath(index);
        if (!File.Exists(path)) return null;
        try
        {
            return JsonSerializer.Deserialize<WindowSnapshot>(File.ReadAllText(path), JsonOpts);
        }
        catch (JsonException)
        {
            return null;
        }
    }

    /// <summary>加载已提交窗口；任一窗口缺失或损坏即整体无效。</summary>
    public List<WindowSnapshot>? LoadCommittedWindows(CheckpointManifest manifest)
    {
        var windows = new List<WindowSnapshot>();
        for (var i = 0; i < manifest.CommittedWindows; i++)
        {
            var w = ReadWindow(i);
            if (w is null || w.Index != i) return null;
            windows.Add(w);
        }
        return windows;
    }

    /// <summary>作废全部快照（整体删除，禁止部分混用）。</summary>
    public void Invalidate()
    {
        lock (_writeLock)
        {
            if (Directory.Exists(RootDir))
                Directory.Delete(RootDir, recursive: true);
            Directory.CreateDirectory(WindowsDir);
            _nextCommitIndex = 0;
        }
    }

    public void ResetCommitCursor(int committed) => _nextCommitIndex = committed;

    private static void AtomicWrite(string path, string content)
    {
        var tmp = path + ".tmp";
        File.WriteAllText(tmp, content);
        File.Move(tmp, path, overwrite: true);
    }
}
