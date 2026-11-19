using System.Text.Json;

namespace OrderGateway;

/// <summary>
/// Writes the repeatable stage report. The document contains only logical
/// names and relative offsets - no absolute paths, host names or accounts.
/// </summary>
public static class StageReportWriter
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        WriteIndented = true,
    };

    public static string Write(
        string outputDirectory,
        string scenarioName,
        IReadOnlyDictionary<string, object> content)
    {
        Directory.CreateDirectory(outputDirectory);
        var fileName = $"stage_report_{scenarioName}.json";
        var fullPath = Path.Combine(outputDirectory, fileName);

        var envelope = new Dictionary<string, object?>
        {
            ["schemaVersion"] = 1,
            ["scenario"] = scenarioName,
            ["generatedAtUtc"] = DateTimeOffset.UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ"),
            ["report"] = content,
        };

        File.WriteAllText(fullPath, JsonSerializer.Serialize(envelope, JsonOptions));
        return fullPath;
    }

    public static string WritePlanEvidence(
        string outputDirectory,
        string keyOrderLabel,
        string showplanXml)
    {
        Directory.CreateDirectory(outputDirectory);
        var fileName = $"actual_plan_{keyOrderLabel}.sqlplan";
        var fullPath = Path.Combine(outputDirectory, fileName);
        File.WriteAllText(fullPath, showplanXml);
        return fullPath;
    }
}
