using System.Diagnostics;
using GameEditor.Spatial;

namespace GameEditor.Tests;

internal sealed class TestFailureException(string message) : Exception(message);

internal static class TestAssert
{
    public static void True(bool condition, string message)
    {
        if (!condition)
        {
            throw new TestFailureException(message);
        }
    }

    public static void False(bool condition, string message) => True(!condition, message);

    public static void Equal<T>(T expected, T actual, string message)
    {
        if (!EqualityComparer<T>.Default.Equals(expected, actual))
        {
            throw new TestFailureException($"{message}: expected '{expected}', actual '{actual}'.");
        }
    }

    public static void NotEqual<T>(T unexpected, T actual, string message)
    {
        if (EqualityComparer<T>.Default.Equals(unexpected, actual))
        {
            throw new TestFailureException($"{message}: did not expect '{actual}'.");
        }
    }

    public static void SequenceEqual<T>(
        IReadOnlyList<T> expected,
        IReadOnlyList<T> actual,
        string message)
    {
        if (expected.Count != actual.Count)
        {
            throw new TestFailureException(
                $"{message}: count expected {expected.Count}, actual {actual.Count}.");
        }

        for (var i = 0; i < expected.Count; i++)
        {
            if (!EqualityComparer<T>.Default.Equals(expected[i], actual[i]))
            {
                throw new TestFailureException(
                    $"{message}: item {i} expected '{expected[i]}', actual '{actual[i]}'.");
            }
        }
    }

    public static void Error(ErrorCode expected, DomainError? actual, string message)
    {
        if (actual is null || actual.Code != expected)
        {
            throw new TestFailureException(
                $"{message}: expected {expected}, actual {(actual is null ? "<none>" : actual.ToString())}.");
        }
    }

    public static void Success<T>(Result<T> result, string message)
    {
        if (!result.IsSuccess)
        {
            throw new TestFailureException($"{message}: {result.Error}");
        }
    }

    public static void Close(float expected, float actual, float tolerance, string message)
    {
        if (MathF.Abs(expected - actual) > tolerance)
        {
            throw new TestFailureException(
                $"{message}: expected {expected:R}, actual {actual:R}, tolerance {tolerance:R}.");
        }
    }

    public static void VectorEqual(
        System.Numerics.Vector3 expected,
        System.Numerics.Vector3 actual,
        float tolerance,
        string message)
    {
        Close(expected.X, actual.X, tolerance, $"{message}.x");
        Close(expected.Y, actual.Y, tolerance, $"{message}.y");
        Close(expected.Z, actual.Z, tolerance, $"{message}.z");
    }
}

internal static class TestRunner
{
    public static int Run(IReadOnlyList<(string Name, Action Test)> tests)
    {
        var failures = 0;
        var total = Stopwatch.StartNew();
        foreach (var (name, test) in tests)
        {
            var stopwatch = Stopwatch.StartNew();
            try
            {
                test();
                stopwatch.Stop();
                Console.WriteLine($"PASS {name} ({stopwatch.ElapsedMilliseconds} ms)");
            }
            catch (Exception exception)
            {
                failures++;
                stopwatch.Stop();
                Console.WriteLine($"FAIL {name} ({stopwatch.ElapsedMilliseconds} ms)");
                Console.WriteLine($"     {exception.GetType().Name}: {exception.Message}");
                if (exception is not TestFailureException)
                {
                    Console.WriteLine(exception.StackTrace);
                }
            }
        }

        total.Stop();
        Console.WriteLine();
        Console.WriteLine($"{tests.Count - failures}/{tests.Count} tests passed in {total.Elapsed.TotalSeconds:F2}s.");
        return failures == 0 ? 0 : 1;
    }
}
