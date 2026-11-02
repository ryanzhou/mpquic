# Windows end-to-end smoke test (used by CI).
#
# Installs remote, relay and local instances as Windows services, proxies
# HTTP through them (direct + relay paths, and relay only), exercises the
# service commands and a failing service, runs the foreground CLI modes
# (local, tunnel, remote, relay) and the GUI smoke test, then uninstalls
# everything. Needs an elevated PowerShell 7 (GitHub's Windows runners are).
#
#   pwsh scripts/windows-smoke.ps1 -Exe .\mpquic.exe
param([Parameter(Mandatory = $true)][string]$Exe)

$ErrorActionPreference = 'Stop'
$Exe = (Resolve-Path $Exe).Path
$Data = Join-Path $env:ProgramData 'MPQUIC'
$Tmp = Join-Path ([IO.Path]::GetTempPath()) "mpquic-smoke-$PID"
New-Item -ItemType Directory -Force $Tmp | Out-Null
$Password = 'smoke-' + [guid]::NewGuid()
$Services = 'mpq-remote', 'mpq-relay', 'mpq-local', 'mpq-local2', 'mpq-bad'
$Procs = @()

# mpq runs mpquic.exe and fails on a non-zero exit code.
function mpq {
    Write-Host "> mpquic $($args -join ' ')"
    & $Exe @args
    if ($LASTEXITCODE -ne 0) { throw "mpquic $($args -join ' '): exit code $LASTEXITCODE" }
}

function Start-Mpquic([string[]]$ArgList, [string]$Name) {
    Write-Host "> mpquic $($ArgList -join ' ') (background)"
    $p = Start-Process -FilePath $Exe -ArgumentList $ArgList -PassThru -NoNewWindow `
        -RedirectStandardError (Join-Path $Tmp "$Name.err") -RedirectStandardOutput (Join-Path $Tmp "$Name.out")
    $script:Procs += $p
    return $p
}

# Fetch fetches $Url (through a SOCKS5 proxy if given), retrying while the
# paths come up, and checks the body.
function Fetch([string]$Url, [string]$Socks = '') {
    $curlArgs = @('-sS', '--max-time', '5', $Url)
    if ($Socks) { $curlArgs = @('--socks5-hostname', $Socks) + $curlArgs }
    $curlErr = Join-Path $Tmp 'curl.err'
    $body = ''
    for ($i = 1; $i -le 30; $i++) {
        $body = & curl.exe @curlArgs 2> $curlErr
        if ($LASTEXITCODE -eq 0 -and "$body".Trim() -eq 'mpquic-smoke-ok') {
            Write-Host "  fetched $Url via $(if ($Socks) { $Socks } else { 'direct' }) (attempt $i)"
            return
        }
        Start-Sleep -Seconds 1
    }
    $why = (Get-Content -Raw -ErrorAction SilentlyContinue $curlErr)
    if (-not $why) { $why = "unexpected body: '$body'" }
    throw "cannot fetch $Url via $(if ($Socks) { $Socks } else { 'direct' }): curl exit $LASTEXITCODE, $("$why".Trim())"
}

# Free-Port returns a currently unused local TCP port. Fixed ports can fall
# into Windows' excluded (Hyper-V/WinNAT reserved) port ranges on runners.
function Free-Port {
    $l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $l.Start()
    try { return $l.LocalEndpoint.Port } finally { $l.Stop() }
}

function Status([string]$Name) {
    $out = & $Exe service status --name $Name
    return ($out | Select-String '^status:').ToString() -replace '^status:\s*', ''
}

function Expect-Status([string]$Name, [string]$Want) {
    $got = Status $Name
    if ($got -notlike $Want) { throw "${Name}: status '$got', want '$Want'" }
    Write-Host "  $Name is $got"
}

try {
    mpq version

    # No arguments from a terminal prints the usage (and must not open the GUI).
    $p = Start-Process -FilePath $Exe -PassThru -NoNewWindow -RedirectStandardError (Join-Path $Tmp 'usage.err')
    if (-not $p.WaitForExit(15000)) { $p.Kill(); throw 'mpquic without arguments did not exit' }
    if (-not (Select-String -Quiet 'Usage:' (Join-Path $Tmp 'usage.err'))) { throw 'no usage output' }

    # Target web server.
    $www = Join-Path $Tmp 'www'
    New-Item -ItemType Directory -Force $www | Out-Null
    # Named parameters: with -NoNewline first, the positional Path and Value get swapped.
    Set-Content -Path (Join-Path $www "hello.txt") -Value "mpquic-smoke-ok" -NoNewline
    $python = (Get-Command python -ErrorAction SilentlyContinue).Source
    if (-not $python) { $python = (Get-Command python3).Source }
    Write-Host "> $python --version: $(& $python --version 2>&1)"
    $WebPort = Free-Port
    $Web = "127.0.0.1:$WebPort"
    $Url = "http://$Web/hello.txt"
    $WebProc = Start-Process -FilePath $python -ArgumentList '-u', '-m', 'http.server', $WebPort, '--bind', '127.0.0.1' `
        -WorkingDirectory $www -PassThru -NoNewWindow `
        -RedirectStandardOutput (Join-Path $Tmp 'http.out') -RedirectStandardError (Join-Path $Tmp 'http.err')
    $script:Procs += $WebProc
    Start-Sleep -Seconds 1
    if ($WebProc.HasExited) { throw "python http.server exited with code $($WebProc.ExitCode)" }
    Fetch $Url

    Write-Host '=== services'
    mpq service install --name mpq-remote remote -s 127.0.0.1 -p 18388 -k $Password -u
    mpq service install --name mpq-relay relay --listen 127.0.0.1:19000 --target 127.0.0.1:18388
    mpq service install --name mpq-local local -s 127.0.0.1:18388 -s 127.0.0.1:19000 -l 11080 -k $Password -u --stats 5
    mpq service install --name mpq-local2 local -s 127.0.0.1:19000 -l 11081 -k $Password
    foreach ($n in 'mpq-remote', 'mpq-relay', 'mpq-local', 'mpq-local2') { Expect-Status $n 'running' }
    Fetch $Url '127.0.0.1:11080'
    Fetch $Url '127.0.0.1:11081'   # relay path only

    # The service config is admin/LOCAL SERVICE only; the remote gets a firewall rule.
    $acl = (icacls (Join-Path $Data 'mpq-local.json')) -join "`n"
    if ($acl -match 'BUILTIN\\Users|Everyone|Authenticated Users') { throw "config is readable by users:`n$acl" }
    netsh advfirewall firewall show rule name='MPQUIC (mpq-remote)' | Out-Host
    if ($LASTEXITCODE -ne 0) { throw 'missing firewall rule' }

    mpq service stop --name mpq-local
    Expect-Status mpq-local 'stopped'
    mpq service start --name mpq-local
    mpq service restart --name mpq-remote
    Fetch $Url '127.0.0.1:11080'
    mpq service install --name mpq-local   # reinstall from the stored config
    Expect-Status mpq-local 'running'
    Fetch $Url '127.0.0.1:11080'

    # A service that cannot start (its UDP port is taken) reports the failure.
    & $Exe service install --name mpq-bad remote -s 127.0.0.1 -p 18388 -k $Password
    if ($LASTEXITCODE -eq 0) { throw 'mpq-bad should have failed to start' }
    Expect-Status mpq-bad 'stopped*'
    mpq service uninstall --name mpq-bad --purge

    Write-Host '=== foreground'
    Start-Mpquic @('remote', '-s', '127.0.0.1', '-p', '18389', '-k', $Password, '-u') 'fg-remote' | Out-Null
    Start-Mpquic @('relay', '--listen', '127.0.0.1:19001', '--target', '127.0.0.1:18389') 'fg-relay' | Out-Null
    Start-Mpquic @('local', '-s', '127.0.0.1:18389', '-s', '127.0.0.1:19001', '-l', '11090', '-k', $Password, '-u') 'fg-local' | Out-Null
    Start-Mpquic @('tunnel', '-s', '127.0.0.1:19001', '-l', '11091', '-L', $Web, '-k', $Password) 'fg-tunnel' | Out-Null
    Fetch $Url '127.0.0.1:11090'
    Fetch 'http://127.0.0.1:11091/hello.txt'

    Write-Host '=== gui'
    $p = Start-Process -FilePath $Exe -ArgumentList 'gui', '--smoke-test', '3s' -PassThru -NoNewWindow `
        -RedirectStandardOutput (Join-Path $Tmp 'gui.out') -RedirectStandardError (Join-Path $Tmp 'gui.err')
    if (-not $p.WaitForExit(60000)) { $p.Kill(); throw 'GUI smoke test timed out' }
    Get-Content (Join-Path $Tmp 'gui.out'), (Join-Path $Tmp 'gui.err') | Out-Host
    if ($p.ExitCode -ne 0 -or -not (Select-String -Quiet 'gui smoke test ok' (Join-Path $Tmp 'gui.out'))) {
        throw "GUI smoke test failed (exit code $($p.ExitCode))"
    }

    Write-Host '=== uninstall'
    foreach ($n in 'mpq-remote', 'mpq-relay', 'mpq-local', 'mpq-local2') { mpq service uninstall --name $n --purge }
    & $Exe service status --name mpq-local | Out-Null
    if ($LASTEXITCODE -ne 3) { throw "status after uninstall: exit code $LASTEXITCODE, want 3" }
    if (Test-Path (Join-Path $Data 'mpq-local.json')) { throw 'config left behind by --purge' }
    Write-Host 'windows smoke test passed'
}
catch {
    Write-Host "FAILED: $_"
    foreach ($f in Get-ChildItem -ErrorAction SilentlyContinue (Join-Path $Data 'logs'), $Tmp -File) {
        Write-Host "----- $($f.FullName)"
        Get-Content -Tail 40 $f.FullName | Out-Host
    }
    throw
}
finally {
    foreach ($p in $Procs) { if ($p -and -not $p.HasExited) { Stop-Process -Force -Id $p.Id -ErrorAction SilentlyContinue } }
    foreach ($n in $Services) { & $Exe service uninstall --name $n --purge *> $null }
}
