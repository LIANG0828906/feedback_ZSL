using System.Globalization;
using System.Security.Cryptography;
using System.Text;

namespace MeshRepair.Core.Geometry;

public static class HashUtil
{
    public static string Sha256Hex(ReadOnlySpan<byte> data)
        => Convert.ToHexString(SHA256.HashData(data)).ToLowerInvariant();

    public static string Sha256Hex(string text)
        => Sha256Hex(Encoding.UTF8.GetBytes(text));

    public static string Sha256File(string path)
    {
        using var stream = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.Read);
        return Convert.ToHexString(SHA256.HashData(stream)).ToLowerInvariant();
    }

    public static string StableId(string prefix, IEnumerable<string> values)
    {
        var canonical = string.Join("\n", values);
        return $"{prefix}:{Sha256Hex(canonical)[..20]}";
    }

    public static string Invariant(long value) => value.ToString(CultureInfo.InvariantCulture);

    public static string Invariant(double value) => value.ToString("R", CultureInfo.InvariantCulture);
}
