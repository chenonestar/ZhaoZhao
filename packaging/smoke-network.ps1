# 网络盘冒烟测试（Windows）：把一个文件夹共享出来并映射成盘符，放入几百个混合文件，
# 用默认设置建立索引，验证：
#   - 全部处理完、不会卡住（v0.1.2 在这种条件下会死锁）；
#   - 磁盘被识别为网络盘，扫描版 PDF 逐页 OCR，老格式转换、压缩包都正常；
#   - 日志里没有「疑似卡住」，诊断信息可以导出且能隐藏路径；
#   - 退出后没有残留进程。
param([string]$Stage = "stage", [string]$MkScanPdf = "mkscanpdf.exe", [int]$Port = 17896, [string]$Drive = "Z")
$ErrorActionPreference = "Stop"
$base = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { $env:TEMP }
$root = Join-Path $base "网络盘测试"
$share = Join-Path $root "共享文件夹"
$data = Join-Path $root "data"
$pages = Join-Path $root "页面"
Remove-Item $root -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $share, $data, $pages | Out-Null
$utf8 = [Text.UTF8Encoding]::new($false)
$fail = New-Object System.Collections.Generic.List[string]

Write-Host "== 共享并映射为 ${Drive}:"
New-SmbShare -Name zzsmoke -Path $share -FullAccess Everyone | Out-Null
net use "${Drive}:" \\localhost\zzsmoke /persistent:no | Out-Host
if (-not (Test-Path "${Drive}:\")) { throw "映射网络盘失败" }
$net = "${Drive}:\合同资料"
New-Item -ItemType Directory -Force $net, "$net\扫描件", "$net\老文档", "$net\图片" | Out-Null

Write-Host "== 生成样本"
for ($i = 1; $i -le 200; $i++) {
    [IO.File]::WriteAllText("$net\合同$i.txt", "第 $i 号采购合同 违约金条款 nettext$i", $utf8)
}
# 老格式：一次转换 20 个
$src = Join-Path $root "doc源"
New-Item -ItemType Directory -Force $src | Out-Null
for ($i = 1; $i -le 20; $i++) {
    [IO.File]::WriteAllText("$src\老文档$i.txt", "老格式文档 legacynet$i", [Text.UTF8Encoding]::new($true))
}
$soffice = Join-Path (Resolve-Path $Stage) "components\libreoffice\program\soffice.exe"
& $soffice --headless --convert-to doc --outdir "$net\老文档" (Get-ChildItem $src -Filter *.txt | ForEach-Object FullName) | Out-Null
# 图片和扫描页：用微软雅黑写字
Add-Type -AssemblyName System.Drawing
function New-TextImage($path, [string[]]$lines) {
    $bmp = New-Object Drawing.Bitmap 1000, 700
    $g = [Drawing.Graphics]::FromImage($bmp)
    $g.Clear([Drawing.Color]::White)
    $g.TextRenderingHint = [Drawing.Text.TextRenderingHint]::AntiAlias
    $font = New-Object Drawing.Font("Microsoft YaHei", 32)
    $y = 60
    foreach ($l in $lines) { $g.DrawString($l, $font, [Drawing.Brushes]::Black, 40, $y); $y += 90 }
    $bmp.Save($path, [Drawing.Imaging.ImageFormat]::Png)
    $g.Dispose(); $bmp.Dispose()
}
for ($i = 1; $i -le 40; $i++) { New-TextImage "$net\图片\照片$i.png" @("网络盘图片 第 $i 张", "IMGNET$i") }
for ($k = 1; $k -le 8; $k++) {
    $files = @()
    for ($p = 1; $p -le 8; $p++) {
        $f = Join-Path $pages "s${k}p$p.png"
        New-TextImage $f @("扫描合同 $k 第 $p 页", "SCANNET${k}X$p")
        $files += $f
    }
    & $MkScanPdf "$net\扫描件\扫描合同$k.pdf" @files
    if ($LASTEXITCODE -ne 0) { throw "生成扫描 PDF 失败" }
}
[IO.File]::WriteAllText("$root\inner.txt", "压缩包里的内容 zipnet", $utf8)
Compress-Archive -Path "$root\inner.txt" -DestinationPath "$net\归档.zip"
$total = (Get-ChildItem $net -Recurse -File).Count
Write-Host "  共 $total 个文件"

Write-Host "== 启动找找（默认设置）"
$proc = Start-Process "$Stage\zhaozhao.exe" -ArgumentList @("-data", "`"$data`"", "-port", "$Port", "-no-ui", "-add", "`"$net`"") -PassThru
try {
    $inst = Join-Path $data "instance.json"
    for ($i = 0; $i -lt 100 -and -not (Test-Path $inst); $i++) { Start-Sleep -Milliseconds 200 }
    if (-not (Test-Path $inst)) { throw "找找没有启动" }
    $tok = (Get-Content $inst -Raw | ConvertFrom-Json).token
    $sess = New-Object Microsoft.PowerShell.Commands.WebRequestSession
    $sess.Cookies.Add((New-Object Net.Cookie("zz", $tok, "/", "127.0.0.1")))
    $api = "http://127.0.0.1:$Port"

    Write-Host "== 等待索引完成（最多 20 分钟）"
    $deadline = (Get-Date).AddMinutes(20)
    $last = -1; $lastChange = Get-Date
    while ($true) {
        $st = Invoke-RestMethod "$api/api/status" -WebSession $sess
        $cur = ($st.current | ForEach-Object { "$($_.kind):$($_.stage) $($_.progress)" }) -join "; "
        Write-Host ("  总数={0} 已提取={1} 待处理={2} 失败={3} OCR队列={4} 停滞={5}s 处理中: {6}" -f $st.totals.Total, $st.totals.Done, $st.totals.Pending, $st.totals.Failed, $st.ocrQueue, $st.stalledSec, $cur)
        if ($st.totals.Pending -ne $last) { $last = $st.totals.Pending; $lastChange = Get-Date }
        if (-not $st.scanning -and $st.totals.Total -ge $total -and $st.totals.Pending -eq 0 -and $st.current.Count -eq 0) { break }
        if ($st.stalledSec -gt 0) { $fail.Add("状态显示疑似卡住 $($st.stalledSec) 秒"); break }
        if (((Get-Date) - $lastChange).TotalMinutes -gt 6) { $fail.Add("6 分钟没有任何进展（待处理 $last）"); break }
        if ((Get-Date) -gt $deadline) { $fail.Add("20 分钟内没有完成"); break }
        Start-Sleep -Seconds 5
    }
    $netVol = $st.volumes | Where-Object { $_.kind -eq "network" }
    if (-not $netVol) { $fail.Add("${Drive}: 没有被识别为网络盘：" + (($st.volumes | ForEach-Object { "$($_.mount)=$($_.kind)" }) -join ", ")) }

    function Expect($label, $q, $pattern) {
        $r = (Invoke-RestMethod ("$api/api/search?q=" + [uri]::EscapeDataString($q)) -WebSession $sess).results
        $hit = $r | Where-Object { $_.path -like $pattern }
        if ($hit) { Write-Host "  通过: $label" } else { $fail.Add("$label：搜索「$q」没有命中 $pattern") }
    }
    Write-Host "== 搜索验证"
    Expect "网络盘文本" "nettext137" "*合同137.txt"
    Expect "网络盘老格式 .doc" "legacynet7" "*老文档7.doc"
    Expect "网络盘图片 OCR" "IMGNET23" "*照片23.png"
    Expect "扫描 PDF 第 6 页 OCR" "SCANNET5X6" "*扫描合同5.pdf"
    Expect "压缩包" "zipnet" "*归档.zip*"

    Write-Host "== 失败列表"
    # 空数组时 Invoke-RestMethod 仍会输出一个空元素：只统计有路径的条目
    @(Invoke-RestMethod "$api/api/failed" -WebSession $sess) | Where-Object { $_ -and $_.path } | ForEach-Object { Write-Host "  $($_.path)  $($_.reason)"; $fail.Add("失败: $($_.path) $($_.reason)") }

    Write-Host "== 导出诊断信息（隐藏路径）"
    # 0.2.4 起在后台导出：开始后查询进度，直到完成（最多 2 分钟）
    $d = Invoke-RestMethod "$api/api/diagnostics" -Method Post -WebSession $sess -Headers @{ "X-ZhaoZhao" = "1" } -ContentType "application/json" -Body '{"anonymize":true}'
    $steps = @()
    $until = (Get-Date).AddMinutes(2)
    while ($d.running -and (Get-Date) -lt $until) {
        if ($d.step -and $steps -notcontains $d.step) { $steps += $d.step; Write-Host "  进度：$($d.step)" }
        Start-Sleep -Milliseconds 300
        $d = Invoke-RestMethod "$api/api/diagnostics/status" -WebSession $sess
    }
    if ($d.running) { $fail.Add("导出诊断信息超过 2 分钟还没完成（$($d.step)）") }
    elseif ($d.err) { $fail.Add("导出诊断信息失败：$($d.err)") }
    elseif (-not $d.path -or -not (Test-Path $d.path)) { $fail.Add("诊断信息没有生成") }
    else {
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $z = [IO.Compression.ZipFile]::OpenRead($d.path)
        $text = ($z.Entries | ForEach-Object { (New-Object IO.StreamReader($_.Open())).ReadToEnd() }) -join "`n"
        $z.Dispose()
        foreach ($leak in @("合同资料", "扫描合同", "老文档", "照片")) {
            if ($text.Contains($leak)) { $fail.Add("诊断信息里没有隐藏「$leak」") }
        }
        if ($text -notmatch "扫描完成") { $fail.Add("诊断信息里没有日志") }
        Write-Host "  $($d.path)"
    }

    Write-Host "== 退出"
    Invoke-RestMethod "$api/api/quit" -Method Post -WebSession $sess -Headers @{ "X-ZhaoZhao" = "1" } -ContentType "application/json" -Body "{}" | Out-Null
    if (-not $proc.WaitForExit(15000)) { $fail.Add("调用退出后 15 秒内没有退出") }
    Start-Sleep -Seconds 2
    $left = Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Name -match '^(zhaozhao|soffice|RapidOCR)' }
    if ($left) { $fail.Add("退出后仍有残留进程：" + (($left | ForEach-Object { "$($_.Name)($($_.Id))" }) -join ", ")) }
}
catch { $fail.Add("异常: $_") }
finally {
    if ($proc -and -not $proc.HasExited) { taskkill /T /F /PID $proc.Id | Out-Null }
    $log = Join-Path $data "zhaozhao.log"
    if (Test-Path $log) {
        $all = Get-Content $log
        if ($all -match "疑似卡住") { $fail.Add("日志里有「疑似卡住」") }
        Write-Host "== 日志统计：共 $($all.Count) 行，$([math]::Round((Get-Item $log).Length / 1KB)) KB"
        Write-Host "== 日志（重要事件）"
        $all | Where-Object { $_ -match "启动：|设置：|并发设置|扫描完成|失败|卡住|超时|退出|组件自检" } | Select-Object -First 60 | ForEach-Object { Write-Host $_ }
    }
    net use "${Drive}:" /delete /y | Out-Null
    Remove-SmbShare -Name zzsmoke -Force -ErrorAction SilentlyContinue
}
if ($fail.Count -gt 0) {
    Write-Host "`n网络盘冒烟测试失败："
    $fail | ForEach-Object { Write-Host " - $_" }
    exit 1
}
Write-Host "`n网络盘冒烟测试全部通过"
