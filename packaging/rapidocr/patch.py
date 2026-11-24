"""给 RapidOCR-json 的源代码打补丁（找找自己编译用）。

1. 线程数真正生效：原版对推理库只设了「节点之间」的线程数（SetInterOpNumThreads），「节点内部」的
   （SetIntraOpNumThreads）被注释掉了，推理库按物理核心数开线程：--numThread 设多少，每个进程都开满
   全部核心。多个进程同时识别时线程数成倍超过 CPU，互相抢。三处（找文字、判方向、认字）都打开。
2. 每张图可以单独指定找文字时把长边缩到多少像素：请求里加 "max_side_len"。找找认大幅面页时用 2048，
   不用再另开一个组件进程。
3. 版本号后面加 -zz1，日志里能看出用的是哪个。

源文件里的注释有的是 GBK 编码，所以按字节替换。用法：python patch.py <cpp 目录>
"""
import pathlib
import sys

cpp = pathlib.Path(sys.argv[1])


def patch(name, pairs):
    p = cpp / name
    b = p.read_bytes()
    for old, new, count in pairs:
        n = b.count(old)
        if n != count:
            sys.exit(f"{name}: 找到 {n} 处 {old!r}，应为 {count} 处（源代码版本不对？）")
        b = b.replace(old, new)
    p.write_bytes(b)
    print("已修改", name)


for f in ("src/DbNet.cpp", "src/AngleNet.cpp", "src/CrnnNet.cpp"):
    patch(f, [(b"//sessionOptions.SetIntraOpNumThreads(numThread);", b"sessionOptions.SetIntraOpNumThreads(numThread);", 1)])

patch("src/main.cpp", [
    (b'#define PROJECT_VER "v0.2.0"', b'#define PROJECT_VER "v0.2.0-zz1"', 1),
    (b"OcrLite *OCRLiteP = NULL;", b"OcrLite *OCRLiteP = NULL;\nint zzSide = 0; // zz: max_side_len of the current request (0: use --maxSideLen)", 1),
    (b"ocrLite.detect(mat, padding, maxSideLen,", b"ocrLite.detect(mat, padding, zzSide > 0 ? zzSide : maxSideLen,", 1),
    (b"std::string type = tool::load_json_str(jsonIn);",
     b"zzSide = 0;\n"
     b"                try {\n"
     b"                    auto zj = nlohmann::json::parse(jsonIn);\n"
     b"                    auto it = zj.find(\"max_side_len\");\n"
     b"                    if (it != zj.end()) zzSide = it->get<int>();\n"
     b"                } catch (...) { zzSide = 0; }\n"
     b"                std::string type = tool::load_json_str(jsonIn);", 1),
])
