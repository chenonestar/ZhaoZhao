# 实验（只测不换）：用 Windows 自带的 OCR（Windows.Media.Ocr）识别目录里的 BMP，每页输出一行 JSON：
# {"file": 文件名, "ms": 识别用时（毫秒，不含读图）, "text": 识别出的文字}。由 ocr_model_test.py 调用并算准确率。
# 必须用 Windows PowerShell 5.1（powershell.exe）：它能直接调用 Windows 运行时的接口，PowerShell 7 不行。
param([Parameter(Mandatory)][string]$Dir, [string]$Lang = 'zh-Hans-CN')
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)

Add-Type -AssemblyName System.Runtime.WindowsRuntime
$null = [Windows.Storage.StorageFile, Windows.Storage, ContentType = WindowsRuntime]
$null = [Windows.Storage.Streams.IRandomAccessStream, Windows.Storage.Streams, ContentType = WindowsRuntime]
$null = [Windows.Graphics.Imaging.BitmapDecoder, Windows.Graphics, ContentType = WindowsRuntime]
$null = [Windows.Graphics.Imaging.SoftwareBitmap, Windows.Graphics, ContentType = WindowsRuntime]
$null = [Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType = WindowsRuntime]
$null = [Windows.Globalization.Language, Windows.Globalization, ContentType = WindowsRuntime]

# 把 Windows 运行时的异步操作转成 .NET 的 Task 再等它完成
$asTask = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
    $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1'
} | Select-Object -First 1
function Await($op, [Type]$type) {
    $task = $asTask.MakeGenericMethod($type).Invoke($null, @($op))
    $null = $task.Wait(-1)
    $task.Result
}

$langs = ([Windows.Media.Ocr.OcrEngine]::AvailableRecognizerLanguages | ForEach-Object { $_.LanguageTag }) -join '、'
Write-Output "系统里可用的识别语言：$langs；单张图片最大边长 $([Windows.Media.Ocr.OcrEngine]::MaxImageDimension) 像素"
$engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromLanguage([Windows.Globalization.Language]::new($Lang))
if ($null -eq $engine) { throw "系统里没有 $Lang 的识别语言" }

Get-ChildItem -LiteralPath $Dir -Filter *.bmp | Sort-Object Name | ForEach-Object {
    $file = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($_.FullName)) ([Windows.Storage.StorageFile])
    $stream = Await ($file.OpenAsync([Windows.Storage.FileAccessMode]::Read)) ([Windows.Storage.Streams.IRandomAccessStream])
    $decoder = Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($stream)) ([Windows.Graphics.Imaging.BitmapDecoder])
    $bitmap = Await ($decoder.GetSoftwareBitmapAsync()) ([Windows.Graphics.Imaging.SoftwareBitmap])
    $bitmap = [Windows.Graphics.Imaging.SoftwareBitmap]::Convert($bitmap, [Windows.Graphics.Imaging.BitmapPixelFormat]::Bgra8)
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $result = Await ($engine.RecognizeAsync($bitmap)) ([Windows.Media.Ocr.OcrResult])
    $ms = $sw.ElapsedMilliseconds
    $text = ($result.Lines | ForEach-Object { $_.Text }) -join "`n"
    $stream.Dispose()
    [pscustomobject]@{ file = $_.Name; ms = $ms; text = $text } | ConvertTo-Json -Compress
}
