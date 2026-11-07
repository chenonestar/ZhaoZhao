#!/usr/bin/env python3
"""生成加密的 .xls 测试样本（需要 LibreOffice 和 Python-UNO，系统 python3 + python3-uno）：
  保护工作簿.xls  用 Excel「保护工作簿」的默认密码 VelvetSweatshop 加密，打开时不需要密码
  有打开密码.xls  用真正的打开密码加密
用法: /usr/bin/python3 tools/make_encrypted_xls.py testdata/legacy
"""
import os, subprocess, sys, tempfile, time
import uno
from com.sun.star.beans import PropertyValue

out = os.path.abspath(sys.argv[1])
work = tempfile.mkdtemp()

def prop(n, v):
    p = PropertyValue(); p.Name = n; p.Value = v; return p

# 先生成一个普通的工作簿（fods：LibreOffice 的单文件 XML 格式，手写最简单）
fods = """<?xml version="1.0" encoding="UTF-8"?>
<office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"
 xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:version="1.2" office:mimetype="application/vnd.oasis.opendocument.spreadsheet">
<office:body><office:spreadsheet><table:table table:name="预算">
<table:table-row><table:table-cell office:value-type="string"><text:p>%s</text:p></table:table-cell>
<table:table-cell office:value-type="float" office:value="8848"><text:p>8848</text:p></table:table-cell></table:table-row>
</table:table></office:spreadsheet></office:body></office:document>"""

pipe = "zzfixture%d" % os.getpid()
so = subprocess.Popen(["soffice", "--headless", "--invisible", "--norestore", "--nologo", "-env:UserInstallation=file://" + work + "/profile",
                       "--accept=pipe,name=%s;urp;StarOffice.ComponentContext" % pipe])
local = uno.getComponentContext()
resolver = local.ServiceManager.createInstanceWithContext("com.sun.star.bridge.UnoUrlResolver", local)
for _ in range(300):
    try:
        ctx = resolver.resolve("uno:pipe,name=%s;urp;StarOffice.ComponentContext" % pipe); break
    except Exception:
        time.sleep(0.2)
desktop = ctx.ServiceManager.createInstanceWithContext("com.sun.star.frame.Desktop", ctx)
for name, text, pw in [("保护工作簿.xls", "保护工作簿里的关键词：狐獴", "VelvetSweatshop"), ("有打开密码.xls", "有打开密码的关键词：穿山甲", "secret-123")]:
    src = os.path.join(work, "in.fods")
    open(src, "w", encoding="utf-8").write(fods % text)
    d = desktop.loadComponentFromURL(uno.systemPathToFileUrl(src), "_blank", 0, (prop("Hidden", True),))
    d.storeToURL(uno.systemPathToFileUrl(os.path.join(out, name)), (prop("FilterName", "MS Excel 97"), prop("Password", pw)))
    d.close(True)
try:
    desktop.terminate()
except Exception:
    pass
so.wait(30)
print("ok")
