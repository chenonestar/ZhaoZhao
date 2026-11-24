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
# 用机器上最新的 Visual Studio（CMake 自动选），编译 64 位、静态链接 C 运行库（和原版一样）
& "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe" -latest -property displayName
# 随源代码的 OpenCV 静态库是 VS 2019（vc16）编译的，OpenCV 的配置文件不认识更新的 VS：直接指定（新的 VS 能链接旧的库）
cmake -S $cpp -B $build -A x64 -DOCR_OUTPUT=BIN -DOCR_BUILD_CRT=True -DOCR_ONNX=CPU -DOpenCV_ARCH=x64 -DOpenCV_RUNTIME=vc16
if ($LASTEXITCODE -ne 0) { throw "cmake 配置失败" }
cmake --build $build --config Release -- /m /v:minimal
if ($LASTEXITCODE -ne 0) { throw "编译失败" }
$exe = Get-ChildItem $build -Recurse -Filter "RapidOcrOnnx.exe" | Select-Object -First 1
if (-not $exe) { throw "没有找到编译出的 RapidOcrOnnx.exe" }
Copy-Item $exe.FullName (Join-Path $Out "RapidOCR-json-zz.exe")
Get-Item (Join-Path $Out "RapidOCR-json-zz.exe") | Format-Table Name, Length
