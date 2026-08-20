<#
.SYNOPSIS
    Registers the sdlc engine as a Windows Scheduled Task.

.DESCRIPTION
    Unlike autoship — a scheduled one-shot that exits between ticks — `sdlc run`
    is a long-lived engine: it holds the SQLite database, the gradle slots and
    the device pool, and ticks itself every `orchestrator.poll_interval`. So the
    scheduler's job here is not "wake it up every N minutes", it is "start it at
    logon and put it back if it dies".

    The task therefore uses an at-logon trigger with no execution time limit and
    a restart policy, rather than a repeating trigger inside a working-hours
    window. Everything else matches scripts/register-task.ps1 in autoship: an
    S4U interactive-token principal, the config's directory as the working
    directory, and IgnoreNew so a second copy can never race the first.

    Crash-safety is the engine's own (SPEC §7): every state entry records the
    worktree HEAD, and on restart the worktree is reconciled before the state
    re-runs. A restart is therefore a normal event, not a recovery procedure.

.PARAMETER ExePath
    Path to sdlc.exe.

.PARAMETER ConfigPath
    Path to sdlc.yaml. Its directory becomes the task's working directory.

.PARAMETER TaskName
    Scheduled task name. Defaults to "sdlc".

.PARAMETER RestartMinutes
    Minutes to wait before restarting the engine if it exits unexpectedly.
    Defaults to 5.

.PARAMETER Once
    Register the task to run `sdlc run --once` (drain the runnable work and
    exit) instead of the persistent engine. Use this for the soak described in
    docs/running.md, where you want to watch one drain at a time.

.PARAMETER WhatIf
    Print the task definition without registering anything.

.EXAMPLE
    powershell -File scripts/register-task.ps1 `
        -ExePath C:\tools\sdlc.exe `
        -ConfigPath C:\Users\you\repos\sdlc-orchestrator\sdlc.yaml -WhatIf
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter(Mandatory = $true)][string] $ExePath,
    [Parameter(Mandatory = $true)][string] $ConfigPath,
    [string] $TaskName = 'sdlc',
    [int]    $RestartMinutes = 5,
    [switch] $Once
)

$ErrorActionPreference = 'Stop'

if (-not (Test-Path -LiteralPath $ExePath)) {
    throw "sdlc.exe not found at $ExePath"
}
if (-not (Test-Path -LiteralPath $ConfigPath)) {
    throw "sdlc.yaml not found at $ConfigPath"
}

$exe = (Resolve-Path -LiteralPath $ExePath).Path
$config = (Resolve-Path -LiteralPath $ConfigPath).Path
$workingDir = Split-Path -Parent $config

# --config is a global flag on sdlc: it precedes the subcommand.
$argument = "--config `"$config`" run"
if ($Once) { $argument += ' --once' }

$action = New-ScheduledTaskAction `
    -Execute $exe `
    -Argument $argument `
    -WorkingDirectory $workingDir

$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

# ExecutionTimeLimit 0 = unlimited: the engine is supposed to keep running, and
# a job can legitimately sit at an approval gate for a day. RestartCount puts it
# back if it dies; the engine's own lock file makes a stale overlap impossible,
# and IgnoreNew keeps Task Scheduler from queueing a second copy behind it.
$settingsArgs = @{
    AllowStartIfOnBatteries       = $true
    DontStopIfGoingOnBatteries    = $true
    StartWhenAvailable            = $true
    WakeToRun                     = $false
    MultipleInstances             = 'IgnoreNew'
    ExecutionTimeLimit            = (New-TimeSpan -Seconds 0)
}
if (-not $Once) {
    $settingsArgs.RestartCount    = 3
    $settingsArgs.RestartInterval = (New-TimeSpan -Minutes $RestartMinutes)
}
$settings = New-ScheduledTaskSettingsSet @settingsArgs

# Interactive-token principal: the agent CLIs (claude, agy) read per-user
# credentials and settings, and the builds need the user's Gradle and Android
# SDK caches — the same reason autoship registers itself this way.
$principal = New-ScheduledTaskPrincipal `
    -UserId "$env:USERDOMAIN\$env:USERNAME" `
    -LogonType S4U `
    -RunLevel Limited

$task = New-ScheduledTask `
    -Action $action `
    -Trigger $trigger `
    -Settings $settings `
    -Principal $principal `
    -Description "sdlc: run the SDLC orchestrator engine for $((Split-Path -Leaf $workingDir))"

$shape = if ($Once) { 'drain once and exit' } else { 'persistent engine' }

Write-Host "Task name:        $TaskName"
Write-Host "Command:          $exe $argument"
Write-Host "Working dir:      $workingDir"
Write-Host "Shape:            $shape"
Write-Host "Trigger:          at logon"
if (-not $Once) {
    Write-Host "Restart policy:   up to 3 times, $RestartMinutes min apart"
}
Write-Host "Time limit:       none (approval gates can wait for days)"
Write-Host "Run as:           $env:USERDOMAIN\$env:USERNAME (S4U, no stored password)"
Write-Host "Overlap policy:   IgnoreNew (sdlc also holds its own engine.lock)"

if ($PSCmdlet.ShouldProcess($TaskName, 'Register scheduled task')) {
    Register-ScheduledTask -TaskName $TaskName -InputObject $task -Force | Out-Null
    Write-Host ""
    Write-Host "Registered. Inspect it with:  schtasks /query /tn $TaskName /v /fo list"
    Write-Host "Start it now with:            schtasks /run /tn $TaskName"
    Write-Host "Watch it with:                sdlc --config `"$config`" status"
}
else {
    Write-Host ""
    Write-Host "-WhatIf: nothing was registered."
}
