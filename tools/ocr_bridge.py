#!/usr/bin/env python3
"""用 rapidocr-onnxruntime 模拟 RapidOCR-json 的管道协议，供开发和测试使用。

协议（与发行包里的 RapidOCR-json.exe 一致）：
  启动完成后在标准输出打印一行 "OCR init completed."
  每行标准输入是 {"image_path": "..."}；每行标准输出是
  {"code": 100, "data": [{"text": "...", "score": 0.9, "box": [[x,y],...]}]}
  code 100 成功，101 没有识别到文字，其他为错误（data 为错误信息）。
"""
import json, sys

from rapidocr_onnxruntime import RapidOCR

# 与 RapidOCR-json 一样接受 --numThread=N（每个进程的线程数）
threads = [int(a.split("=", 1)[1]) for a in sys.argv[1:] if a.startswith("--numThread=")]
engine = RapidOCR(intra_op_num_threads=threads[-1]) if threads else RapidOCR()
sys.stdout.write("OCR init completed.\n")
sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        result, _ = engine(req["image_path"])
        if not result:
            out = {"code": 101, "data": "No text found in image."}
        else:
            out = {"code": 100, "data": [{"text": t, "score": float(s), "box": b} for b, t, s in result]}
    except Exception as e:  # noqa: BLE001
        out = {"code": 902, "data": str(e)}
    sys.stdout.write(json.dumps(out, ensure_ascii=False) + "\n")
    sys.stdout.flush()
