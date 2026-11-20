using TacticalField.Core;

namespace TacticalField.WinForms;

internal static class Program
{
    [STAThread]
    private static void Main(string[] args)
    {
        Application.EnableVisualStyles();
        Application.SetCompatibleTextRenderingDefault(false);
        var smoke = args.Contains("--smoke", StringComparer.Ordinal);
        try
        {
            var dataDir = Path.Combine(
                Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
                "TacticalField",
                "data");
            var samples = SampleGenerator.EnsureGenerated(dataDir);
            var sample = samples.First(value => string.Equals(value.Name, "medium_12x10", StringComparison.Ordinal));
            var snapshotPath = Path.Combine(dataDir, "runtime", "snapshot.json");
            BattlefieldEngine engine;
            if (File.Exists(snapshotPath))
            {
                var recovered = BattlefieldEngine.Recover(snapshotPath);
                if (!recovered.IsSuccess || recovered.Engine is null)
                {
                    MessageBox.Show(
                        $"Snapshot recovery failed: {recovered.Code}\n{recovered.Message}",
                        "Tactical Field",
                        MessageBoxButtons.OK,
                        MessageBoxIcon.Error);
                    return;
                }

                engine = recovered.Engine;
            }
            else
            {
                engine = BattlefieldEngine.Create(sample.Definition.ToBattlefieldDefinition(), new EngineOptions
                {
                    SnapshotPath = snapshotPath,
                    PersistSnapshots = true,
                    HazardPenalty = sample.Definition.HazardPenalty
                });
            }

            Application.Run(new MainForm(engine, dataDir, smoke));
        }
        catch (Exception exception)
        {
            MessageBox.Show(exception.ToString(), "Tactical Field", MessageBoxButtons.OK, MessageBoxIcon.Error);
        }
    }
}

