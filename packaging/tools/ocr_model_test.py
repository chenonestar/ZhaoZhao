"""OCR 认字模型检查：同一批模拟扫描页，比较发布包里各个认字模型（组件自带的 PP-OCRv4、打包时放入的
PP-OCRv6 small 和它的 INT8 量化版）的速度和准确率。直接驱动发布包里的 RapidOCR-json（和找找用的同一个组件、
同样的管道协议）。新模型是找找默认用的：组件加载不了，或者准确率低于 97% 时以非 0 退出，打包失败。

另外检查 Windows 自带的 OCR（找找里可选的「Windows 自带 OCR（实验）」）：用找找里的实现（packaging/tools/winocr，
直接调用 Windows.Media.Ocr）认同样的页面，记下速度和准确率；系统装了中文识别语言、准确率却低于 90% 时也以非 0 退出。

模拟扫描页：A4、200 DPI 的灰度 BMP（找找渲染 PDF 页后交给组件的格式），宋体随机常用字，加了模糊、噪点、轻微倾斜。
第 1 页同时存一份 PNG，确认组件读 BMP 和读 PNG 的结果一样。

用法：python ocr_model_test.py <组件目录 components\\ocr> <工作目录> [winocr.exe]
"""
import json
import os
import random
import subprocess
import sys
import time

import numpy as np
from PIL import Image, ImageDraw, ImageFilter, ImageFont

sys.stdout.reconfigure(encoding='utf-8')  # CI 的控制台默认是 cp1252，打印中文会出错
COMPONENT, WORK = sys.argv[1], sys.argv[2]
WINOCR = sys.argv[3] if len(sys.argv) > 3 else ''

PAGES, THREADS = 6, 2


def font(size=36):
    for name in ('simsun.ttc', 'msyh.ttc', 'simhei.ttf'):
        p = os.path.join(os.environ.get('WINDIR', r'C:\Windows'), 'Fonts', name)
        if os.path.exists(p):
            return ImageFont.truetype(p, size)
    raise SystemExit('没有中文字体')


def gb2312_level1():
    out = []
    for hi in range(0xB0, 0xD8):
        for lo in range(0xA1, 0xFF):
            try:
                out.append(bytes([hi, lo]).decode('gb2312'))
            except UnicodeDecodeError:
                pass
    return out


def make_pages(d, n=PAGES, size=36, prefix='page'):
    """size 是字的像素高：36 约 13 磅（200 DPI 的正文），18 约 6.5 磅（表格、图纸、脚注里的小字）。"""
    r = random.Random(1 if prefix == 'page' else 7)
    chars = gb2312_level1()
    weights = [5 if i < 500 else 1 for i in range(len(chars))]  # 常用字更常出现
    f = font(size)
    step = size * 62 // 36
    pages = []
    for p in range(n):
        img = Image.new('L', (1654, 2339), 255)
        dr = ImageDraw.Draw(img)
        lines, y = [], 160
        while y < 2339 - 200:
            s = ''.join(r.choices(chars, weights, k=r.randint(20, 34) * 36 // size))
            if r.random() < 0.5:
                k = r.randint(0, len(s))
                s = s[:k] + str(r.randint(1, 99999)) + s[k:]
            if r.random() < 0.6:
                k = r.randint(1, len(s) - 1)
                s = s[:k] + r.choice('，。、；：') + s[k:]
            dr.text((150, y), s, font=f, fill=0)
            lines.append(s)
            y += step
        img = img.rotate(0.4, resample=Image.BILINEAR, fillcolor=255).filter(ImageFilter.GaussianBlur(0.7))
        a = np.asarray(img).astype(np.int16) + np.random.default_rng(p).normal(0, 12, (2339, 1654)).astype(np.int16)
        img = Image.fromarray(np.clip(a, 0, 255).astype(np.uint8), 'L')
        path = os.path.join(d, f'{prefix}{p}.bmp')
        img.save(path)
        if p == 0:
            img.save(path[:-4] + '.png')
        pages.append((path, ''.join(lines)))
    return pages


def edit(a, b):
    prev = list(range(len(b) + 1))
    for i, ca in enumerate(a, 1):
        cur = [i] + [0] * len(b)
        for j, cb in enumerate(b, 1):
            cur[j] = min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (ca != cb))
        prev = cur
    return prev[-1]


def norm(s):
    s = ''.join(ch for ch in s if not ch.isspace())
    return s.replace(',', '，').replace(';', '；').replace(':', '：')


class Engine:
    def __init__(self, exe, args):
        self.p = subprocess.Popen([exe] + args + [f'--numThread={THREADS}'], cwd=os.path.dirname(exe),
                                  stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        t0 = time.time()
        while True:
            line = self.p.stdout.readline()
            if not line:
                err = self.p.stderr.read().decode('utf-8', 'replace')[-500:]
                raise RuntimeError(f'启动失败（退出码 {self.p.wait()}）：{err}')
            if b'init completed' in line:
                break
        self.init = time.time() - t0

    def ocr(self, path):
        req = json.dumps({'image_path': path.replace('\\', '/')}, ensure_ascii=True) + '\n'
        self.p.stdin.write(req.encode())
        self.p.stdin.flush()
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise RuntimeError('组件退出了')
            line = line.strip()
            if line.startswith(b'{'):
                break
        r = json.loads(line)
        if r.get('code') == 101:
            return ''
        if r.get('code') != 100:
            raise RuntimeError(f'识别失败：{line[:300]!r}')
        items = sorted(r['data'], key=lambda x: (x['box'][0][1] + x['box'][2][1]) / 2)
        return ''.join(x['text'] for x in items)

    def close(self):
        # 组件关了输入也不退出（找找也是直接结束它）
        self.p.kill()
        self.p.wait(timeout=30)


def read_args(d):
    with open(os.path.join(d, 'args.txt'), encoding='utf-8-sig') as f:
        return [l.strip() for l in f if l.strip() and not l.startswith('#')]


def run(name, exe, args, pages):
    """返回 (每页秒数, 字准确率%)；组件启动失败、识别出错时返回 None。"""
    try:
        e = Engine(exe, args)
    except Exception as ex:
        print(f'{name}：{ex}', flush=True)
        return None
    try:
        bmp0, png0 = e.ocr(pages[0][0]), e.ocr(pages[0][0][:-4] + '.png')  # 也当预热
        same = 'BMP、PNG 结果相同' if bmp0 == png0 else f'BMP、PNG 结果不同（{len(bmp0)} / {len(png0)} 字）'
        errs = total = 0
        t0 = time.perf_counter()
        for path, gt in pages:
            got = e.ocr(path)
            errs += edit(norm(got), norm(gt))
            total += len(norm(gt))
        per = (time.perf_counter() - t0) / len(pages)
        acc = 100 * (1 - errs / total)
        print(f'{name}：每页 {per:.2f} 秒（{THREADS} 线程），字准确率 {acc:.1f}%，加载 {e.init:.1f} 秒，{same}', flush=True)
        return per, acc
    except Exception as ex:
        print(f'{name}：{ex}', flush=True)
        return None
    finally:
        e.close()


def windows_ocr(pages):
    """Windows 自带的 OCR（找找里的实现，单线程逐页识别）。返回字准确率（%）；用不了时返回 None。"""
    if not WINOCR:
        return None
    d = os.path.dirname(pages[0][0])
    try:
        p = subprocess.run([WINOCR, d], capture_output=True, timeout=900)
    except Exception as ex:
        raise SystemExit(f'Windows 自带 OCR：没能运行（{ex}）')
    out = p.stdout.decode('utf-8', 'replace')
    if p.returncode == 3:
        print('Windows 自带 OCR：' + out.strip(), flush=True)
        return None
    if p.returncode != 0:
        raise SystemExit(f'Windows 自带 OCR：失败（退出码 {p.returncode}）：{p.stderr.decode("utf-8", "replace")[-800:]}')
    got = {}
    for line in out.splitlines():
        if line.strip().startswith('{'):
            r = json.loads(line)
            got[r['file']] = r
    errs = total = 0
    ms = []
    for path, gt in pages:
        r = got.get(os.path.basename(path))
        if r is None:
            continue
        errs += edit(norm(r['text']), norm(gt))
        total += len(norm(gt))
        ms.append(r['ms'])
    if not total:
        raise SystemExit('Windows 自带 OCR：没有结果')
    acc = 100 * (1 - errs / total)
    print(f'Windows 自带 OCR：每页 {sum(ms) / len(ms) / 1000:.2f} 秒（单线程），字准确率 {acc:.1f}%，共 {len(ms)} 页', flush=True)
    return acc


def main():
    os.makedirs(WORK, exist_ok=True)
    imgs = os.path.join(WORK, 'pages')
    os.makedirs(imgs, exist_ok=True)
    pages = make_pages(imgs)
    print(f'{len(pages)} 页模拟扫描页，平均每页 {sum(len(g) for _, g in pages) // len(pages)} 字', flush=True)
    exe = next(os.path.join(COMPONENT, n) for n in ('RapidOCR-json.exe', 'RapidOCR_json.exe')
               if os.path.exists(os.path.join(COMPONENT, n)))
    base = read_args(COMPONENT)
    swap = lambda rec, keys: [a for a in base if not a.startswith(('--rec=', '--keys='))] + [f'--rec={rec}', f'--keys={keys}']
    v4 = run('组件自带的 PP-OCRv4', exe, base, pages)
    v6 = run('PP-OCRv6 small', exe, swap('PP-OCRv6_rec_small.onnx', 'PP-OCRv6_keys.txt'), pages)
    v6q = run('PP-OCRv6 small 量化版', exe, swap('PP-OCRv6_rec_small_int8.onnx', 'PP-OCRv6_keys.txt'), pages)
    if v4 and v6:
        print(f'PP-OCRv6 small 比 PP-OCRv4 每页快 {100 * (1 - v6[0] / v4[0]):.0f}%', flush=True)
    if v6 and v6q:
        print(f'量化版比普通版每页快 {100 * (1 - v6q[0] / v6[0]):.0f}%', flush=True)
    # 大图找字（OCR 引擎对比里的「PP-OCRv6 small 大图找字」）：组件找文字前把图片长边缩到 1024 像素，A4 页上的小字
    # 可能找不到；改成 2048 看正文页慢多少、小字页多认出多少
    big = swap('PP-OCRv6_rec_small.onnx', 'PP-OCRv6_keys.txt') + ['--maxSideLen=2048']
    v6big = run('PP-OCRv6 small 大图找字', exe, big, pages)
    if v6 and v6big:
        print(f'大图找字比平常每页慢 {100 * (v6big[0] / v6[0] - 1):.0f}%', flush=True)
    small = os.path.join(WORK, 'small')
    os.makedirs(small, exist_ok=True)
    spages = make_pages(small, 2, 18, 'small')
    print(f'小字页（约 6.5 磅）{len(spages)} 页，平均每页 {sum(len(g) for _, g in spages) // len(spages)} 字', flush=True)
    run('PP-OCRv6 small 认小字页', exe, swap('PP-OCRv6_rec_small.onnx', 'PP-OCRv6_keys.txt'), spages)
    run('PP-OCRv6 small 大图找字认小字页', exe, big, spages)
    win = windows_ocr(pages)
    if not v6 or v6[1] < 97:
        raise SystemExit('新认字模型（找找默认用的）在组件里不能用或准确率太低')
    if win is not None and win < 90:
        raise SystemExit('Windows 自带 OCR（找找里的实现）准确率太低')


main()
