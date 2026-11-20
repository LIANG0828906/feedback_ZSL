using System.Globalization;
using System.Text;

namespace TacticalField.Core;

public static class SampleRunner
{
    public static SampleRunResult Run(SampleDefinition definition, EngineOptions? options = null)
    {
        definition.Validate();
        var engine = BattlefieldEngine.Create(definition.ToBattlefieldDefinition(), options ?? new EngineOptions
        {
            PersistSnapshots = false
        });
        var initialView = engine.Current;
        var builder = new StringBuilder();
        builder.Append("TACTICAL_FIELD_TRACE_V1\n");
        builder.Append("sample=").Append(definition.Name).Append('\n');
        builder.Append("grid=").Append(definition.Width.ToString(CultureInfo.InvariantCulture))
            .Append('x').Append(definition.Height.ToString(CultureInfo.InvariantCulture)).Append('\n');
        builder.Append("hazard_penalty=").Append(definition.HazardPenalty.ToString(CultureInfo.InvariantCulture)).Append('\n');
        builder.Append("initial version=").Append(initialView.Version.ToString(CultureInfo.InvariantCulture))
            .Append(" sequence=").Append(initialView.BatchSequence.ToString(CultureInfo.InvariantCulture))
            .Append(" chain=").Append(initialView.ChainHash).Append('\n');

        AppendFields(builder, "initial_field", initialView);

        var hits = 0;
        var misses = 0;
        foreach (var key in OrderedFieldKeys(initialView))
        {
            AppendQuery(builder, engine, initialView, key, "initial", ref hits, ref misses);
        }

        var results = new List<BatchResult>();
        foreach (var batch in definition.Batches.OrderBy(batch => batch.Index))
        {
            var changes = batch.Changes.Select(change => change.ToBattleChange()).ToArray();
            var result = engine.SubmitBatch(changes);
            results.Add(result);
            builder.Append("batch index=").Append(batch.Index.ToString(CultureInfo.InvariantCulture))
                .Append(" sequence=").Append(result.BatchSequence.ToString(CultureInfo.InvariantCulture))
                .Append(" changes=").Append(string.Join(";", changes.Select(change => change.Canonical())))
                .Append('\n');
            builder.Append("batch_result code=").Append(result.Code)
                .Append(" version=").Append(result.PublicationVersion.ToString(CultureInfo.InvariantCulture))
                .Append(" sequence=").Append(result.BatchSequence.ToString(CultureInfo.InvariantCulture))
                .Append(" chain=").Append(result.ChainHash)
                .Append('\n');

            if (!result.IsSuccess)
            {
                builder.Append("batch_error message=").Append(result.Message ?? string.Empty).Append('\n');
                break;
            }

            foreach (var delta in result.Deltas)
            {
                builder.Append("delta key=").Append(delta.Key)
                    .Append(" impact=[").Append(CanonicalText.JoinInts(delta.ImpactCells)).Append(']')
                    .Append(" impact_count=").Append(delta.ImpactCells.Length.ToString(CultureInfo.InvariantCulture))
                    .Append(" recomputed=[").Append(CanonicalText.JoinInts(delta.RecomputedCells)).Append(']')
                    .Append(" recomputed_count=").Append(delta.RecomputedCells.Length.ToString(CultureInfo.InvariantCulture))
                    .Append(" reused_count=").Append(delta.ReusedCells.ToString(CultureInfo.InvariantCulture))
                    .Append(" old_version=").Append(delta.OldFieldVersion.ToString(CultureInfo.InvariantCulture))
                    .Append(" new_version=").Append(delta.NewFieldVersion.ToString(CultureInfo.InvariantCulture))
                    .Append(" old_hash=").Append(delta.OldHash)
                    .Append(" new_hash=").Append(delta.NewHash)
                    .Append('\n');
            }

            var view = engine.Current;
            foreach (var key in OrderedFieldKeys(view))
            {
                AppendQuery(builder, engine, view, key, $"batch:{batch.Index}", ref hits, ref misses);
            }
        }

        var finalView = engine.Current;
        builder.Append("final version=").Append(finalView.Version.ToString(CultureInfo.InvariantCulture))
            .Append(" sequence=").Append(finalView.BatchSequence.ToString(CultureInfo.InvariantCulture))
            .Append(" chain=").Append(finalView.ChainHash).Append('\n');
        AppendFields(builder, "final_field", finalView);
        builder.Append("cache hits=").Append(hits.ToString(CultureInfo.InvariantCulture))
            .Append(" misses=").Append(misses.ToString(CultureInfo.InvariantCulture)).Append('\n');

        var trace = builder.ToString();
        return new SampleRunResult(
            trace,
            Hashing.Hex(Hashing.Sha256Utf8(trace)),
            initialView,
            finalView,
            results.AsReadOnly(),
            hits,
            misses);
    }

    private static void AppendFields(StringBuilder builder, string prefix, BattlefieldView view)
    {
        foreach (var field in view.Fields.Values.OrderBy(field => field.Key.UnitId, StringComparer.Ordinal).ThenBy(field => field.Key.Target))
        {
            builder.Append(prefix)
                .Append(" unit=").Append(field.Key.UnitId)
                .Append(" target=").Append(field.Key.Target.ToString(CultureInfo.InvariantCulture))
                .Append(" version=").Append(field.FieldVersion.ToString(CultureInfo.InvariantCulture))
                .Append(" hash=").Append(field.ContentHash)
                .Append(" reachable=").Append(field.ReachableCount.ToString(CultureInfo.InvariantCulture))
                .Append('\n');
        }
    }

    private static void AppendQuery(
        StringBuilder builder,
        BattlefieldEngine engine,
        BattlefieldView view,
        FieldKey key,
        string phase,
        ref int hits,
        ref int misses)
    {
        var unit = view.Units[key.UnitId];
        var query = engine.QueryPath(
            key.UnitId,
            key.Target,
            unit.StartCell,
            expectedFieldVersion: view.Fields[key].FieldVersion,
            expectedFieldHash: view.Fields[key].ContentHash);
        if (query.CacheHit)
        {
            hits++;
        }
        else
        {
            misses++;
        }

        builder.Append("query phase=").Append(phase)
            .Append(" unit=").Append(key.UnitId)
            .Append(" target=").Append(key.Target.ToString(CultureInfo.InvariantCulture))
            .Append(" start=").Append(unit.StartCell.ToString(CultureInfo.InvariantCulture))
            .Append(" code=").Append(query.Code)
            .Append(" cache=").Append(query.CacheHit ? "hit" : "miss")
            .Append(" cost=").Append(query.Cost.ToString(CultureInfo.InvariantCulture))
            .Append(" path=[").Append(CanonicalText.JoinInts(query.Cells)).Append(']')
            .Append(" path_hash=").Append(query.PathHash)
            .Append(" deps=[").Append(CanonicalText.JoinInts(query.Dependencies)).Append(']')
            .Append(" field_version=").Append(query.FieldVersion.ToString(CultureInfo.InvariantCulture))
            .Append(" field_hash=").Append(query.FieldHash)
            .Append(" publication=").Append(query.PublicationVersion.ToString(CultureInfo.InvariantCulture))
            .Append('\n');
    }

    private static IEnumerable<FieldKey> OrderedFieldKeys(BattlefieldView view)
        => view.Fields.Keys.OrderBy(key => key.UnitId, StringComparer.Ordinal).ThenBy(key => key.Target);
}

