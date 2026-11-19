namespace InventoryMigration.Api.Services;

public sealed class LatencyTracker
{
    private readonly object _sync = new();
    private readonly Queue<double> _samples = new();
    private readonly int _capacity = 20_000;

    public void Record(TimeSpan elapsed)
    {
        var milliseconds = elapsed.TotalMilliseconds;
        lock (_sync)
        {
            _samples.Enqueue(milliseconds);
            while (_samples.Count > _capacity)
            {
                _samples.Dequeue();
            }
        }
    }

    public double P95Ms()
    {
        double[] values;
        lock (_sync)
        {
            if (_samples.Count == 0)
            {
                return 0;
            }

            values = _samples.ToArray();
        }

        Array.Sort(values);
        var index = (int)Math.Ceiling(values.Length * 0.95) - 1;
        return Math.Round(values[Math.Clamp(index, 0, values.Length - 1)], 3);
    }
}
