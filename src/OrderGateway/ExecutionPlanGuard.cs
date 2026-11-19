using System.Xml.Linq;

namespace OrderGateway;

public sealed record PlanAccessPath(string IndexName, string AccessMethod, bool IsTableScan);

/// <summary>
/// Parses an actual SHOWPLAN XML document and reports every access path
/// touching dbo.Orders. In showplan XML the operator kind lives on the parent
/// RelOp (PhysicalOp/LogicalOp, e.g. "Index Seek") while IndexScan/TableScan
/// are merely element names; Seek vs Scan is therefore taken from RelOp and
/// the targeted index from the nested Object node.
/// </summary>
public static class ExecutionPlanGuard
{
    private static readonly XNamespace ShowplanNs =
        "http://schemas.microsoft.com/sqlserver/2004/07/showplan";

    public static IReadOnlyList<PlanAccessPath> ExtractOrderAccessPaths(string showplanXml)
    {
        var doc = XDocument.Parse(showplanXml);
        var paths = new List<PlanAccessPath>();

        foreach (var relOp in doc.Descendants(ShowplanNs + "RelOp"))
        {
            var physicalOp = (string?)relOp.Attribute("PhysicalOp") ?? string.Empty;
            if (physicalOp is not ("Index Scan" or "Index Seek" or "Clustered Index Scan"
                or "Clustered Index Seek" or "Table Scan"))
            {
                continue;
            }

            var objectNode = relOp.Descendants(ShowplanNs + "Object").FirstOrDefault();
            if (objectNode is null) continue;

            var table = (string?)objectNode.Attribute("Table") ?? string.Empty;
            if (!table.Contains("Orders", StringComparison.OrdinalIgnoreCase)) continue;

            var indexName = CleanName((string?)objectNode.Attribute("Index") ?? string.Empty);
            bool isScan = physicalOp.Contains("Scan", StringComparison.OrdinalIgnoreCase);
            paths.Add(new PlanAccessPath(indexName, physicalOp.Trim(), isScan));
        }

        return paths;
    }

    private static string CleanName(string raw)
    {
        if (string.IsNullOrWhiteSpace(raw)) return "(none)";
        return raw.TrimStart('[').Replace("].[", ".").TrimEnd(']');
    }

    public static PlanAccessPath SelectDominantPath(IReadOnlyList<PlanAccessPath> paths)
    {
        if (paths.Count == 0)
            return new PlanAccessPath("(none)", "Unknown", true);

        return paths
            .OrderBy(p => p.IsTableScan)
            .ThenBy(p => p.AccessMethod.Contains("Seek", StringComparison.OrdinalIgnoreCase) ? 0 : 1)
            .First();
    }
}
