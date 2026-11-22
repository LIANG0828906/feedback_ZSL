using System.Globalization;
using System.Text;
using System.Text.Json;
using SaveKernel.Hashing;
using SaveKernel.Model;

namespace SaveKernel.Export;

/// <summary>
/// 把内部模型导出为指定受支持版本的物理格式。
/// 对低版本无法表达的字段生成 E_LOSSY 警告清单（字段路径、原值、处置方式）。
/// 单位换算与字段取舍遵循声明的降级映射规则。
/// </summary>
public static class SaveExporter
{
    public const string DispositionDropped = "Dropped(字段取舍:低版本无法表达)";
    public const string DispositionTruncated = "Truncated(毫秒整除截断为秒)";
    public const string DispositionConverted = "Converted(毫秒换算为秒)";

    public static (string Text, LossReport Losses) Export(SaveModel model, int targetVersion)
    {
        if (model.Version != SaveModel.CurrentVersion)
            throw new SaveKernelException(ErrorCode.E_VER, $"只能导出当前内部模型(v{SaveModel.CurrentVersion})，当前为 v{model.Version}");
        return targetVersion switch
        {
            3 => (ExportV3(model), new LossReport()),
            2 => ExportV2(model),
            1 => ExportV1(model),
            _ => throw new SaveKernelException(ErrorCode.E_VER, $"不支持导出的版本: {targetVersion}", "version", actual: targetVersion.ToString())
        };
    }

    private static string ExportV3(SaveModel m)
    {
        using var stream = new MemoryStream();
        using (var writer = new Utf8JsonWriter(stream, new JsonWriterOptions { Indented = true }))
        {
            writer.WriteStartObject();
            writer.WriteNumber("version", 3);
            writer.WriteNumber("levelId", m.LevelId);
            writer.WriteNumber("gold", m.Gold);
            writer.WriteStartObject("position");
            writer.WriteNumber("x", m.PosX);
            writer.WriteNumber("y", m.PosY);
            writer.WriteEndObject();
            writer.WriteNumber("playtimeMs", m.Playtime);
            writer.WriteStartObject("skillTree");
            foreach (var kv in m.SkillTree) writer.WriteNumber(kv.Key, kv.Value);
            writer.WriteEndObject();
            writer.WriteStartArray("inventory");
            foreach (var item in m.Inventory)
            {
                writer.WriteStartObject();
                writer.WriteString("itemId", item.ItemId);
                writer.WriteNumber("count", item.Count);
                writer.WriteNumber("durability", item.Durability ?? 0);
                writer.WriteEndObject();
            }
            writer.WriteEndArray();
            writer.WriteEndObject();
        }
        return Encoding.UTF8.GetString(stream.ToArray());
    }

    private static (string, LossReport) ExportV2(SaveModel m)
    {
        var losses = new LossReport();
        DropSkillTree(m, losses);
        var sb = new StringBuilder();
        sb.AppendLine("version=2");
        sb.AppendLine($"Level={m.LevelId.ToString(CultureInfo.InvariantCulture)}");
        sb.AppendLine($"Gold={m.Gold.ToString(CultureInfo.InvariantCulture)}");
        sb.AppendLine($"PlayerX={CanonicalForm.FmtDouble(m.PosX)}");
        sb.AppendLine($"PlayerY={CanonicalForm.FmtDouble(m.PosY)}");
        sb.AppendLine($"PlaytimeMs={m.Playtime.ToString(CultureInfo.InvariantCulture)}");
        sb.AppendLine($"InventoryCount={m.Inventory.Count.ToString(CultureInfo.InvariantCulture)}");
        for (var i = 0; i < m.Inventory.Count; i++)
        {
            var item = m.Inventory[i];
            sb.AppendLine($"Inventory.{i}.ItemId={item.ItemId}");
            sb.AppendLine($"Inventory.{i}.Count={item.Count.ToString(CultureInfo.InvariantCulture)}");
            sb.AppendLine($"Inventory.{i}.Durability={(item.Durability ?? 0).ToString(CultureInfo.InvariantCulture)}");
        }
        return (sb.ToString(), losses);
    }

    private static (string, LossReport) ExportV1(SaveModel m)
    {
        var losses = new LossReport();
        DropSkillTree(m, losses);

        var seconds = m.Playtime / 1000;
        var remainder = m.Playtime % 1000;
        losses.Add("playtime",
            m.Playtime.ToString(CultureInfo.InvariantCulture) + "ms",
            remainder == 0 ? DispositionConverted : DispositionTruncated);

        var sb = new StringBuilder();
        sb.AppendLine("version=1");
        sb.AppendLine($"level_id={m.LevelId.ToString(CultureInfo.InvariantCulture)}");
        sb.AppendLine($"gold={m.Gold.ToString(CultureInfo.InvariantCulture)}");
        sb.AppendLine($"pos_x={CanonicalForm.FmtDouble(m.PosX)}");
        sb.AppendLine($"pos_y={CanonicalForm.FmtDouble(m.PosY)}");
        sb.AppendLine($"playtime_sec={seconds.ToString(CultureInfo.InvariantCulture)}");
        sb.AppendLine($"item_count={m.Inventory.Count.ToString(CultureInfo.InvariantCulture)}");
        for (var i = 0; i < m.Inventory.Count; i++)
        {
            var item = m.Inventory[i];
            sb.AppendLine($"item.{i}.id={item.ItemId}");
            sb.AppendLine($"item.{i}.count={item.Count.ToString(CultureInfo.InvariantCulture)}");
            if (item.Durability is not null)
                losses.Add(CanonicalForm.ItemPath(i, "durability"),
                    item.Durability.Value.ToString(CultureInfo.InvariantCulture), DispositionDropped);
        }
        return (sb.ToString(), losses);
    }

    private static void DropSkillTree(SaveModel m, LossReport losses)
    {
        foreach (var kv in m.SkillTree)
            losses.Add($"skillTree.{kv.Key}", kv.Value.ToString(CultureInfo.InvariantCulture), DispositionDropped);
    }
}
