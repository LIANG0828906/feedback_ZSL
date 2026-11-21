using BattleEconomy.Verify;

if (args.Length > 0 && args[0] == "--crash-child")
{
    return CrashChild.Run(args);
}

return await VerificationSuite.RunAsync();
