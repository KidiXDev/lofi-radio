$ErrorActionPreference = "Stop"

$owner = "KidiXDev"
$repo = "lofi-radio"
$apiUrl = "https://api.github.com/repos/$owner/$repo/releases/latest"

$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
switch ($arch) {
    "x64" { $archLabel = "x86_64" }
    "arm64" { $archLabel = "arm64" }
    default { throw "Unsupported architecture: $arch" }
}

$assetName = "lofi-radio_Windows_${archLabel}.zip"
$release = Invoke-RestMethod -Uri $apiUrl -Headers @{ "User-Agent" = "lofi-radio-installer" }
$asset = $release.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
if (-not $asset) {
    throw "Failed to find asset: $assetName"
}

$installDir = Join-Path $env:LOCALAPPDATA "lofi-radio\bin"
New-Item -ItemType Directory -Path $installDir -Force | Out-Null

$tmpZip = Join-Path ([System.IO.Path]::GetTempPath()) ("lofi-radio-" + [guid]::NewGuid() + ".zip")
Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $tmpZip

Expand-Archive -Path $tmpZip -DestinationPath $installDir -Force
Remove-Item -Path $tmpZip -Force

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not $userPath) {
    $userPath = ""
}
$parts = $userPath.Split(';', [System.StringSplitOptions]::RemoveEmptyEntries)
if ($parts -notcontains $installDir) {
    $newPath = if ([string]::IsNullOrWhiteSpace($userPath)) { $installDir } else { "$userPath;$installDir" }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Write-Host "Added $installDir to User PATH. Open a new terminal to use 'lofi'."
}

Write-Host "Installed: $installDir\lofi.exe"
Write-Host "Run: lofi"
