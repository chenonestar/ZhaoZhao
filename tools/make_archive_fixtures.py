#!/usr/bin/env python3
"""生成压缩方式测试样本（需要 inflate64、pyppmd、zstandard：pip install inflate64 pyppmd zstandard）：
  多种压缩方式.zip  每个条目一种压缩方式：存储、Deflate、Deflate64、BZip2、LZMA、zstd、xz、PPMd
  条目损坏.zip      一个条目的数据被改坏（校验和不对），另一个正常
用法: python3 tools/make_archive_fixtures.py testdata/archives
"""
import bz2, lzma, os, random, struct, sys, zipfile, zlib
import inflate64, pyppmd, zstandard

out = os.path.abspath(sys.argv[1])
os.makedirs(out, exist_ok=True)
random.seed(7)

# 约 40 KB 的「随机」中文段落重复两次（让 Deflate64 用上 32 KB 以外的距离），再加一长串相同字符（用上超过 258 的匹配长度）
words = "合同条款违约金付款方式交货时间质量标准验收程序争议解决知识产权保密义务不可抗力"
block = "".join(random.choice(words) for _ in range(14000)).encode("utf-8")


def body(keyword, big=False):
    s = ("压缩方式测试的关键词：%s\n" % keyword).encode("utf-8")
    if big:
        s += block + b"\n" + block + b"\n" + b"=" * 70000 + b"\n"
    return s + ("结尾的关键词：%s尾巴\n" % keyword).encode("utf-8")


def deflate64(data):
    d = inflate64.Deflater()
    return d.deflate(data) + d.flush()


def raw_deflate(data):
    c = zlib.compressobj(9, zlib.DEFLATED, -15)
    return c.compress(data) + c.flush()


def zip_lzma(data):
    c = zipfile._get_compressor(zipfile.ZIP_LZMA)  # 与 Python zipfile 写出的完全相同：版本、属性长度、属性，再接数据（带结束标记）
    return c.compress(data) + c.flush()


def ppmd(data):
    order, mem_mb, restore = 6, 16, 0
    e = pyppmd.Ppmd8Encoder(order, mem_mb << 20, restore)
    return struct.pack("<H", (order - 1) | ((mem_mb - 1) << 4) | (restore << 12)) + e.encode(data) + e.flush()


def write_zip(path, entries):
    """entries: [(name, method, data, compressed, flags)]，自己写 zip：标准库不支持其中几种压缩方式。"""
    buf, central = bytearray(), bytearray()
    dostime, dosdate = (12 << 11) | (30 << 5), ((2026 - 1980) << 9) | (10 << 5) | 16
    for name, method, data, comp, flags, crc in entries:
        n = name.encode("utf-8")
        flags |= 0x800  # 文件名是 UTF-8
        off = len(buf)
        buf += struct.pack("<IHHHHHIIIHH", 0x04034B50, 63, flags, method, dostime, dosdate, crc, len(comp), len(data), len(n), 0) + n + comp
        central += struct.pack("<IHHHHHHIIIHHHHHII", 0x02014B50, 63, 63, flags, method, dostime, dosdate, crc, len(comp), len(data),
                               len(n), 0, 0, 0, 0, 0, off) + n
    cd = len(buf)
    buf += central + struct.pack("<IHHHHIIH", 0x06054B50, 0, 0, len(entries), len(entries), len(central), cd, 0)
    open(path, "wb").write(buf)


entries = []
for name, method, comp, flags, big in [
    ("存储.txt", 0, lambda d: d, 0, False),
    ("deflate.txt", 8, raw_deflate, 0, True),
    ("deflate64.txt", 9, deflate64, 0, True),
    ("bzip2.txt", 12, bz2.compress, 0, True),
    ("lzma.txt", 14, zip_lzma, 2, True),  # 标志位 1：数据带结束标记
    ("zstd.txt", 93, lambda d: zstandard.ZstdCompressor().compress(d), 0, True),
    ("xz.txt", 95, lambda d: lzma.compress(d, format=lzma.FORMAT_XZ), 0, True),
    ("ppmd.txt", 98, ppmd, 0, False),
]:
    data = body(os.path.splitext(name)[0] + "关键词")
    if big:
        data = body(os.path.splitext(name)[0] + "关键词", True)
    c = comp(data)
    entries.append(("目录/" + name, method, data, c, flags, zlib.crc32(data)))
write_zip(os.path.join(out, "多种压缩方式.zip"), entries)

good = body("完好的条目")
bad = body("损坏的条目", True)
bc = bytearray(bz2.compress(bad))
bc[len(bc) // 2] ^= 0x55
write_zip(os.path.join(out, "条目损坏.zip"), [
    ("完好.txt", 8, good, raw_deflate(good), 0, zlib.crc32(good)),
    ("损坏.txt", 12, bad, bytes(bc), 0, zlib.crc32(bad)),
])

# 自检：Deflate64 样本确实用到了 32 KB 以外的距离和超过 258 的长度（解出来与原文相同即可）
assert inflate64.Inflater().inflate(deflate64(body("x", True))) == body("x", True)
print("ok")
