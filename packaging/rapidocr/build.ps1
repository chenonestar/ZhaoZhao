# 编译找找用的 RapidOCR-json：和发布包同一个版本的源代码，打上 patch.py 的补丁（线程数真正生效、每张图可以
# 单独指定找文字的图多大），用随源代码提供的静态库编译。结果放到 $Out\RapidOCR-json-zz.exe。
param([Parameter(Mandatory = $true)][string]$Out, [string]$Tag = "")
$ErrorActionPreference = "Stop"
$work = Join-Path $env:RUNNER_TEMP "rapidocr-src"
Remove-Item $work -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $work, $Out | Out-Null
if (-not $Tag) { $Tag = (gh release view --repo hiroi-sora/RapidOCR-json --json tagName | ConvertFrom-Json).tagName }
Write-Host "RapidOCR-json 源代码：$Tag"
gh api "repos/hiroi-sora/RapidOCR-json/zipball/$Tag" > (Join-Path $work "src.zip")
Expand-Archive (Join-Path $work "src.zip") $work
$cpp = (Get-ChildItem $work -Directory | Select-Object -First 1).FullName + "\cpp"
7z x -y "-o$cpp" (Join-Path $cpp "lib.7z") | Select-Object -Last 2
# lib.7z 里可能多一层 lib 目录
foreach ($d in "onnxruntime-static", "opencv-static") {
  if (-not (Test-Path "$cpp\$d") -and (Test-Path "$cpp\lib\$d")) { Move-Item "$cpp\lib\$d" "$cpp\$d" }
  if (-not (Test-Path "$cpp\$d")) { throw "lib.7z 里没有 $d" }
}
$env:PYTHONIOENCODING = "utf-8"
python (Join-Path $PSScriptRoot "patch.py") $cpp
if ($LASTEXITCODE -ne 0) { throw "打补丁失败" }
$build = Join-Path $cpp "build-zz"
cmake -S $cpp -B $build -G "Visual Studio 17 2022" -A x64 -DOCR_OUTPUT=BIN -DOCR_BUILD_CRT=True -DOCR_ONNX=CPU
if ($LASTEXITCODE -ne 0) { throw "cmake 配置失败" }
cmake --build $build --config Release -- /m /v:minimal
if ($LASTEXITCODE -ne 0) { throw "编译失败" }
$exe = Get-ChildItem $build -Recurse -Filter "RapidOcrOnnx.exe" | Select-Object -First 1
if (-not $exe) { throw "没有找到编译出的 RapidOcrOnnx.exe" }
Copy-Item $exe.FullName (Join-Path $Out "RapidOCR-json-zz.exe")
Get-Item (Join-Path $Out "RapidOCR-json-zz.exe") | Format-Table Name, Length
