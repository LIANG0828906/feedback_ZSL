param(
    [ValidateSet("stop", "start")]
    [string]$Action,
    [string]$Instance = "SQLEXPRESS_MAIN",
    [switch]$Wait
)

$ErrorActionPreference = "Stop"
$serviceName = "MSSQL`$$Instance"
$service = Get-Service -Name $serviceName -ErrorAction Stop

if ($Action -eq "stop") {
    if ($service.Status -ne "Stopped") {
        Stop-Service -Name $serviceName -Force
    }
} else {
    if ($service.Status -ne "Running") {
        Start-Service -Name $serviceName
    }
}

if ($Wait) {
    $service.WaitForStatus(
        $(if ($Action -eq "stop") { "Stopped" } else { "Running" }),
        [TimeSpan]::FromSeconds(90)
    )
}

Get-Service -Name $serviceName |
    Select-Object Name, Status, StartType |
    Format-List

