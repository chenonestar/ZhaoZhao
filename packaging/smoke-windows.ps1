# 完整版冒烟测试（在 Windows 上运行）：用打包好的目录真实跑一遍。
#   - 老格式：.doc 用内置解析器；内置解析不了的（SYLK 格式的 .xls）排到转换队列，经随包的常驻 LibreOffice 转换
#   - 图片 OCR（经随包的 RapidOCR-json）
#   - 压缩包内文件
# 故意把 TEMP 设成含中文和空格的路径，验证中文用户名场景（LibreOffice 配置目录 URL、OCR 请求路径转义）。
param([string]$Stage = "stage", [int]$Port = 17895)
$ErrorActionPreference = "Stop"
$base = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { $env:TEMP }
$root = Join-Path $base "冒烟 测试"
$samples = Join-Path $root "样本目录"
$data = Join-Path $root "data"
$tmp = Join-Path $root "临时 目录"
Remove-Item $root -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $samples, $data, $tmp | Out-Null
$env:TEMP = $tmp; $env:TMP = $tmp
$utf8bom = [Text.UTF8Encoding]::new($true)
$utf8 = [Text.UTF8Encoding]::new($false)

Write-Host "== 生成样本"
[IO.File]::WriteAllText("$samples\简单.txt", "这个文件里有违约金 smoketext", $utf8)
[IO.File]::WriteAllText("$root\src.txt", "采购合同老格式条款 legacydoc", $utf8bom)
$soffice = Join-Path (Resolve-Path $Stage) "components\libreoffice\program\soffice.exe"
& $soffice --headless --convert-to doc --outdir $samples "$root\src.txt" | Out-Host
Move-Item "$samples\src.doc" "$samples\老格式.doc"
# 内置解析器不认识的格式（SYLK），扩展名却是 .xls：应转交 LibreOffice
[IO.File]::WriteAllText("$samples\符号链接格式.xls", "ID;PWXL;N;E`r`nC;Y1;X1;K`"sylkcell`"`r`nE`r`n", $utf8)

Add-Type -AssemblyName System.Drawing
$bmp = New-Object Drawing.Bitmap 900, 260
$g = [Drawing.Graphics]::FromImage($bmp)
$g.Clear([Drawing.Color]::White)
$g.TextRenderingHint = [Drawing.Text.TextRenderingHint]::AntiAlias
$font = New-Object Drawing.Font("Microsoft YaHei", 30)
$g.DrawString("合同编号 HT-2025-0888 已盖章", $font, [Drawing.Brushes]::Black, 30, 30)
$g.DrawString("违约金为合同总额的百分之五", $font, [Drawing.Brushes]::Black, 30, 110)
$bmp.Save("$samples\扫描件.png", [Drawing.Imaging.ImageFormat]::Png)
$g.Dispose(); $bmp.Dispose()

[IO.File]::WriteAllText("$root\inner.txt", "压缩包里的内容 zipinner", $utf8)
Compress-Archive -Path "$root\inner.txt" -DestinationPath "$samples\归档.zip"
Get-ChildItem $samples | Format-Table Name, Length | Out-Host

Write-Host "== 启动找找"
$proc = Start-Process "$Stage\zhaozhao.exe" -ArgumentList @("-data", "`"$data`"", "-port", "$Port", "-no-ui", "-add", "`"$samples`"") -PassThru
$fail = New-Object System.Collections.Generic.List[string]
try {
    $inst = Join-Path $data "instance.json"
    for ($i = 0; $i -lt 100 -and -not (Test-Path $inst); $i++) { Start-Sleep -Milliseconds 200 }
    if (-not (Test-Path $inst)) { throw "找找没有启动（没有生成 instance.json）" }
    $tok = (Get-Content $inst -Raw | ConvertFrom-Json).token
    $sess = New-Object Microsoft.PowerShell.Commands.WebRequestSession
    $sess.Cookies.Add((New-Object Net.Cookie("zz", $tok, "/", "127.0.0.1")))
    $api = "http://127.0.0.1:$Port"

    Write-Host "== 等待索引完成"
    $deadline = (Get-Date).AddMinutes(8)
    while ($true) {
        $st = Invoke-RestMethod "$api/api/status" -WebSession $sess
        Write-Host ("  总数={0} 待处理={1} 失败={2} 扫描中={3} 正在处理={4}" -f $st.totals.Total, $st.totals.Pending, $st.totals.Failed, $st.scanning, (($st.current | ForEach-Object { "$($_.kind):$($_.stage)" }) -join ";"))
        if (-not $st.scanning -and $st.totals.Total -ge 5 -and $st.totals.Pending -eq 0 -and $st.current.Count -eq 0 -and -not $st.componentsChecking) { break }
        if ((Get-Date) -gt $deadline) { $fail.Add("索引超时未完成"); break }
        Start-Sleep -Seconds 3
    }

    function Search($q) {
        (Invoke-RestMethod ("$api/api/search?q=" + [uri]::EscapeDataString($q)) -WebSession $sess).results
    }
    function Expect($label, $q, $pattern) {
        $r = Search $q
        $hit = $r | Where-Object { $_.path -like $pattern }
        if ($hit) { Write-Host "  通过: $label（$q → $($hit[0].path)）" }
        else { $fail.Add("$label：搜索「$q」没有命中 $pattern；实际结果：" + (($r | ForEach-Object { $_.path }) -join " | ")) }
    }
    Write-Host "== 搜索验证"
    Expect "文本（含中文路径）" "smoketext" "*简单.txt"
    Expect "老格式 .doc（内置解析）" "legacydoc" "*老格式.doc"
    Expect "内置解析不了的老格式（常驻 LibreOffice 转换）" "sylkcell" "*符号链接格式.xls"
    Expect "图片 OCR（英文数字）" "HT-2025-0888" "*扫描件.png"
    Expect "图片 OCR（中文）" "违约金" "*扫描件.png"
    Expect "压缩包内文件" "zipinner" "*归档.zip*inner.txt"
    $ocr = Search "HT-2025-0888" | Where-Object { $_.path -like "*扫描件.png" }
    if ($ocr -and -not $ocr[0].ocr) { $fail.Add("OCR 结果应带 OCR 标记") }

    Write-Host "== 组件自检"
    foreach ($c in $st.components) {
        Write-Host "  $($c.name)：$($c.message)"
        if (-not $c.ok) { $fail.Add("组件自检失败：$($c.name)：$($c.message)") }
    }
    if (@($st.components).Count -lt 2) { $fail.Add("状态里缺少组件自检结果") }

    Write-Host "== 失败列表"
    Invoke-RestMethod "$api/api/failed" -WebSession $sess | ForEach-Object { Write-Host "  $($_.path)  $($_.reason)" }

    Write-Host "== 退出（与托盘「退出」相同的路径）"
    Invoke-RestMethod "$api/api/quit" -Method Post -WebSession $sess -Headers @{ "X-ZhaoZhao" = "1" } -ContentType "application/json" -Body "{}" | Out-Null
    if (-not $proc.WaitForExit(15000)) { $fail.Add("调用退出后 15 秒内没有退出") }
    else { Write-Host "  主程序已退出" }
    Start-Sleep -Seconds 2
    $left = Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Name -match '^(zhaozhao|soffice|RapidOCR)' -or ($_.Name -match '^python' -and $_.Path -like "*components*") }
    if ($left) { $fail.Add("退出后仍有残留进程：" + (($left | ForEach-Object { "$($_.Name)($($_.Id))" }) -join ", ")) }
    else { Write-Host "  没有残留进程" }
}
catch { $fail.Add("异常: $_") }
finally {
    if ($proc -and -not $proc.HasExited) { taskkill /T /F /PID $proc.Id | Out-Null }   # 连同解析子进程和 OCR 子进程
    Start-Sleep -Seconds 2
    $log = Join-Path $data "zhaozhao.log"
    if (Test-Path $log) {
        Write-Host "== 日志（末尾 40 行）"; Get-Content $log -Tail 40
        $text = Get-Content $log -Raw -Encoding UTF8
        if ($text -notmatch "常驻 LibreOffice 已启动") { $fail.Add("没有使用常驻 LibreOffice（日志里没有「常驻 LibreOffice 已启动」）") }
        if ($text -match "常驻 LibreOffice 不可用") { $fail.Add("常驻 LibreOffice 不可用：" + (($text -split "`n") -match "常驻 LibreOffice 不可用" | Select-Object -First 1)) }
    }
}
if ($fail.Count -gt 0) {
    Write-Host "`n冒烟测试失败："
    $fail | ForEach-Object { Write-Host " - $_" }
    exit 1
}
Write-Host "`n冒烟测试全部通过"
