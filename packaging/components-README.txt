找找 · 组件目录
================

找找由三部分组成：主程序、格式转换组件（体积较大）、OCR 组件。
没有放入组件时，对应的功能会降级，但不影响其他功能：

1. components\libreoffice\   格式转换组件（老格式 .doc/.xls/.ppt/.rtf 和 WPS 的 .wps/.et/.dps）
   - 把 LibreOffice 的便携版解压到这里，使 components\libreoffice\program\soffice.exe 存在。
   - 也可以直接安装 LibreOffice，找找会自动在 C:\Program Files\LibreOffice 里找到它。
   - 找不到时，老格式文件会出现在「失败列表」里（原因：未找到格式转换组件），装好后点「重试」即可。

2. components\ocr\           OCR 组件（扫描 PDF、图片）
   - 把 RapidOCR-json（PaddleOCR 模型的 ONNX 版本，CPU 离线推理）解压到这里，
     使 components\ocr\RapidOCR-json.exe 及其 models 目录存在。
   - 没有该组件时，图片只索引文件名，扫描 PDF 只索引文字层。
   - 也可以用环境变量 ZHAOZHAO_OCR 指定其他兼容命令（管道协议见 tools\ocr_bridge.py）。

所有组件都在本机离线运行，找找不联网。
