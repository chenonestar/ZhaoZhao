#!/usr/bin/env python3
"""生成加密的 docx/xlsx/pptx 测试样本（需要 LibreOffice、msoffcrypto-tool 和 cryptography）：
  默认密码.docx/.xlsx/.pptx  Agile 加密（Office 2010 以后的默认方式），默认密码 VelvetSweatshop，Excel 打开时不要密码
  标准加密.xlsx              Excel 2007 的标准加密（AES-128），同样用默认密码
  打开密码.xlsx              Agile 加密，真正的打开密码 my-own-pw
  权限管理.xlsx              模拟权限管理（IRM）：数据空间的名称带 DRM，只用来测试识别
用法: python3 tools/make_encrypted_ooxml.py testdata/encrypted
"""
import hashlib, io, os, struct, subprocess, sys, tempfile, zipfile
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from msoffcrypto.format.ooxml import OOXMLFile
from msoffcrypto.method.container.ecma376_encrypted import ECMA376Encrypted
import msoffcrypto

DEFAULT = "VelvetSweatshop"
out = os.path.abspath(sys.argv[1])
os.makedirs(out, exist_ok=True)
work = tempfile.mkdtemp()

NS = ('xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" '
      'xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" '
      'xmlns:presentation="urn:oasis:names:tc:opendocument:xmlns:presentation:1.0" '
      'xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0" office:version="1.2"')
sources = {
    "xlsx": ("in.fods", '<?xml version="1.0" encoding="UTF-8"?><office:document %s office:mimetype="application/vnd.oasis.opendocument.spreadsheet">'
             '<office:body><office:spreadsheet><table:table table:name="填报"><table:table-row>'
             '<table:table-cell office:value-type="string"><text:p>%%s</text:p></table:table-cell></table:table-row>'
             '</table:table></office:spreadsheet></office:body></office:document>' % NS),
    "docx": ("in.fodt", '<?xml version="1.0" encoding="UTF-8"?><office:document %s office:mimetype="application/vnd.oasis.opendocument.text">'
             '<office:body><office:text><text:p>%%s</text:p></office:text></office:body></office:document>' % NS),
    "pptx": ("in.fodp", '<?xml version="1.0" encoding="UTF-8"?><office:document %s office:mimetype="application/vnd.oasis.opendocument.presentation">'
             '<office:body><office:presentation><draw:page draw:name="p1"><draw:frame svg:x="2cm" svg:y="2cm" svg:width="20cm" svg:height="3cm">'
             '<draw:text-box><text:p>%%s</text:p></draw:text-box></draw:frame></draw:page></office:presentation></office:body></office:document>' % NS),
}


def make_plain(ext, text):
    name, tpl = sources[ext]
    d = tempfile.mkdtemp(dir=work)
    src = os.path.join(d, name)
    open(src, "w", encoding="utf-8").write(tpl % text)
    subprocess.run(["soffice", "--headless", "--norestore", "-env:UserInstallation=file://" + work + "/profile",
                    "--convert-to", ext, "--outdir", d, src], check=True, capture_output=True)
    return open(os.path.join(d, os.path.splitext(name)[0] + "." + ext), "rb").read()


def agile(plain, password):
    buf = io.BytesIO()
    OOXMLFile(io.BytesIO(plain)).encrypt(password, buf)
    return buf.getvalue()


def aes_ecb(key, data, enc=True):
    c = Cipher(algorithms.AES(key), modes.ECB())
    x = c.encryptor() if enc else c.decryptor()
    return x.update(data) + x.finalize()


def standard(plain, password):
    """Excel 2007 的标准加密（MS-OFFCRYPTO 2.3.4.5～2.3.4.9），AES-128、SHA-1。"""
    salt = os.urandom(16)
    h = hashlib.sha1(salt + password.encode("utf-16-le")).digest()
    for i in range(50000):
        h = hashlib.sha1(struct.pack("<I", i) + h).digest()
    h = hashlib.sha1(h + struct.pack("<I", 0)).digest()
    x1 = hashlib.sha1(bytes(b ^ 0x36 for b in h) + b"\x36" * 44).digest()
    x2 = hashlib.sha1(bytes(b ^ 0x5C for b in h) + b"\x5c" * 44).digest()
    key = (x1 + x2)[:16]
    verifier = os.urandom(16)
    vhash = hashlib.sha1(verifier).digest() + b"\x00" * 12
    csp = "Microsoft Enhanced RSA and AES Cryptographic Provider\0".encode("utf-16-le")
    header = struct.pack("<8I", 0x24, 0, 0x660E, 0x8004, 128, 0x18, 0, 0) + csp
    info = struct.pack("<HHII", 3, 2, 0x24, len(header)) + header
    info += struct.pack("<I", 16) + salt + aes_ecb(key, verifier) + struct.pack("<I", 20) + aes_ecb(key, vhash)
    padded = plain + b"\x00" * (-len(plain) % 16)
    pkg = struct.pack("<Q", len(plain)) + aes_ecb(key, padded)
    buf = io.BytesIO()
    ECMA376Encrypted(pkg, info).write_to(buf)
    return buf.getvalue()


def check(data, password, expect_ok=True):
    """用 msoffcrypto 独立解密一遍，确认样本本身是对的。"""
    f = msoffcrypto.OfficeFile(io.BytesIO(data))
    f.load_key(password=password)
    o = io.BytesIO()
    f.decrypt(o)
    assert zipfile.is_zipfile(io.BytesIO(o.getvalue()))


files = {}
for ext, text in [("xlsx", "默认密码表格的关键词：雪豹"), ("docx", "默认密码文档的关键词：藏羚羊"), ("pptx", "默认密码演示的关键词：白鳍豚")]:
    files["默认密码." + ext] = (agile(make_plain(ext, text), DEFAULT), DEFAULT)
files["标准加密.xlsx"] = (standard(make_plain("xlsx", "标准加密表格的关键词：金丝猴"), DEFAULT), DEFAULT)
files["打开密码.xlsx"] = (agile(make_plain("xlsx", "打开密码表格的关键词：大鲵"), "my-own-pw"), "my-own-pw")

for name, (data, pw) in files.items():
    check(data, pw)
    open(os.path.join(out, name), "wb").write(data)

# 权限管理：把数据空间的名称换成同样长度、带 DRM 的名称（目录项里的名称是 UTF-16LE）
data = files["默认密码.xlsx"][0]
old, new = "StrongEncryptionDataSpace".encode("utf-16-le"), "DRMEncryptedDataSpace0000".encode("utf-16-le")
assert old in data and len(old) == len(new)
open(os.path.join(out, "权限管理.xlsx"), "wb").write(data.replace(old, new))
print("ok:", ", ".join(sorted(files)) + ", 权限管理.xlsx")
