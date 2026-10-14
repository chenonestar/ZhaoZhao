import { useEffect, useState } from "react";
import { api } from "./api";
import type { Config, FailedItem, HotkeyState, RootStatus, Status } from "./types";
import { fmtDuration, fmtSize, fmtTime } from "./util";
import { FolderPicker } from "./FolderPicker";
import { Help } from "./Help";

type Tab = "index" | "failed" | "settings" | "help";

interface Props {
  status: Status | null;
  tab: Tab;
  setTab: (t: Tab) => void;
  onClose: () => void;
  refresh: () => void;
  onQuit: () => void;
}

const KIND: Record<string, string> = { local: "本地盘", removable: "移动硬盘", network: "网络盘" };

export function Manage({ status, tab, setTab, onClose, refresh, onQuit }: Props) {
  return (
    <div className="modal-back" onClick={onClose}>
      <div className="modal manage" onClick={(e) => e.stopPropagation()}>
        <div className="tabs">
          {([["index", "索引"], ["failed", "失败列表"], ["settings", "设置"], ["help", "语法帮助"]] as [Tab, string][]).map(([k, l]) => (
            <button key={k} className={tab === k ? "on" : ""} onClick={() => setTab(k)}>
              {l}
              {k === "failed" && status && status.totals.Failed + status.totals.Encrypted > 0 ? ` (${status.totals.Failed + status.totals.Encrypted})` : ""}
            </button>
          ))}
          <span className="grow" />
          <button className="danger" title="结束找找（包括后台索引）。下次启动后从中断处继续"
            onClick={() => { if (confirm("退出找找？后台索引会停止，下次启动后从中断处继续。")) api.quit().finally(onQuit); }}>退出找找</button>
          <button onClick={onClose}>关闭</button>
        </div>
        <div className="manage-body">
          {tab === "index" && <IndexTab status={status} refresh={refresh} />}
          {tab === "failed" && <FailedTab refresh={refresh} />}
          {tab === "settings" && <SettingsTab />}
          {tab === "help" && <Help />}
        </div>
      </div>
    </div>
  );
}

function IndexTab({ status, refresh }: { status: Status | null; refresh: () => void }) {
  const [picker, setPicker] = useState(false);
  const [err, setErr] = useState("");
  const [optsFor, setOptsFor] = useState<RootStatus | null>(null);
  const run = (p: Promise<unknown>) => p.then(() => { setErr(""); refresh(); }).catch((e) => setErr(e.message));
  if (!status) return <div className="dim pad">加载中…</div>;
  const t = status.totals;
  return (
    <div>
      <div className="summary">
        <div>
          <b>{status.paused ? "已暂停" : status.rebuilding ? "正在重建全文索引…" : status.idle ? "索引已是最新" : "正在建立索引"}</b>
          <span className="dim">
            {" "}{status.roots.length > 1 ? `全部 ${status.roots.length} 个索引根合计：` : ""}文件 {t.Total} · 已提取 {t.Done} · 待处理 {t.Pending} · 仅文件名 {t.MetaOnly} · 失败 {t.Failed} · 已加密 {t.Encrypted}
          </span>
        </div>
        {status.stalledSec > 0 && (
          <div className="error">
            已经 {fmtDuration(status.stalledSec)} 没有任何文件完成，可能卡住了。请到「设置」页底部点「导出诊断信息」，把生成的文件发给开发者；
            也可以先暂停再恢复，或退出后重新启动。
          </div>
        )}
        {status.ocrQueue > 0 && (
          <div className="dim">OCR 队列 {status.ocrQueue}，{status.ocrEtaSec > 0 ? `按最近的速度预计还需 ${fmtDuration(status.ocrEtaSec)}` : "正在估算剩余时间"}</div>
        )}
        {status.legacyQueue > 0 && <div className="dim">老格式待转换 {status.legacyQueue}</div>}
        {status.onBattery && <div className="dim">正在使用电池：OCR 和格式转换已暂停</div>}
        {status.legacy && !status.idle && (
          <div className="dim small">
            提取 {status.active}/{status.workers} · 老格式转换 {status.legacy.used}/{status.legacy.cap} · OCR {status.ocr.used}/{status.ocr.cap} · 网络盘读取 {status.net.used}/{status.net.cap}
          </div>
        )}
        {status.current.length > 0 && (
          <div className="current dim">
            正在处理：
            {status.current.slice(0, 12).map((j) => (
              <div key={j.path} title={j.path}>
                <span className="badge">{j.kind}</span> {j.path.split(/[\\/]/).pop()} — {j.stage}{j.progress ? ` ${j.progress}` : ""} · {fmtDuration(j.seconds)}
              </div>
            ))}
            {status.current.length > 12 && <div>…… 还有 {status.current.length - 12} 个</div>}
          </div>
        )}
        <div className="components small">
          {status.componentsChecking && <div className="dim">正在检测格式转换和 OCR 组件…</div>}
          {(status.components ?? []).map((c) => (
            <div key={c.name} className={c.ok ? "dim" : "bad"} title={c.path}>
              {c.ok ? "✓" : "✗"} {c.name}：{c.message}
            </div>
          ))}
        </div>
        <div className="btns">
          {status.paused ? <button className="primary" onClick={() => run(api.resume())}>恢复全部任务</button> : <button onClick={() => run(api.pause())}>暂停全部任务</button>}
          <button onClick={() => run(api.rescan())}>立即扫描全部</button>
          <button disabled={status.componentsChecking} onClick={() => run(api.checkComponents())}>检测组件</button>
          <button onClick={() => { if (confirm("重建会重新提取所有文件的内容，耗时较长。继续吗？")) run(api.rebuild()); }}>重建全部索引</button>
        </div>
      </div>
      {err && <div className="error">{err}</div>}

      <div className="section-head">
        <h4>索引根</h4>
        <button className="primary" onClick={() => setPicker(true)}>添加目录…</button>
      </div>
      {status.roots.length === 0 && <div className="dim pad">还没有索引根。点击「添加目录」选择要建立索引的目录或整个盘符。</div>}
      <table className="grid">
        <tbody>
          {status.roots.map((r) => (
            <tr key={r.id}>
              <td className="path-cell">
                <div title={r.path}>{r.path}</div>
                <div className="dim small">
                  {r.paused ? "已暂停" : !r.online ? "磁盘离线" : r.scanning ? `扫描中，已看到 ${r.scanned} 个文件` : `上次扫描 ${fmtTime(r.lastScan)}`}
                  {" · "}{KIND[r.kind] ?? ""}
                </div>
              </td>
              <td className="num small">
                <div>文件 {r.counts.Total} · 已提取 {r.counts.Done}</div>
                <div className="dim">
                  待处理 {r.counts.Pending}
                  {r.legacyPending + r.ocrPending > 0 && `（老格式 ${r.legacyPending} · OCR ${r.ocrPending}）`}
                </div>
                <div className="dim">仅文件名 {r.counts.MetaOnly} · 失败 {r.counts.Failed} · 已加密 {r.counts.Encrypted}</div>
              </td>
              <td className="actions">
                {r.paused ? <button onClick={() => run(api.resume(r.id))}>恢复</button> : <button onClick={() => run(api.pause(r.id))}>暂停</button>}
                <button onClick={() => run(api.rescan(r.id))}>扫描</button>
                <button onClick={() => setOptsFor(r)}>选项</button>
                <button onClick={() => { if (confirm("重新提取该索引根下所有文件的内容？")) run(api.rebuild(r.id)); }}>重建</button>
                <button className="danger" onClick={() => { if (confirm(`移除索引根并删除它的全部索引数据？\n${r.path}`)) run(api.removeRoot(r.id)); }}>移除</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <div className="section-head"><h4>磁盘</h4></div>
      <table className="grid">
        <thead><tr><th>卷标</th><th>状态</th><th>文件数</th><th>索引占用</th><th /></tr></thead>
        <tbody>
          {status.volumes.map((v) => (
            <tr key={v.id}>
              <td>{v.label}<div className="dim small">{KIND[v.kind] ?? v.kind}</div></td>
              <td>
                {v.online ? <span className="ok">在线 {v.mount}</span> : <span className="offline-text">离线（上次 {v.lastMount.replace(/[\\/]+$/, "")}，最后在线 {fmtTime(v.lastSeen)}）</span>}
                {v.syncing && <span className="badge"> 同步中</span>}
              </td>
              <td className="num">{v.files}</td>
              <td className="num">{fmtSize(v.indexBytes)}</td>
              <td className="actions"><button className="danger" onClick={() => { if (confirm(`删除「${v.label}」的全部索引？`)) run(api.deleteVolume(v.id)); }}>删除索引</button></td>
            </tr>
          ))}
        </tbody>
      </table>
      {picker && <FolderPicker onClose={() => setPicker(false)} onPick={(p) => { setPicker(false); run(api.addRoot(p)); }} />}
      {optsFor && <RootOptsDialog root={optsFor} onClose={() => setOptsFor(null)} onSave={(o) => { setOptsFor(null); run(api.rootOpts(optsFor.id, o)); }} />}
    </div>
  );
}

function RootOptsDialog({ root, onClose, onSave }: { root: RootStatus; onClose: () => void; onSave: (o: unknown) => void }) {
  const [inc, setInc] = useState((root.opts?.includeExts ?? []).join(" "));
  const [exc, setExc] = useState((root.opts?.excludeGlobs ?? []).join("\n"));
  const [follow, setFollow] = useState(root.opts?.followLinks ?? false);
  const [ocr, setOcr] = useState(root.opts?.ocr ?? true);
  return (
    <div className="modal-back" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>索引根选项</h3>
        <div className="dim small">{root.path}</div>
        <label className="field"><span>只提取这些类型的内容（扩展名，空格分隔；留空为不限）</span>
          <input value={inc} onChange={(e) => setInc(e.target.value)} placeholder="docx xlsx pdf txt" /></label>
        <label className="field"><span>附加排除规则（每行一条，支持 * ? 通配符）</span>
          <textarea rows={4} value={exc} onChange={(e) => setExc(e.target.value)} /></label>
        <label className="check"><input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />跟随符号链接和目录联接</label>
        <label className="check"><input type="checkbox" checked={ocr} onChange={(e) => setOcr(e.target.checked)} />对该索引根里的图片和扫描 PDF 做 OCR（需要 OCR 组件）</label>
        <div className="modal-foot">
          <button onClick={onClose}>取消</button>
          <button className="primary" onClick={() => onSave({
            includeExts: inc.split(/[\s,]+/).filter(Boolean),
            excludeGlobs: exc.split("\n").map((s) => s.trim()).filter(Boolean),
            followLinks: follow || undefined,
            ocr: ocr ? undefined : false,
          })}>保存</button>
        </div>
      </div>
    </div>
  );
}

function FailedTab({ refresh }: { refresh: () => void }) {
  const [items, setItems] = useState<FailedItem[] | null>(null);
  const [sel, setSel] = useState<Set<number>>(new Set());
  const [err, setErr] = useState("");
  const load = () => api.failed().then(setItems).catch((e) => setErr(e.message));
  useEffect(() => { load(); }, []);
  const run = (p: Promise<unknown>) => p.then(() => { setSel(new Set()); load(); refresh(); }).catch((e) => setErr(e.message));
  if (!items) return <div className="dim pad">加载中…</div>;
  const ids = [...sel];
  return (
    <div>
      <div className="btns">
        <button disabled={!ids.length} onClick={() => run(api.retry(ids))}>重试所选</button>
        <button disabled={!items.length} onClick={() => run(api.retry([]))}>重试全部失败项</button>
        <button disabled={!ids.length} onClick={() => { if (confirm("把所选文件加入排除规则并从索引移除？")) run(api.exclude(ids)); }}>加入排除规则</button>
      </div>
      {err && <div className="error">{err}</div>}
      {items.length === 0 && <div className="dim pad">没有失败的文件。</div>}
      <table className="grid">
        <tbody>
          {items.map((f) => (
            <tr key={f.id}>
              <td><input type="checkbox" checked={sel.has(f.id)} onChange={() => { const n = new Set(sel); n.has(f.id) ? n.delete(f.id) : n.add(f.id); setSel(n); }} /></td>
              <td className="path-cell"><div title={f.path}>{f.path}</div></td>
              <td><span className={"badge " + (f.state === 3 ? "" : "bad")}>{f.state === 3 ? "已加密" : "失败"}</span> {f.reason}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const lines = (s: string) => s.split("\n").map((x) => x.trim()).filter(Boolean);

function SettingsTab() {
  const [c, setC] = useState<Config | null>(null);
  const [msg, setMsg] = useState("");
  const [hk, setHk] = useState<HotkeyState | null>(null);
  useEffect(() => { api.config().then(setC); api.hotkey().then(setHk).catch(() => {}); }, []);
  if (!c) return <div className="dim pad">加载中…</div>;
  const num = (k: keyof Config, label: string, hint = "") => (
    <label className="field inline"><span>{label}</span>
      <input type="number" value={c[k] as number} onChange={(e) => setC({ ...c, [k]: Number(e.target.value) })} />
      {hint && <em className="dim small">{hint}</em>}</label>
  );
  const chk = (k: keyof Config, label: string) => (
    <label className="check"><input type="checkbox" checked={c[k] as boolean} onChange={(e) => setC({ ...c, [k]: e.target.checked })} />{label}</label>
  );
  const listField = (k: "excludeDirNames" | "excludePrefixes" | "excludeGlobs" | "excludeExts", label: string) => (
    <label className="field"><span>{label}（每行一条）</span>
      <textarea rows={4} value={(c[k] ?? []).join("\n")} onChange={(e) => setC({ ...c, [k]: lines(e.target.value) })} /></label>
  );
  return (
    <div className="settings">
      <h4>排除规则</h4>
      {listField("excludeDirNames", "排除的目录名（任意层级）")}
      {listField("excludePrefixes", "排除的路径前缀")}
      {listField("excludeGlobs", "排除的通配符（匹配文件名或完整路径）")}
      {listField("excludeExts", "排除的扩展名")}
      <button onClick={() => api.defaults().then((d) => setC({ ...c, excludeDirNames: d.excludeDirNames, excludePrefixes: d.excludePrefixes, excludeGlobs: [], excludeExts: [] }))}>恢复默认排除清单</button>

      <h4>文件</h4>
      {num("maxTextMB", "文本类文件大小上限（MB）", "超出的文件只索引文件名")}
      {num("maxOtherMB", "其他类型大小上限（MB）")}
      {chk("sniffUnknown", "未知扩展名的文件抽样判断是否为文本，是则按文本索引")}
      {chk("skipCloud", "跳过云盘中「仅在线可用」的占位文件")}
      {chk("followLinks", "跟随符号链接和目录联接")}

      <h4>呼出窗口</h4>
      <HotkeyField value={c.hotkey} onChange={(v) => setC({ ...c, hotkey: v })} state={hk} />

      <h4>搜索</h4>
      {chk("simplify", "繁简体归一（搜「预算」也能找到「預算」）")}

      <h4>后台任务（修改后立即生效）</h4>
      {num("workers", "提取并发数", "0 表示 CPU 核心数的一半")}
      {num("timeoutSec", "单个文件提取超时（秒）")}
      {num("legacyWorkers", "老格式转换并发数")}
      {chk("ocrEnabled", "识别图片和扫描 PDF 里的文字（OCR）")}
      {num("ocrWorkers", "OCR 并发数", "0 表示自动：8 个以上逻辑处理器时 2 个，否则 1 个；每个约占几百 MB 内存")}
      {chk("pauseOnBattery", "使用电池时暂停 OCR 和格式转换")}

      <h4>移动硬盘与网络盘</h4>
      {num("networkScanMin", "网络盘定时扫描间隔（分钟）")}
      {num("networkReadConc", "网络盘读取并发数", "同时从网络盘读取的文件数；读完后在本机提取，不占用这个名额")}
      {num("autoDeleteDays", "超过 N 天未接入则自动删除该盘索引", "0 表示关闭")}

      <h4>压缩包与邮件</h4>
      {num("containerMaxDepth", "最大嵌套层数")}
      {num("archiveMaxMB", "单个压缩包解压总量上限（MB）")}
      {num("archiveMaxRatio", "单个内部文件压缩比上限（倍）")}

      <h4>日志与诊断</h4>
      <label className="field inline"><span>日志详细程度</span>
        <select value={c.logLevel || "detail"} onChange={(e) => setC({ ...c, logLevel: e.target.value })}>
          <option value="detail">详细（记录每个文件的处理过程，便于排查问题）</option>
          <option value="normal">普通（只记录错误和重要事件）</option>
        </select></label>
      <Diagnostics />

      <div className="modal-foot">
        {msg && <span className="flash">{msg}</span>}
        <button className="primary" onClick={() => api.saveConfig(c).then((n) => {
          setC(n); setMsg("已保存"); setTimeout(() => setMsg(""), 1500);
          api.hotkey().then(setHk).catch(() => {});
        }).catch((e) => setMsg(e.message))}>保存设置</button>
      </div>
    </div>
  );
}

// 快捷键录入：点一下输入框，直接按下想要的组合。
function HotkeyField({ value, onChange, state }: { value: string; onChange: (v: string) => void; state: HotkeyState | null }) {
  const on = value !== "";
  const record = (e: React.KeyboardEvent<HTMLInputElement>) => {
    e.preventDefault();
    if (e.key === "Escape" || e.key === "Tab") return;
    if (e.key === "Backspace" || e.key === "Delete") { onChange(""); return; }
    let key = "";
    if (e.code === "Space") key = "Space";
    else if (/^Key[A-Z]$/.test(e.code)) key = e.code.slice(3);
    else if (/^Digit[0-9]$/.test(e.code)) key = e.code.slice(5);
    else if (/^F([1-9]|1[0-9]|2[0-4])$/.test(e.key)) key = e.key;
    if (!key) return; // 只按了 Ctrl/Alt 等修饰键：等用户按完
    const mods = [e.ctrlKey && "Ctrl", e.altKey && "Alt", e.shiftKey && "Shift", e.metaKey && "Win"].filter(Boolean);
    onChange([...mods, key].join("+"));
  };
  return (
    <div className="field">
      <label className="check"><input type="checkbox" checked={on} onChange={(e) => onChange(e.target.checked ? "Ctrl+Alt+Space" : "")} />
        用全局快捷键呼出找找（默认不开启）</label>
      {on && (
        <label className="field inline"><span>快捷键（点这里，然后按下组合键）</span>
          <input readOnly value={value} onKeyDown={record} placeholder="例如 Ctrl+Alt+Space" /></label>
      )}
      {state && state.hotkey !== "" && state.hotkey === value && (
        <div className={state.active ? "ok small" : "error small"}>{state.hotkey}：{state.message}</div>
      )}
      <div className="dim small">保存后立即生效。至少包含 Ctrl、Alt 或 Win 之一；与其他程序（如 QQ）冲突时会提示，请换一个组合。</div>
    </div>
  );
}

function Diagnostics() {
  const [anon, setAnon] = useState(true);
  const [out, setOut] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <div className="field">
      <div className="dim small">遇到问题时导出诊断信息（日志、当前状态、设置、运行环境），把生成的 zip 文件发给开发者。</div>
      <label className="check"><input type="checkbox" checked={anon} onChange={(e) => setAnon(e.target.checked)} />隐藏文件名和路径（替换成代号，保留扩展名和目录层级）</label>
      <div className="btns">
        <button disabled={busy} onClick={() => {
          setBusy(true); setOut("");
          api.diagnostics(anon).then((r) => setOut("已生成：" + r.path)).catch((e) => setOut("导出失败：" + e.message)).finally(() => setBusy(false));
        }}>{busy ? "正在导出…" : "导出诊断信息"}</button>
      </div>
      {out && <div className="small">{out}</div>}
    </div>
  );
}
