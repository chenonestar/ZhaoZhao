import { useEffect, useState } from "react";
import { api } from "./api";
import type { Config, FailedItem, RootStatus, Status } from "./types";
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
}

const KIND: Record<string, string> = { local: "本地盘", removable: "移动硬盘", network: "网络盘" };

export function Manage({ status, tab, setTab, onClose, refresh }: Props) {
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
            {" "}文件 {t.Total} · 已提取 {t.Done} · 待处理 {t.Pending} · 仅文件名 {t.MetaOnly} · 失败 {t.Failed} · 已加密 {t.Encrypted}
          </span>
        </div>
        {status.ocrQueue > 0 && (
          <div className="dim">OCR 队列 {status.ocrQueue}{status.ocrEtaSec > 0 ? `，预计剩余 ${fmtDuration(status.ocrEtaSec)}` : ""}</div>
        )}
        {status.legacyQueue > 0 && <div className="dim">老格式待转换 {status.legacyQueue}</div>}
        {status.onBattery && <div className="dim">正在使用电池：OCR 和格式转换已暂停</div>}
        {status.current.length > 0 && (
          <div className="current dim">正在处理（{status.active}/{status.workers}）：{status.current.map((c) => <div key={c} title={c}>{c}</div>)}</div>
        )}
        <div className="btns">
          {status.paused ? <button className="primary" onClick={() => run(api.resume())}>恢复全部任务</button> : <button onClick={() => run(api.pause())}>暂停全部任务</button>}
          <button onClick={() => run(api.rescan())}>立即扫描全部</button>
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
                <div>文件 {r.counts.Total}</div>
                <div className="dim">待处理 {r.counts.Pending} · 失败 {r.counts.Failed}</div>
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
        <div className="modal-foot">
          <button onClick={onClose}>取消</button>
          <button className="primary" onClick={() => onSave({
            includeExts: inc.split(/[\s,]+/).filter(Boolean),
            excludeGlobs: exc.split("\n").map((s) => s.trim()).filter(Boolean),
            followLinks: follow || undefined,
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
  useEffect(() => { api.config().then(setC); }, []);
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
      <textarea rows={4} value={c[k].join("\n")} onChange={(e) => setC({ ...c, [k]: lines(e.target.value) })} /></label>
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
      <label className="field inline"><span>全局快捷键（修改后重启生效）</span>
        <input value={c.hotkey} onChange={(e) => setC({ ...c, hotkey: e.target.value })} placeholder="Ctrl+Alt+F" /></label>

      <h4>搜索</h4>
      {chk("simplify", "繁简体归一（搜「预算」也能找到「預算」）")}

      <h4>后台任务</h4>
      {num("workers", "提取并发数", "0 表示 CPU 核心数的一半")}
      {num("timeoutSec", "单个文件提取超时（秒）")}
      {num("legacyWorkers", "老格式转换并发数")}
      {chk("pauseOnBattery", "使用电池时暂停 OCR 和格式转换")}

      <h4>移动硬盘与网络盘</h4>
      {num("networkScanMin", "网络盘定时扫描间隔（分钟）")}
      {num("networkReadConc", "网络盘读取并发数")}
      {num("autoDeleteDays", "超过 N 天未接入则自动删除该盘索引", "0 表示关闭")}

      <h4>压缩包与邮件</h4>
      {num("containerMaxDepth", "最大嵌套层数")}
      {num("archiveMaxMB", "单个压缩包解压总量上限（MB）")}
      {num("archiveMaxRatio", "单个内部文件压缩比上限（倍）")}

      <div className="modal-foot">
        {msg && <span className="flash">{msg}</span>}
        <button className="primary" onClick={() => api.saveConfig(c).then((n) => { setC(n); setMsg("已保存"); setTimeout(() => setMsg(""), 1500); }).catch((e) => setMsg(e.message))}>保存设置</button>
      </div>
    </div>
  );
}
