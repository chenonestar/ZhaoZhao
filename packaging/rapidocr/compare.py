"""比较原版 RapidOCR-json 和找找自己编译的（patch.py）：认出的字是否一样、线程数是否真的按 --numThread、
多个进程同时识别时每分钟多少页、每张图单独指定 max_side_len 是否生效。

用法：python compare.py <原版组件目录> <修好的 exe> <样张图片>
组件目录里要有 RapidOCR-json.exe、models 和 args.txt（和找找用的一样）。修好的 exe 和原版用同一份模型。
认出的字不一样、修好的版本启动不了或不认 max_side_len 时退出码为 1。
"""
import json
import os
import pathlib
import subprocess
import sys
import threading
import time

import psutil

comp, fixed, sample = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), pathlib.Path(sys.argv[3])
orig = next(p for p in (comp / "RapidOCR-json.exe", comp / "RapidOCR_json.exe") if p.exists())
args = [l.strip() for l in (comp / "args.txt").read_text(encoding="utf-8-sig").splitlines() if l.strip() and not l.startswith("#")]


class OCR:
    def __init__(self, exe, threads):
        self.p = subprocess.Popen([str(exe), *args, f"--numThread={threads}"], cwd=str(comp), stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise RuntimeError(f"{exe.name} 启动失败")
            if b"init completed" in line:
                break

    def run(self, img, side=0):
        req = {"image_path": str(img).replace("\\", "/")}
        if side:
            req["max_side_len"] = side
        self.p.stdin.write((json.dumps(req) + "\n").encode())
        self.p.stdin.flush()
        r = json.loads(self.p.stdout.readline())
        if r.get("code") not in (100, 101):
            raise RuntimeError(f"识别失败：{r}")
        return r.get("data") or []

    def close(self):
        self.p.stdin.close()
        self.p.wait(timeout=30)


def text(data):
    return "".join(d["text"] for d in data)


bad = False
print(f"逻辑处理器 {psutil.cpu_count()} 个，物理核心 {psutil.cpu_count(logical=False)} 个")

# 1. 认出的字一样
a, b = OCR(orig, 1), OCR(fixed, 1)
ta, tb = text(a.run(sample)), text(b.run(sample))
same = sum(x == y for x, y in zip(ta, tb)) / max(len(ta), len(tb), 1)
print(f"认出的字：原版 {len(ta)} 字，修好的 {len(tb)} 字，逐字相同 {same:.1%}")
if same < 0.99:
    bad = True
    print("!! 认出的字不一样")

# 2. 每张图单独指定 max_side_len：大图找字框更细，用时更长；不指定时和启动参数一样
t0 = time.time(); d1 = b.run(sample); t1 = time.time() - t0
t0 = time.time(); d2 = b.run(sample, 2048); t2 = time.time() - t0
t0 = time.time(); d3 = b.run(sample); t3 = time.time() - t0
print(f"max_side_len：不指定 {t1:.2f} 秒 {len(d1)} 个框；2048 {t2:.2f} 秒 {len(d2)} 个框；再不指定 {t3:.2f} 秒 {len(d3)} 个框")
if text(d1) != text(d3) or (t2 < t1 * 1.2 and json.dumps(d1) == json.dumps(d2)):
    bad = True
    print("!! max_side_len 没有生效，或者用过之后没有恢复")
a.close(); b.close()


# 3. 线程数：设 1 线程时进程里有几个线程、识别时占几个逻辑处理器
def usage(exe, threads, n=4):
    o = OCR(exe, threads)
    o.run(sample)
    ps = psutil.Process(o.p.pid)
    c0, t0 = ps.cpu_times(), time.time()
    for _ in range(n):
        o.run(sample)
    c1, wall = ps.cpu_times(), time.time() - t0
    nth = ps.num_threads()
    o.close()
    return wall / n, ((c1.user - c0.user) + (c1.system - c0.system)) / wall, nth


for th in (1, 2):
    for name, exe in (("原版", orig), ("修好的", fixed)):
        s, cpu, nth = usage(exe, th)
        print(f"{name} --numThread={th}：每页 {s:.2f} 秒，识别时占 {cpu:.1f} 个逻辑处理器，进程里 {nth} 个线程")


# 4. 多个进程同时识别（每个 1 线程）：每分钟多少页
def throughput(exe, procs, each=4):
    os_ = [OCR(exe, 1) for _ in range(procs)]
    for o in os_:
        o.run(sample)
    t0 = time.time()
    ts = [threading.Thread(target=lambda o=o: [o.run(sample) for _ in range(each)]) for o in os_]
    for t in ts:
        t.start()
    for t in ts:
        t.join()
    wall = time.time() - t0
    for o in os_:
        o.close()
    return procs * each / wall * 60


n = psutil.cpu_count(logical=False) or 2
for procs in sorted({1, 2, n, psutil.cpu_count()}):
    r1, r2 = throughput(orig, procs), throughput(fixed, procs)
    print(f"{procs} 个进程 × 1 线程：原版每分钟 {r1:.1f} 页，修好的 {r2:.1f} 页（{(r2 / r1 - 1):+.0%}）")

sys.exit(1 if bad else 0)
