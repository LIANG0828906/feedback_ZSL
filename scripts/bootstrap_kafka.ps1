param(
    [string]$KafkaHome = "C:\kafka\kafka_2.13-3.6.0",
    [string]$Workspace = (Resolve-Path (Join-Path $PSScriptRoot "..")),
    [string]$BootstrapServer = "127.0.0.1:9092",
    [int]$ControllerPort = 9093
)

$ErrorActionPreference = "Stop"
$kafkaBin = Join-Path $KafkaHome "bin\windows"
$storageScript = Join-Path $kafkaBin "kafka-storage.bat"
$serverScript = Join-Path $kafkaBin "kafka-server-start.bat"
$topicsScript = Join-Path $kafkaBin "kafka-topics.bat"
if (-not (Test-Path $storageScript)) {
    throw "Kafka Windows scripts were not found under $kafkaBin"
}

$dataDir = Join-Path $Workspace "data\kafka"
$logDir = Join-Path $Workspace "data\logs"
$configPath = Join-Path $dataDir "server.properties"
New-Item -ItemType Directory -Force -Path $dataDir, $logDir | Out-Null

$logDirProperties = $dataDir.Replace("\", "/")
$config = @"
process.roles=broker,controller
node.id=1
controller.quorum.voters=1@127.0.0.1:$ControllerPort
listeners=PLAINTEXT://127.0.0.1:9092,CONTROLLER://127.0.0.1:$ControllerPort
inter.broker.listener.name=PLAINTEXT
advertised.listeners=PLAINTEXT://127.0.0.1:9092
controller.listener.names=CONTROLLER
listener.security.protocol.map=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT
log.dirs=$logDirProperties
num.partitions=12
default.replication.factor=1
offsets.topic.replication.factor=1
transaction.state.log.replication.factor=1
transaction.state.log.min.isr=1
auto.create.topics.enable=false
"@
Set-Content -Path $configPath -Value $config -Encoding ASCII

$metaFile = Join-Path $dataDir "meta.properties"
if (-not (Test-Path $metaFile)) {
    $clusterId = (& $storageScript random-uuid).Trim()
    & $storageScript format -t $clusterId -c $configPath
    if ($LASTEXITCODE -ne 0) {
        throw "Kafka storage format failed"
    }
}

$env:KAFKA_HEAP_OPTS = "-Xms192m -Xmx384m"
$serverOut = Join-Path $logDir "kafka-server.out.log"
$serverErr = Join-Path $logDir "kafka-server.err.log"
$serverCommand = "`"$serverScript`" `"$configPath`""
$process = Start-Process -FilePath "cmd.exe" `
    -ArgumentList @("/c", $serverCommand) `
    -WorkingDirectory $KafkaHome `
    -RedirectStandardOutput $serverOut `
    -RedirectStandardError $serverErr `
    -WindowStyle Hidden `
    -PassThru
$process.Id | Set-Content (Join-Path $dataDir "kafka.pid")

$hostName, $portText = $BootstrapServer.Split(":")
$port = [int]$portText
$ready = $false
for ($attempt = 0; $attempt -lt 90; $attempt++) {
    try {
        $client = [System.Net.Sockets.TcpClient]::new()
        $client.Connect($hostName, $port)
        $client.Close()
        $ready = $true
        break
    } catch {
        Start-Sleep -Milliseconds 500
    }
}
if (-not $ready) {
    throw "Kafka did not listen on $BootstrapServer; see $serverErr"
}

& $topicsScript --bootstrap-server $BootstrapServer `
    --create --if-not-exists --topic order-events `
    --partitions 12 --replication-factor 1
if ($LASTEXITCODE -ne 0) {
    throw "Creating order-events failed"
}
Write-Host "Kafka 3.6 KRaft broker is ready on $BootstrapServer (PID $($process.Id))."

