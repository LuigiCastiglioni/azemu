<#
.SYNOPSIS
  Installs azemu as WebAssembly run by Node.js: no .exe is created, which is what machines that block
  unsigned or unknown executables (Defender ASR "prevalence, age or trusted list", AppLocker, WDAC) need.
  No admin rights required.

.DESCRIPTION
  Default: downloads azemu.wasm, azemu-wasm.js and wasm_exec.js from a GitHub release.
  -FromSource: builds azemu.wasm from the checkout this script lives in (needs Go).
  Either way it writes azemu.cmd / azemu.ps1 shims into -Dir and adds -Dir to your user PATH.

.EXAMPLE
  irm https://github.com/luigi23997/azemu/releases/latest/download/install-wasm.ps1 | iex

.EXAMPLE
  .\scripts\install-wasm.ps1 -FromSource
#>
[CmdletBinding()]
param(
    [string]$Dir = (Join-Path $env:USERPROFILE '.azemu\bin'),
    [string]$Repo = 'luigi23997/azemu',
    [string]$Tag = 'latest',
    [switch]$FromSource,
    [switch]$NoPath
)
$ErrorActionPreference = 'Stop'

function Need($cmd, $hint) {
    if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) { throw "$cmd not found. $hint" }
}

Need node 'Install Node.js 22+ (winget install OpenJS.NodeJS.LTS).'
$nodeMajor = [int]((node --version).TrimStart('v').Split('.')[0])
if ($nodeMajor -lt 20) { throw "Node.js 20 or newer is required (found $(node --version))." }

New-Item -ItemType Directory -Force $Dir | Out-Null

if ($FromSource) {
    Need go 'Install Go (winget install GoLang.Go), or drop -FromSource to download a release.'
    $src = Split-Path -Parent $PSScriptRoot
    $goroot = (go env GOROOT).Trim()
    $wasmExec = Join-Path $goroot 'lib\wasm\wasm_exec.js'
    if (-not (Test-Path $wasmExec)) { $wasmExec = Join-Path $goroot 'misc\wasm\wasm_exec.js' }
    Push-Location $src
    try {
        $env:GOOS = 'js'; $env:GOARCH = 'wasm'
        go build -trimpath -ldflags='-s -w' -o (Join-Path $Dir 'azemu.wasm') ./cmd/azemu
        if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
    } finally {
        Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
        Pop-Location
    }
    Copy-Item -Force (Join-Path $src 'cmd\azemu\azemu-wasm.js') $Dir
    Copy-Item -Force $wasmExec (Join-Path $Dir 'wasm_exec.js')
} else {
    $base = if ($Tag -eq 'latest') { "https://github.com/$Repo/releases/latest/download" } else { "https://github.com/$Repo/releases/download/$Tag" }
    foreach ($f in 'azemu.wasm', 'azemu-wasm.js', 'wasm_exec.js') {
        Write-Host "downloading $f"
        Invoke-WebRequest -UseBasicParsing "$base/$f" -OutFile (Join-Path $Dir $f)
    }
}

Set-Content -Path (Join-Path $Dir 'azemu.cmd') -Encoding ASCII -Value "@echo off`r`nnode `"%~dp0azemu-wasm.js`" %*`r`n"
Set-Content -Path (Join-Path $Dir 'azemu.ps1') -Encoding UTF8 -Value "& node (Join-Path `$PSScriptRoot 'azemu-wasm.js') @args`r`nexit `$LASTEXITCODE`r`n"

if (-not $NoPath) {
    $user = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($user -split ';') -notcontains $Dir) {
        [Environment]::SetEnvironmentVariable('Path', "$Dir;$user", 'User')
        Write-Host "added $Dir to your user PATH: open a new terminal to use azemu"
    }
}

Write-Host "azemu $(& node (Join-Path $Dir 'azemu-wasm.js') version) installed in $Dir"
Write-Host 'next: azemu doctor --native   (checks func, node, python, uv, terraform, az login, the CA)'
Write-Host '      azemu trust              (once: adds the azemu CA to your user trust store)'
