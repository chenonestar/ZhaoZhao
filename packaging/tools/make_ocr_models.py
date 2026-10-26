"""打包时生成新认字模型，放进 OCR 组件的 models 目录（找找按文件名认，见 internal/extract/ocrmodel.go）：

- PP-OCRv6_rec_small.onnx：PP-OCRv6 small 认字模型，取自 rapidocr 的安装包。组件带的推理库较旧，不认新格式的
  模型（模型格式版本 10 时组件直接崩溃），所以标成旧格式（8）；模型本身用到的算子旧推理库都支持。
- PP-OCRv6_rec_small_int8.onnx：同一个模型的 INT8 静态量化版（卷积、矩阵乘的权重和中间结果换成 8 位整数）。
  用模拟的文字行图片（随机常用字、几种字体和字号、模糊、噪点）校准各层的取值范围。
- PP-OCRv6_keys.txt：字表，取自模型里的 character 元数据，每行一个字（组件自己加空白和空格两类）。

用法：python make_ocr_models.py <rapidocr 的 wheel 文件> <输出目录（组件的 models 目录）>
"""
import io
import math
import os
import random
import sys
import zipfile

import numpy as np
import onnx
from onnx import version_converter
from onnxruntime.quantization import CalibrationDataReader, CalibrationMethod, QuantFormat, QuantType, quantize_static
from onnxruntime.quantization.shape_inference import quant_pre_process
from PIL import Image, ImageDraw, ImageFilter, ImageFont

sys.stdout.reconfigure(encoding='utf-8')
WHEEL, OUT = sys.argv[1], sys.argv[2]
REC, REC_Q, KEYS = 'PP-OCRv6_rec_small.onnx', 'PP-OCRv6_rec_small_int8.onnx', 'PP-OCRv6_keys.txt'
CAL_LINES = 400


def fonts():
    out = []
    for name in ('simsun.ttc', 'msyh.ttc', 'simhei.ttf', 'simkai.ttf', 'simfang.ttf'):
        p = os.path.join(os.environ.get('WINDIR', r'C:\Windows'), 'Fonts', name)
        if os.path.exists(p):
            out.append(p)
    if not out:
        raise SystemExit('没有中文字体')
    return out


def gb2312_level1():
    out = []
    for hi in range(0xB0, 0xD8):
        for lo in range(0xA1, 0xFF):
            try:
                out.append(bytes([hi, lo]).decode('gb2312'))
            except UnicodeDecodeError:
                pass
    return out


def lines():
    """模拟扫描件上的文字行，按组件的做法缩放成 48 像素高、归一化到 [-1, 1]。"""
    r = random.Random(7)
    rng = np.random.default_rng(7)
    chars = gb2312_level1()
    weights = [5 if i < 500 else 1 for i in range(len(chars))]
    fs = fonts()
    for _ in range(CAL_LINES):
        s = ''.join(r.choices(chars, weights, k=r.randint(4, 30)))
        if r.random() < 0.5:
            k = r.randint(0, len(s))
            s = s[:k] + str(r.randint(1, 99999)) + s[k:]
        if r.random() < 0.5:
            k = r.randint(0, len(s))
            s = s[:k] + r.choice('，。、；：（）“”《》') + s[k:]
        font = ImageFont.truetype(r.choice(fs), r.randint(24, 48))
        x0, y0, x1, y1 = font.getbbox(s)
        pad = r.randint(4, 12)
        img = Image.new('L', (x1 - x0 + 2 * pad, y1 - y0 + 2 * pad), r.randint(215, 255))
        ImageDraw.Draw(img).text((pad - x0, pad - y0), s, font=font, fill=r.randint(0, 90))
        if r.random() < 0.7:
            img = img.filter(ImageFilter.GaussianBlur(r.uniform(0.3, 1.0)))
        a = np.asarray(img).astype(np.float32) + rng.normal(0, r.uniform(0, 14), (img.height, img.width))
        img = Image.fromarray(np.clip(a, 0, 255).astype(np.uint8), 'L')
        w = max(1, math.ceil(48 * img.width / img.height))
        img = img.resize((w, 48), Image.BILINEAR).convert('RGB')
        x = (np.asarray(img).astype(np.float32) / 255 - 0.5) / 0.5
        yield x.transpose(2, 0, 1)[np.newaxis].copy()


class Reader(CalibrationDataReader):
    def __init__(self, name):
        self.name, self.it = name, lines()

    def get_next(self):
        x = next(self.it, None)
        return None if x is None else {self.name: x}


def main():
    os.makedirs(OUT, exist_ok=True)
    with zipfile.ZipFile(WHEEL) as z:
        data = z.read('rapidocr/models/PP-OCRv6_rec_small.onnx')
    m = onnx.load_from_string(data)
    chars = dict((p.key, p.value) for p in m.metadata_props)['character'].split('\n')
    with open(os.path.join(OUT, KEYS), 'w', encoding='utf-8', newline='\n') as f:
        f.write('\n'.join(chars) + '\n')
    print(f'字表 {len(chars)} 个字；模型格式版本 {m.ir_version}，opset {m.opset_import[0].version}', flush=True)
    m.ir_version = 8
    onnx.save(m, os.path.join(OUT, REC))

    # 量化：按通道量化要 opset 13；先补全形状信息，不做推理库的图优化（优化后的图旧推理库可能不认）
    tmp = os.path.join(OUT, 'tmp-op13.onnx'), os.path.join(OUT, 'tmp-pre.onnx'), os.path.join(OUT, 'tmp-q.onnx')
    onnx.save(version_converter.convert_version(onnx.load_from_string(data), 13), tmp[0])
    quant_pre_process(tmp[0], tmp[1], skip_optimization=True, skip_symbolic_shape=True)
    quantize_static(tmp[1], tmp[2], Reader(m.graph.input[0].name), quant_format=QuantFormat.QDQ, per_channel=True,
                    activation_type=QuantType.QUInt8, weight_type=QuantType.QInt8, op_types_to_quantize=['Conv', 'MatMul'],
                    calibrate_method=CalibrationMethod.MinMax)
    q = onnx.load(tmp[2])
    q.ir_version = 8
    onnx.save(q, os.path.join(OUT, REC_Q))
    for p in tmp:
        os.remove(p)
    for n in (REC, REC_Q, KEYS):
        print(f'{n}：{os.path.getsize(os.path.join(OUT, n)) / 1048576:.1f} MB', flush=True)


main()
