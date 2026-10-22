"""OCR 认字模型对比（CI 实验，只测不换）：同一批模拟扫描页，比较现在的 PP-OCRv4 和 PP-OCRv6 small 认字模型
的速度和准确率。直接驱动发布包里的 RapidOCR-json（和找找用的同一个组件、同样的管道协议）。

模拟扫描页：A4、200 DPI 的灰度 BMP（找找渲染 PDF 页后交给组件的格式），宋体随机常用字，加了模糊、噪点、轻微倾斜。
第 1 页同时存一份 PNG，确认组件读 BMP 和读 PNG 的结果一样。

用法：python ocr_model_test.py <组件目录 components\\ocr> <rapidocr 的 wheel 文件> <工作目录>
"""
import json
import os
import random
import shutil
import subprocess
import sys
import time
import zipfile

import numpy as np
from PIL import Image, ImageDraw, ImageFilter, ImageFont

COMPONENT, WHEEL, WORK = sys.argv[1], sys.argv[2], sys.argv[3]
PAGES, THREADS = 6, 2


def font():
    for name in ('simsun.ttc', 'msyh.ttc', 'simhei.ttf'):
        p = os.path.join(os.environ.get('WINDIR', r'C:\Windows'), 'Fonts', name)
        if os.path.exists(p):
            return ImageFont.truetype(p, 36)
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


def make_pages(d):
    r = random.Random(1)
    chars = gb2312_level1()
    weights = [5 if i < 500 else 1 for i in range(len(chars))]  # 常用字更常出现
    f = font()
    pages = []
    for p in range(PAGES):
        img = Image.new('L', (1654, 2339), 255)
        dr = ImageDraw.Draw(img)
        lines, y = [], 160
        while y < 2339 - 200:
            s = ''.join(r.choices(chars, weights, k=r.randint(20, 34)))
            if r.random() < 0.5:
                k = r.randint(0, len(s))
                s = s[:k] + str(r.randint(1, 99999)) + s[k:]
            if r.random() < 0.6:
                k = r.randint(1, len(s) - 1)
                s = s[:k] + r.choice('，。、；：') + s[k:]
            dr.text((150, y), s, font=f, fill=0)
            lines.append(s)
            y += 62
        img = img.rotate(0.4, resample=Image.BILINEAR, fillcolor=255).filter(ImageFilter.GaussianBlur(0.7))
        a = np.asarray(img).astype(np.int16) + np.random.default_rng(p).normal(0, 12, (2339, 1654)).astype(np.int16)
        img = Image.fromarray(np.clip(a, 0, 255).astype(np.uint8), 'L')
        path = os.path.join(d, f'page{p}.bmp')
        img.save(path)
        if p == 0:
            img.save(os.path.join(d, 'page0.png'))
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
        self.p.stdin.close()
        self.p.wait(timeout=30)


def read_args(d):
    with open(os.path.join(d, 'args.txt'), encoding='utf-8-sig') as f:
        return [l.strip() for l in f if l.strip() and not l.startswith('#')]


def run(name, exe, args, pages):
    try:
        e = Engine(exe, args)
    except Exception as ex:
        print(f'{name}：{ex}', flush=True)
        return
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
        print(f'{name}：每页 {per:.2f} 秒（{THREADS} 线程），字准确率 {100 * (1 - errs / total):.1f}%，'
              f'加载 {e.init:.1f} 秒，{same}', flush=True)
    except Exception as ex:
        print(f'{name}：{ex}', flush=True)
    finally:
        e.close()


def main():
    os.makedirs(WORK, exist_ok=True)
    imgs = os.path.join(WORK, 'pages')
    os.makedirs(imgs, exist_ok=True)
    pages = make_pages(imgs)
    print(f'{len(pages)} 页模拟扫描页，平均每页 {sum(len(g) for _, g in pages) // len(pages)} 字', flush=True)
    exe = next(os.path.join(COMPONENT, n) for n in ('RapidOCR-json.exe', 'RapidOCR_json.exe')
               if os.path.exists(os.path.join(COMPONENT, n)))
    base = read_args(COMPONENT)
    run('现在的 PP-OCRv4', exe, base, pages)

    # PP-OCRv6 small 认字模型：从 rapidocr 的安装包里取，字表在模型里（每行一个字，组件自己加空白和空格）
    import onnx
    v6 = os.path.join(WORK, 'ocr-v6')
    shutil.rmtree(v6, ignore_errors=True)
    shutil.copytree(COMPONENT, v6)
    models = os.path.join(v6, 'models')
    with zipfile.ZipFile(WHEEL) as z:
        data = z.read('rapidocr/models/PP-OCRv6_rec_small.onnx')
    m = onnx.load_from_string(data)
    chars = dict((p.key, p.value) for p in m.metadata_props)['character'].split('\n')
    with open(os.path.join(models, 'v6_keys.txt'), 'w', encoding='utf-8', newline='\n') as f:
        f.write('\n'.join(chars) + '\n')
    with open(os.path.join(models, 'v6_rec.onnx'), 'wb') as f:
        f.write(data)
    m.ir_version = 8  # 组件带的推理库较旧时可能不认新格式的模型：另存一份标成旧格式
    onnx.save(m, os.path.join(models, 'v6_rec_ir8.onnx'))
    print(f'PP-OCRv6 small：字表 {len(chars)} 个字，模型格式版本 {onnx.load_from_string(data).ir_version}', flush=True)
    swap = lambda rec: [a for a in base if not a.startswith(('--rec=', '--keys='))] + [f'--rec={rec}', '--keys=v6_keys.txt']
    v6exe = os.path.join(v6, os.path.basename(exe))
    run('PP-OCRv6 small 认字', v6exe, swap('v6_rec.onnx'), pages)
    run('PP-OCRv6 small 认字（标成旧格式）', v6exe, swap('v6_rec_ir8.onnx'), pages)


main()
