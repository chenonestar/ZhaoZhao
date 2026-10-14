# 找找：常驻 LibreOffice 的转换桥。
# 由解析子进程启动：连接已用 --accept 启动的 soffice，然后从标准输入逐行读取转换任务（JSON），
# 每个任务回复一行 JSON。标准输入关闭时结束 LibreOffice 并退出。
# 用法: python lobridge.py <管道名> <连接时限秒数>
import json
import sys
import time

try:
    import uno
    from com.sun.star.beans import PropertyValue
    from com.sun.star.connection import NoConnectException
    from com.sun.star.lang import DisposedException
except Exception as e:  # 没有 Python-UNO 组件
    print(json.dumps({"ready": False, "err": "无法加载 Python-UNO 组件: %s" % e}), flush=True)
    sys.exit(2)


def prop(name, value):
    p = PropertyValue()
    p.Name = name
    p.Value = value
    return p


def main():
    pipe = sys.argv[1]
    limit = float(sys.argv[2]) if len(sys.argv) > 2 else 90
    url = "uno:pipe,name=%s;urp;StarOffice.ComponentContext" % pipe
    local = uno.getComponentContext()
    resolver = local.ServiceManager.createInstanceWithContext("com.sun.star.bridge.UnoUrlResolver", local)
    deadline = time.time() + limit
    while True:
        try:
            ctx = resolver.resolve(url)
            break
        except NoConnectException:
            if time.time() > deadline:
                print(json.dumps({"ready": False, "err": "连接 LibreOffice 超时（%d 秒）" % limit}), flush=True)
                return 3
            time.sleep(0.1)
    desktop = ctx.ServiceManager.createInstanceWithContext("com.sun.star.frame.Desktop", ctx)
    print(json.dumps({"ready": True}), flush=True)

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        req = json.loads(line)
        t0 = time.time()
        resp = {"ok": False}
        doc = None
        dead = False
        try:
            # 只读、隐藏、不运行宏、不更新链接；不提供交互处理，加密文件直接失败而不是弹出密码框
            doc = desktop.loadComponentFromURL(uno.systemPathToFileUrl(req["in"]), "_blank", 0, (
                prop("Hidden", True), prop("ReadOnly", True), prop("MacroExecutionMode", 0),
                prop("UpdateDocMode", 0), prop("Silent", True)))
            if doc is None:
                raise RuntimeError("无法打开（文件可能已加密或损坏）")
            doc.storeToURL(uno.systemPathToFileUrl(req["out"]), (prop("FilterName", req["filter"]), prop("Overwrite", True)))
            resp["ok"] = True
        except DisposedException as e:
            dead = True
            resp["err"] = "LibreOffice 连接已断开: %s" % getattr(e, "Message", e)
        except Exception as e:
            msg = getattr(e, "Message", "") or str(e)
            resp["err"] = "%s: %s" % (type(e).__name__, msg)
            if "disposed" in msg.lower() or "Binary URP bridge" in msg:
                dead = True
        finally:
            if doc is not None:
                try:
                    doc.close(True)
                except Exception:
                    try:
                        doc.dispose()
                    except Exception:
                        pass
        resp["sec"] = round(time.time() - t0, 3)
        resp["dead"] = dead
        print(json.dumps(resp), flush=True)
        if dead:
            return 4
    try:
        desktop.terminate()
    except Exception:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
