$ErrorActionPreference = "Stop"

$projectRoot = [System.IO.Path]::GetFullPath((Split-Path -Parent $MyInvocation.MyCommand.Path)).TrimEnd('\')
$buildRoot = Join-Path $projectRoot "build-v59"
$bundleRoot = Join-Path $buildRoot "rafael"
$bundleBinRoot = Join-Path $bundleRoot "bin"
$binaryName = "rafael-panel-linux-amd64"
$archivePath = Join-Path $buildRoot "rafael-linux-amd64.tar.gz"
$checksumPath = "$archivePath.sha256"
$xraySourceRoot = Join-Path $projectRoot "third_party\xray-core"
$xrayBinaryAsset = Join-Path $buildRoot "rafael-xray-linux-amd64"
$xrayBinaryChecksum = "$xrayBinaryAsset.sha256"
$xraySourceArchive = Join-Path $buildRoot "rafael-xray-core-source.tar.gz"
$xraySourceChecksum = "$xraySourceArchive.sha256"

function Assert-InProject([string]$Path) {
    $resolved = [System.IO.Path]::GetFullPath($Path)
    $prefix = $projectRoot + [System.IO.Path]::DirectorySeparatorChar
    if (-not $resolved.StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to clean a path outside the project directory: $resolved"
    }
}

function Reset-BuildDirectory([string]$Path) {
    Assert-InProject $Path
    if (Test-Path -LiteralPath $Path) {
        Remove-Item -LiteralPath $Path -Recurse -Force
    }
    New-Item -ItemType Directory -Force -Path $Path | Out-Null
}

function Download-File {
    param([string]$Url, [string]$Destination)
    if (Get-Command curl.exe -ErrorAction SilentlyContinue) {
        & curl.exe --fail --location --retry 5 --retry-all-errors --connect-timeout 20 --output $Destination $Url
        if ($LASTEXITCODE -ne 0) { throw "curl.exe failed downloading $Url" }
        return
    }
    $lastError = $null
    for ($attempt = 1; $attempt -le 5; $attempt++) {
        try {
            Invoke-WebRequest -Uri $Url -OutFile $Destination
            return
        }
        catch {
            $lastError = $_
            Start-Sleep -Seconds ([Math]::Min($attempt * 2, 10))
        }
    }
    throw $lastError
}

New-Item -ItemType Directory -Force -Path $buildRoot | Out-Null
Reset-BuildDirectory $bundleRoot
New-Item -ItemType Directory -Force -Path $bundleBinRoot | Out-Null

if (-not (Test-Path -LiteralPath (Join-Path $xraySourceRoot "app\iplimit\tracker.go"))) {
    throw "Vendored Rafael Xray source is missing app/iplimit/tracker.go."
}
if (-not (Select-String -LiteralPath (Join-Path $xraySourceRoot "app\dispatcher\default.go") -Pattern 'RAFAEL:IPLIMIT' -Quiet)) {
    throw "Vendored Xray dispatcher does not contain the Rafael IP-limit hook."
}

$xrayVersionFile = Join-Path $projectRoot "xray-fork-version.txt"
$xrayVersionText = Get-Content -Raw -LiteralPath $xrayVersionFile
$xrayForkCommit = [regex]::Match($xrayVersionText, '(?m)^fork_base_commit=([0-9a-f]{40})\s*$').Groups[1].Value
$xraySourcePin = [regex]::Match($xrayVersionText, '(?m)^source_sha256=([0-9a-f]{64})\s*$').Groups[1].Value
if (-not $xrayForkCommit -or -not $xraySourcePin) { throw "xray-fork-version.txt must pin the fork commit and vendored source fingerprint." }

$xrayFiles = @(Get-ChildItem -LiteralPath $xraySourceRoot -Recurse -File)
$xrayRelativePaths = [string[]]@($xrayFiles | ForEach-Object { [System.IO.Path]::GetRelativePath($xraySourceRoot, $_.FullName).Replace('\', '/') })
[Array]::Sort($xrayRelativePaths, [System.StringComparer]::Ordinal)
$xrayRows = foreach ($relative in $xrayRelativePaths) {
    $fullPath = Join-Path $xraySourceRoot ($relative.Replace('/', [System.IO.Path]::DirectorySeparatorChar))
    $fileHash = (Get-FileHash -LiteralPath $fullPath -Algorithm SHA256).Hash.ToLowerInvariant()
    "$relative  $fileHash"
}
$xrayManifest = ($xrayRows -join "`n") + "`n"
$xrayActualPin = [Convert]::ToHexString([System.Security.Cryptography.SHA256]::HashData([System.Text.UTF8Encoding]::new($false).GetBytes($xrayManifest))).ToLowerInvariant()
if ($xrayActualPin -ne $xraySourcePin) { throw "Vendored Xray source fingerprint mismatch; refusing to build a different IP-limit fork snapshot." }

Push-Location $projectRoot
try {
    $env:CGO_ENABLED = "0"
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    go build -mod=mod -trimpath -ldflags "-s -w" -o (Join-Path $buildRoot $binaryName) .
    if ($LASTEXITCODE -ne 0) { throw "Go build failed." }

    Copy-Item (Join-Path $buildRoot $binaryName) (Join-Path $bundleRoot "rafael-panel")
    foreach ($file in @(
        "bootstrap.sh",
        "install-local.sh",
        "rafael.sh",
        "rafael.service.debian",
        "rafael-updater.sh",
        "rafael-launcher.sh",
        "LICENSE",
        "NOTICE",
        "README-v59.md",
        "RELEASE_NOTES_v59.md",
        "BUILD_V59_CHECKLIST.md",
        "xray-fork-version.txt"
    )) {
        Copy-Item (Join-Path $projectRoot $file) $bundleRoot
    }
    Copy-Item (Join-Path $xraySourceRoot "LICENSE") (Join-Path $bundleRoot "LICENSE-XRAY-MPL-2.0")

    # Pin the stable updater before it is bundled so the CLI can reject a
    # mismatched updater when installing from a release archive.
    $bundledUpdater = Join-Path $bundleRoot "rafael-updater.sh"
    $bundledCli = Join-Path $bundleRoot "rafael.sh"
    $updaterHash = (Get-FileHash -LiteralPath $bundledUpdater -Algorithm SHA256).Hash.ToLowerInvariant()
    $cliText = [System.IO.File]::ReadAllText($bundledCli)
    if ($cliText -notmatch 'BOOTSTRAP_UPDATER_SHA256=""') {
        throw "rafael.sh is missing the empty BOOTSTRAP_UPDATER_SHA256 placeholder."
    }
    $cliText = $cliText.Replace('BOOTSTRAP_UPDATER_SHA256=""', "BOOTSTRAP_UPDATER_SHA256=`"$updaterHash`"")
    [System.IO.File]::WriteAllText($bundledCli, $cliText, [System.Text.UTF8Encoding]::new($false))

    Push-Location $xraySourceRoot
    try {
        $xrayBuildId = "rafael-iplimit-$($xrayForkCommit.Substring(0, 7))-$($xraySourcePin.Substring(0, 12))"
        go build -trimpath -buildvcs=false -ldflags "-s -w -X github.com/xtls/xray-core/core.build=$xrayBuildId" -o (Join-Path $bundleBinRoot "xray-linux-amd64") ./main
        if ($LASTEXITCODE -ne 0) { throw "Building the vendored Rafael Xray IP-limit fork failed." }
    }
    finally {
        Pop-Location
    }
    if (Test-Path -LiteralPath $xrayBinaryAsset) { Remove-Item -LiteralPath $xrayBinaryAsset -Force }
    if (Test-Path -LiteralPath $xrayBinaryChecksum) { Remove-Item -LiteralPath $xrayBinaryChecksum -Force }
    Copy-Item (Join-Path $bundleBinRoot "xray-linux-amd64") $xrayBinaryAsset
    $xrayBinaryHash = (Get-FileHash -LiteralPath $xrayBinaryAsset -Algorithm SHA256).Hash.ToLowerInvariant()
    "$xrayBinaryHash  $(Split-Path -Leaf $xrayBinaryAsset)" | Set-Content -LiteralPath $xrayBinaryChecksum -Encoding ascii
    Download-File -Url "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat" -Destination (Join-Path $bundleBinRoot "geoip.dat")
    Download-File -Url "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat" -Destination (Join-Path $bundleBinRoot "geosite.dat")

    if (Test-Path -LiteralPath $xraySourceArchive) { Remove-Item -LiteralPath $xraySourceArchive -Force }
    if (Test-Path -LiteralPath $xraySourceChecksum) { Remove-Item -LiteralPath $xraySourceChecksum -Force }
    tar -czf $xraySourceArchive -C (Split-Path -Parent $xraySourceRoot) (Split-Path -Leaf $xraySourceRoot)
    if ($LASTEXITCODE -ne 0) { throw "Could not create the Rafael Xray source archive." }
    $xraySourceHash = (Get-FileHash -LiteralPath $xraySourceArchive -Algorithm SHA256).Hash.ToLowerInvariant()
    "$xraySourceHash  $(Split-Path -Leaf $xraySourceArchive)" | Set-Content -LiteralPath $xraySourceChecksum -Encoding ascii

    if (Test-Path -LiteralPath $archivePath) { Remove-Item -LiteralPath $archivePath -Force }
    if (Test-Path -LiteralPath $checksumPath) { Remove-Item -LiteralPath $checksumPath -Force }
    tar -czf $archivePath -C $buildRoot "rafael"
    if ($LASTEXITCODE -ne 0) { throw "Could not create the release archive." }

    $members = @(tar -tzf $archivePath)
    if ($LASTEXITCODE -ne 0) { throw "Could not read the release archive." }
    foreach ($required in @(
        "rafael/rafael-panel",
        "rafael/install-local.sh",
        "rafael/rafael-updater.sh",
        "rafael/rafael.sh",
        "rafael/rafael.service.debian",
        "rafael/README-v59.md",
        "rafael/RELEASE_NOTES_v59.md",
        "rafael/LICENSE",
        "rafael/LICENSE-XRAY-MPL-2.0",
        "rafael/NOTICE",
        "rafael/BUILD_V59_CHECKLIST.md",
        "rafael/xray-fork-version.txt",
        "rafael/bin/xray-linux-amd64",
        "rafael/bin/geoip.dat",
        "rafael/bin/geosite.dat"
    )) {
        if ($members -notcontains $required) { throw "Release archive is missing $required." }
    }
    $sourceMembers = @(tar -tzf $xraySourceArchive)
    if ($LASTEXITCODE -ne 0) { throw "Could not read the Xray source archive." }
    foreach ($required in @("xray-core/app/iplimit/tracker.go", "xray-core/app/dispatcher/default.go", "xray-core/LICENSE")) {
        if ($sourceMembers -notcontains $required) { throw "Xray source archive is missing $required." }
    }
    $sourceChecksumLine = (Get-Content -Raw -LiteralPath $xraySourceChecksum).Trim()
    if ($sourceChecksumLine -notmatch "^$xraySourceHash  rafael-xray-core-source\.tar\.gz$") {
        throw "Xray source archive checksum sidecar does not match the generated archive."
    }
    $bundledXrayHash = (Get-FileHash -LiteralPath (Join-Path $bundleBinRoot "xray-linux-amd64") -Algorithm SHA256).Hash.ToLowerInvariant()
    $binaryChecksumLine = (Get-Content -Raw -LiteralPath $xrayBinaryChecksum).Trim()
    if ($xrayBinaryHash -ne $bundledXrayHash -or $binaryChecksumLine -notmatch "^$xrayBinaryHash  rafael-xray-linux-amd64$") {
        throw "Standalone Xray binary does not match the binary bundled with Rafael Panel."
    }
    $cliInArchive = (& tar -xzOf $archivePath "rafael/rafael.sh") -join "`n"
    if ($cliInArchive -notmatch '(?m)^CLI_BUILD_VERSION="v59"$') { throw "The release archive is not marked v59." }
    if ($cliInArchive -notmatch "BOOTSTRAP_UPDATER_SHA256=`"$updaterHash`"") { throw "Updater pin in archive does not match the bundled updater." }

    $pincheckRoot = Join-Path $buildRoot "pincheck"
    Reset-BuildDirectory $pincheckRoot
    tar -xzf $archivePath -C $pincheckRoot "rafael/rafael-updater.sh"
    if ($LASTEXITCODE -ne 0) { throw "Could not extract the bundled updater for pin verification." }
    $updaterInArchiveHash = (Get-FileHash -LiteralPath (Join-Path $pincheckRoot "rafael/rafael-updater.sh") -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($updaterInArchiveHash -ne $updaterHash) { throw "Bundled updater hash does not match the CLI pin." }

    $archiveHash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    "$archiveHash  $(Split-Path -Leaf $archivePath)" | Set-Content -LiteralPath $checksumPath -Encoding ascii
    Write-Host "Built v59 archive: $archivePath"
    Write-Host "SHA-256: $checksumPath"
    Write-Host "Xray fork commit: $xrayForkCommit"
    Write-Host "Xray source fingerprint: $xrayActualPin"
    Write-Host "Verified standalone fork binary, matching source archive, and checksums. No panel/browser was launched."
}
finally {
    Pop-Location
}
