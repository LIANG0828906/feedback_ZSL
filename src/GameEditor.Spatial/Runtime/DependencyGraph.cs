namespace GameEditor.Spatial;

public sealed record DependencyEdge(string From, string To, string Reason);

public sealed record InvalidationRecord(string ObjectId, string Cause, string SourceId);

public sealed class DependencyGraph
{
    private readonly Dictionary<string, List<DependencyEdge>> _edges = new(StringComparer.Ordinal);

    public static string TransformInput(string nodeId) => $"input:transform:{nodeId}";
    public static string GeometryInput(string geometryId) => $"input:geometry:{geometryId}";
    public static string WorldDerived(string nodeId) => $"derived:world:{nodeId}";
    public static string BoundsDerived(string nodeId) => $"derived:bounds:{nodeId}";
    public static string EntryDerived(string geometryId) => $"derived:entry:{geometryId}";
    public static string IndexNode(string nodeId) => $"index:node:{nodeId}";

    public IReadOnlyList<DependencyEdge> Edges =>
        _edges.Values.SelectMany(list => list)
            .OrderBy(edge => edge.From, StringComparer.Ordinal)
            .ThenBy(edge => edge.To, StringComparer.Ordinal)
            .ThenBy(edge => edge.Reason, StringComparer.Ordinal)
            .ToList();

    public static DependencyGraph Build(SceneState scene)
    {
        var graph = new DependencyGraph();
        foreach (var node in scene.Nodes.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            graph.AddEdge(
                TransformInput(node.Id),
                WorldDerived(node.Id),
                $"node {node.Id} local transform changed");
        }

        foreach (var node in scene.Nodes.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            if (node.ParentId is { } parentId)
            {
                graph.AddEdge(
                    WorldDerived(parentId),
                    WorldDerived(node.Id),
                    $"parent world transform {parentId} changed");
            }
        }

        foreach (var geometry in scene.Geometries.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            graph.AddEdge(
                GeometryInput(geometry.Id),
                EntryDerived(geometry.Id),
                $"geometry {geometry.Id} content changed");
            graph.AddEdge(
                WorldDerived(geometry.NodeId),
                EntryDerived(geometry.Id),
                $"owner world transform {geometry.NodeId} changed");
            graph.AddEdge(
                EntryDerived(geometry.Id),
                BoundsDerived(geometry.NodeId),
                $"geometry entry {geometry.Id} changed");
            graph.AddEdge(
                EntryDerived(geometry.Id),
                IndexNode(geometry.NodeId),
                $"index entry for {geometry.Id} changed");
        }

        foreach (var node in scene.Nodes.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            graph.AddEdge(
                BoundsDerived(node.Id),
                IndexNode(node.Id),
                $"derived bounds for {node.Id} changed");
        }

        foreach (var node in scene.Nodes.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            if (node.ParentId is { } parentId)
            {
                graph.AddEdge(
                    IndexNode(node.Id),
                    IndexNode(parentId),
                    $"child index summary {node.Id} changed");
            }
        }

        return graph;
    }

    public DependencyGraph Clone()
    {
        var clone = new DependencyGraph();
        foreach (var pair in _edges)
        {
            clone._edges[pair.Key] = pair.Value.ToList();
        }

        return clone;
    }

    public void AddGeometryEdges(GeometryDefinition geometry)
    {
        AddEdge(
            GeometryInput(geometry.Id),
            EntryDerived(geometry.Id),
            $"geometry {geometry.Id} content changed");
        AddEdge(
            WorldDerived(geometry.NodeId),
            EntryDerived(geometry.Id),
            $"owner world transform {geometry.NodeId} changed");
        AddEdge(
            EntryDerived(geometry.Id),
            BoundsDerived(geometry.NodeId),
            $"geometry entry {geometry.Id} changed");
        AddEdge(
            EntryDerived(geometry.Id),
            IndexNode(geometry.NodeId),
            $"index entry for {geometry.Id} changed");
    }

    public void RemoveGeometryEdges(string geometryId)
    {
        var input = GeometryInput(geometryId);
        var entry = EntryDerived(geometryId);
        foreach (var from in _edges.Keys.ToList())
        {
            _edges[from].RemoveAll(edge =>
                edge.From == input ||
                edge.From == entry ||
                edge.To == entry ||
                edge.To == input);
            if (_edges[from].Count == 0)
            {
                _edges.Remove(from);
            }
        }
    }

    public void AddEdge(string from, string to, string reason)
    {
        if (!_edges.TryGetValue(from, out var list))
        {
            list = [];
            _edges[from] = list;
        }

        if (!list.Any(edge => edge.To == to && edge.Reason == reason))
        {
            list.Add(new DependencyEdge(from, to, reason));
            list.Sort((left, right) =>
            {
                var result = string.CompareOrdinal(left.To, right.To);
                return result != 0 ? result : string.CompareOrdinal(left.Reason, right.Reason);
            });
        }
    }

    public IReadOnlyList<InvalidationRecord> CollectInvalidations(IEnumerable<string> changedInputs)
    {
        var records = new Dictionary<string, InvalidationRecord>(StringComparer.Ordinal);
        var queue = new Queue<string>();
        foreach (var input in changedInputs.OrderBy(item => item, StringComparer.Ordinal))
        {
            queue.Enqueue(input);
        }

        var visited = new HashSet<string>(StringComparer.Ordinal);
        while (queue.Count > 0)
        {
            var current = queue.Dequeue();
            if (!visited.Add(current) || !_edges.TryGetValue(current, out var outgoing))
            {
                continue;
            }

            foreach (var edge in outgoing)
            {
                if (!records.ContainsKey(edge.To))
                {
                    records[edge.To] = new InvalidationRecord(edge.To, edge.Reason, current);
                }

                if (!visited.Contains(edge.To))
                {
                    queue.Enqueue(edge.To);
                }
            }
        }

        return records.Values
            .OrderBy(item => item.ObjectId, StringComparer.Ordinal)
            .ToList();
    }
}
