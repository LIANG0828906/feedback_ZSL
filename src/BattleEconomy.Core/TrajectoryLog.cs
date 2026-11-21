using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;

namespace BattleEconomy.Core;

public sealed class TrajectoryLog : IDisposable
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        WriteIndented = false,
        Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping
    };

    private readonly object _sync = new();
    private readonly StreamWriter _writer;

    public TrajectoryLog(string path, bool append = false)
    {
        Path = System.IO.Path.GetFullPath(path);
        Directory.CreateDirectory(System.IO.Path.GetDirectoryName(Path)!);
        var stream = new FileStream(
            Path,
            append ? FileMode.Append : FileMode.Create,
            FileAccess.Write,
            FileShare.Read);
        _writer = new StreamWriter(stream, new UTF8Encoding(encoderShouldEmitUTF8Identifier: false))
        {
            NewLine = "\n",
            AutoFlush = true
        };
    }

    public string Path { get; }

    public void Write(object entry)
    {
        var json = JsonSerializer.Serialize(entry, JsonOptions);
        lock (_sync)
        {
            _writer.WriteLine(json);
        }
    }

    public void Dispose()
    {
        lock (_sync)
        {
            _writer.Dispose();
        }
    }
}
