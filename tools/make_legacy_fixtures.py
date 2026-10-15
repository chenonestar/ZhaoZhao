#!/usr/bin/env python3
"""生成老格式测试样本（.doc/.xls/.ppt/.rtf 以及同结构的 .wps/.et/.dps），提交到 testdata/legacy。
用法: python3 tools/make_legacy_fixtures.py testdata/legacy
加密的 .xls 样本另见 tools/make_encrypted_xls.py。
依赖: python-docx openpyxl python-pptx，以及 soffice。
样本内容尽量覆盖内置解析器要处理的结构：表格、页眉页脚、脚注、超链接域、多工作表、
日期与数字格式、公式（数字和文字结果）、批注、超过 8 KB 的共享字符串表（跨 CONTINUE 记录）、
幻灯片标题正文和备注、文本框。
"""
import datetime, os, shutil, subprocess, sys, tempfile

out = sys.argv[1]
os.makedirs(out, exist_ok=True)
work = tempfile.mkdtemp()
so = shutil.which("soffice") or shutil.which("libreoffice")
if not so:
    sys.exit("需要 soffice")

# ---- Word ----
import docx
from docx.oxml.ns import qn
from docx.oxml import OxmlElement

d = docx.Document()
d.add_heading("采购合同（老格式样本）", 1)
d.add_paragraph("甲方：北京找找科技有限公司，乙方：张三。违约金为合同总额的百分之五。")
d.add_paragraph("English words: invoice number HT-2025-0888, total 1,234.50 yuan.")
t = d.add_table(rows=2, cols=3)
for i, row in enumerate([("报销单号", "金额", "备注"), ("BX-20250101", "880.00", "差旅费")]):
    for j, v in enumerate(row):
        t.rows[i].cells[j].text = v
# 超链接（域）：只应保留显示文字，不应出现 HYPERLINK 域代码
p = d.add_paragraph("参见网站：")
h = OxmlElement("w:hyperlink")
h.set(qn("r:id"), d.part.relate_to("https://example.com/zz", "http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink", is_external=True))
r = OxmlElement("w:r"); tt = OxmlElement("w:t"); tt.text = "找找官方网站"; r.append(tt); h.append(r); p._p.append(h)
d.sections[0].header.paragraphs[0].text = "页眉机密文字"
d.sections[0].footer.paragraphs[0].text = "页脚第一页"
for i in range(1, 60):
    d.add_paragraph(f"第{i}条 条款内容：双方应当遵守本合同约定，履行各自义务。编号{i:04d}。")
d.add_paragraph("文末关键词：海獭")
d.save(f"{work}/合同.docx")

# 脚注：python-docx 不支持，用 LibreOffice 生成带脚注的 rtf 再转 doc 比较麻烦；改为直接写 fodt
fodt = """<?xml version="1.0" encoding="UTF-8"?>
<office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"
 xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"
 xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0" office:version="1.2" office:mimetype="application/vnd.oasis.opendocument.text">
<office:body><office:text>
<text:p>脚注样本正文，这里有一个脚注<text:note text:id="n1" text:note-class="footnote"><text:note-citation>1</text:note-citation><text:note-body><text:p>脚注里的关键词：鸬鹚</text:p></text:note-body></text:note>。</text:p>
<text:p>尾注样本<text:note text:id="n2" text:note-class="endnote"><text:note-citation>i</text:note-citation><text:note-body><text:p>尾注里的关键词：鹈鹕</text:p></text:note-body></text:note>。</text:p>
<text:p><draw:frame svg:width="5cm" svg:height="2cm" text:anchor-type="paragraph"><draw:text-box><text:p>文本框里的关键词：犰狳</text:p></draw:text-box></draw:frame>正文结束。</text:p>
</office:text></office:body></office:document>"""
open(f"{work}/脚注.fodt", "w", encoding="utf-8").write(fodt)

# ---- Excel ----
import openpyxl
from openpyxl.comments import Comment

wb = openpyxl.Workbook()
ws = wb.active
ws.title = "报销"
ws["A1"] = "姓名"; ws["B1"] = "金额"; ws["C1"] = "日期"; ws["D1"] = "合计"
ws["A2"] = "张三"; ws["B2"] = 1234.5; ws["C2"] = datetime.date(2023, 5, 1); ws["D2"] = "=B2*2"
ws["A3"] = "差旅费"; ws["B3"] = 42; ws["C3"] = datetime.datetime(2024, 12, 31, 18, 30)
ws["E2"] = '=CONCATENATE("公式","文字结果")'
ws["A2"].comment = Comment("请核对发票", "x")
ws2 = wb.create_sheet("第二页")
ws2["D4"] = "隐藏的关键词"
ws2.sheet_state = "hidden"
ws3 = wb.create_sheet("大量文字")
for i in range(3500):  # 共享字符串表超过 8 KB，必然跨 CONTINUE 记录
    ws3.cell(row=i + 1, column=1, value=f"共享字符串第{i}项：中文与English混排{i * 7}")
ws3.cell(row=3600, column=1, value="表格末尾关键词：穿山甲")
wb.save(f"{work}/报销.xlsx")

# ---- PowerPoint ----
from pptx import Presentation
from pptx.util import Inches

pr = Presentation()
s = pr.slides.add_slide(pr.slide_layouts[1])
s.shapes.title.text = "季度汇报"
s.placeholders[1].text = "营收增长百分之二十"
s.notes_slide.notes_text_frame.text = "讲稿备注：强调增长"
s2 = pr.slides.add_slide(pr.slide_layouts[5])
s2.shapes.title.text = "第二页标题"
tb = s2.shapes.add_textbox(Inches(1), Inches(2), Inches(5), Inches(1))
tb.text_frame.text = "文本框里的关键词：树袋熊"
pr.save(f"{work}/汇报.pptx")


def conv(src, fmt, dst):
    o = f"{work}/_o"
    subprocess.run([so, "--headless", "--convert-to", fmt, "--outdir", o, src], check=True, capture_output=True)
    base = os.path.splitext(os.path.basename(src))[0]
    shutil.move(f"{o}/{base}.{fmt.split(':')[0]}", dst)


conv(f"{work}/合同.docx", "doc", f"{out}/合同.doc")
conv(f"{work}/脚注.fodt", "doc", f"{out}/脚注.doc")
conv(f"{work}/报销.xlsx", "xls", f"{out}/报销.xls")
conv(f"{work}/汇报.pptx", "ppt", f"{out}/汇报.ppt")
conv(f"{work}/合同.docx", "rtf", f"{out}/合同.rtf")
# WPS 的 .wps/.et/.dps 与 doc/xls/ppt 结构相同，.doc 其实是 RTF 的情况：测试时复制生成，不另存样本
# 扩展名与实际格式不符的常见情况：.xls 其实是网页表格
open(f"{out}/其实是网页.xls", "w", encoding="utf-8").write(
    "<html><body><table><tr><td>网页表格里的关键词</td><td>獾</td></tr></table></body></html>")
# GBK 编码的 RTF（\\'hh 形式），手写
open(f"{out}/gbk.rtf", "wb").write(b"{\\rtf1\\ansi\\ansicpg936{\\fonttbl{\\f0\\fnil\\fcharset134 SimSun;}}\\f0 "
                                   + "".join("\\'%02x" % c for c in "国标码文字：麒麟".encode("gbk")).encode()
                                   + b"\\par Unicode \\u29482?\\u29483? end}")
shutil.rmtree(work, ignore_errors=True)
print("ok", sorted(os.listdir(out)))
