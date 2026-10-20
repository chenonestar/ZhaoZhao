import { useEffect, useRef, useState } from "react";
import { api } from "./api";
import type { Config, DiagState, FailedFilter, FailedFolders, FailedItem, FailedSummary, HotkeyState, LegacyTestState, OCRBenchInfo, ProblemCat, RootStatus, SlotStatus, SpaceDetail, Status } from "./types";
import { copyText, fmtDuration, fmtSize, fmtTime, folderOf } from "./util";
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

// QueueLine 显示一条处理通道：待处理数量、正在处理的线程数、预计剩余时间。三条通道同时进行。
function QueueLine({ label, n, eta, lane, paused, extra }: { label: string; n: number; eta: number; lane?: SlotStatus; paused?: boolean; extra?: string }) {
  if (!n) return null;
  let when = "正在估算剩余时间";
  if (paused) when = "已暂停";
  else if (eta > 0) when = `按最近的速度预计还需 ${fmtDuration(eta)}`;
  return (
    <div className="dim">
      {label} {n}{lane ? `（同时处理 ${lane.used}/${lane.cap}${extra ?? ""}）` : ""}，{when}
    </div>
  );
}

// ocrExtra：OCR 每个进程的线程数和最近的速度，以及正在实测搭配时的说明。
function ocrExtra(s: Status): string {
  let x = s.ocrThreads > 0 ? `，每个进程 ${s.ocrThreads} 线程` : "";
  if (s.ocrPagesPerMin > 0) x += `，最近每分钟 ${s.ocrPagesPerMin.toFixed(1)} 页`;
  if (s.ocrTuning) x += `；${s.ocrTuning}`;
  return x;
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
        <QueueLine label="普通文件待处理" n={status.fastQueue} eta={status.fastEtaSec} lane={{ used: status.active, cap: status.workers }} />
        <QueueLine label="老格式待转换" n={status.legacyQueue} eta={status.legacyEtaSec} lane={status.legacy} paused={status.onBattery} />
        <QueueLine label="OCR 待识别" n={status.ocrQueue} eta={status.ocrEtaSec} lane={status.ocr} paused={status.onBattery} extra={ocrExtra(status)} />
        {status.onBattery && <div className="dim">正在使用电池：OCR 和格式转换已暂停</div>}
        {/* 正在处理：固定高度、内部滚动，卡住提示也放在里面，状态刷新时下面的内容不上下跳动 */}
        {(!status.idle || status.current.length > 0 || status.stalledSec > 0) && (
          <div className="current dim">
            {status.stalledSec > 0 && (
              <div className="error stall">
                已经 {fmtDuration(status.stalledSec)} 没有任何文件完成，可能卡住了。请到「设置」页底部点「导出诊断信息」，把生成的文件发给开发者；
                也可以先暂停再恢复，或退出后重新启动。
              </div>
            )}
            <div>
              正在处理 {status.current.length} 个{status.net && status.net.used > 0 ? ` · 网络盘读取 ${status.net.used}/${status.net.cap}` : ""}
              {status.keepingAwake ? " · 已阻止电脑自动睡眠（索引完成后恢复）" : ""}
            </div>
            {status.current.map((j) => (
              <div key={j.id} title={j.path}>
                <span className="badge">{j.kind}</span> {j.path.split(/[\\/]/).pop()} — {j.stage}{j.progress ? ` ${j.progress}` : ""} · {fmtDuration(j.seconds)}
              </div>
            ))}
          </div>
        )}
        <div className="components small">
          {status.componentsChecking && !(status.components ?? []).length && <div className="dim">正在检测格式转换和 OCR 组件…</div>}
          {(status.components ?? []).map((c) => (
            <div key={c.name} className={c.ok ? "dim" : "bad"} title={c.path}>
              {c.ok ? "✓" : "✗"} {c.name}：{c.message}
            </div>
          ))}
        </div>
        <div className="btns">
          {status.paused ? <button className="primary" onClick={() => run(api.resume())}>恢复全部任务</button> : <button onClick={() => run(api.pause())}>暂停全部任务</button>}
          <button onClick={() => run(api.rescan())}>立即扫描全部</button>
          <button disabled={status.componentsChecking} onClick={() => run(api.checkComponents())}>{status.componentsChecking ? "正在检测组件…" : "检测组件"}</button>
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

      <SpaceSection status={status} run={run} />

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

// 索引库占用的空间：概况、后台整理进度、详细统计、压缩。
function SpaceSection({ status, run }: { status: Status; run: (p: Promise<unknown>) => void }) {
  const sp = status.space;
  const [detail, setDetail] = useState<SpaceDetail | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  if (!sp) return null;
  const pct = (a: number, b: number) => (b > 0 ? Math.round((a * 100) / b) + "%" : "");
  return (
    <>
      <div className="section-head"><h4>索引库占用空间</h4></div>
      <div className="small">
        <div>
          索引库 {fmtSize(sp.dbBytes)}{sp.walBytes > 0 && `，临时日志 ${fmtSize(sp.walBytes)}`}
          {sp.freeBytes > 0 && <>，其中空闲 {fmtSize(sp.freeBytes)}（{sp.incremental ? "空闲时自动归还磁盘" : "点「压缩索引库」归还磁盘"}）</>}
        </div>
        {status.maintenance && <div className="dim">{status.maintenance}</div>}
        {sp.compacting && <div className="dim">正在压缩索引库…（期间可以搜索，新的提取结果稍后写入）</div>}
        {sp.lastCompact && !sp.compacting && <div className="dim">{sp.lastCompact}</div>}
        <div className="btns">
          <button disabled={busy} onClick={() => {
            setBusy(true); setErr("");
            api.spaceStats().then(setDetail).catch((e) => setErr(e.message)).finally(() => setBusy(false));
          }}>{busy ? "正在统计…" : "统计详情"}</button>
          <button disabled={sp.compacting} onClick={() => {
            if (confirm("压缩索引库会重写整个索引库，把空闲空间还给磁盘，并改为以后空闲时自动归还。\n" +
              "需要与索引库差不多大的临时磁盘空间，可能需要几分钟；期间可以搜索，新的提取结果稍后写入。继续吗？")) run(api.compact());
          }}>压缩索引库</button>
        </div>
        {err && <div className="error">{err}</div>}
        {detail && (
          <div className="space-detail">
            <table className="grid">
              <thead><tr><th>组成</th><th>大小</th><th>占比</th></tr></thead>
              <tbody>
                {(detail.parts ?? []).map((p) => (
                  <tr key={p.name}><td>{p.name}</td><td className="num">{fmtSize(p.bytes)}</td><td className="num">{pct(p.bytes, detail.dbBytes)}</td></tr>
                ))}
              </tbody>
            </table>
            <div className="dim">
              提取文本 {detail.texts} 份，{detail.textFiles} 个文件引用（{detail.textFiles - detail.texts} 个与其他内容相同的文件共用），共 {detail.rawChars.toLocaleString()} 字
              {detail.legacy > 0 && `；${detail.legacy} 份还是旧格式，后台正在重新压缩`}
              {detail.fts && <div title="全文索引由若干段组成，空闲时会自动合并成一个段，清掉已删除、已重新提取的文件留下的残留条目">
                全文索引 {detail.fts.segments} 个段（最大的占 {pct(detail.fts.largest, detail.fts.pages)}）{detail.fts.deleted > 0 && `，已删除的残留条目 ${detail.fts.deleted} 个`}
              </div>}
            </div>
            <table className="grid">
              <thead><tr><th>扩展名</th><th>提取文本份数</th><th>大小（压缩后）</th></tr></thead>
              <tbody>
                {(detail.byExt ?? []).map((x) => (
                  <tr key={x.name}><td>{x.name}</td><td className="num">{x.count}</td><td className="num">{fmtSize(x.bytes)}</td></tr>
                ))}
              </tbody>
            </table>
            <details>
              <summary>最大的 {detail.biggest?.length ?? 0} 份提取文本</summary>
              <table className="grid">
                <tbody>
                  {(detail.biggest ?? []).map((b, i) => (
                    <tr key={b.fileId}>
                      <td className="small">{detail.biggestPaths?.[i]}</td>
                      <td className="num">{fmtSize(b.bytes)}</td>
                      <td className="num">{b.chars.toLocaleString()} 字{b.sharers > 1 ? `，${b.sharers} 个文件共用` : ""}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </details>
          </div>
        )}
      </div>
    </>
  );
}

function RootOptsDialog({ root, onClose, onSave }: { root: RootStatus; onClose: () => void; onSave: (o: unknown) => void }) {
  const [inc, setInc] = useState((root.opts?.includeExts ?? []).join(" "));
  const [exc, setExc] = useState((root.opts?.excludeGlobs ?? []).join("\n"));
  const [follow, setFollow] = useState(root.opts?.followLinks ?? false);
  const [ocr, setOcr] = useState(root.opts?.ocr ?? true);
  const [picking, setPicking] = useState(false);
  return (
    <div className="modal-back" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>索引根选项</h3>
        <div className="dim small">{root.path}</div>
        <label className="field"><span>只提取这些类型的内容（扩展名，空格分隔；留空为不限）</span>
          <input value={inc} onChange={(e) => setInc(e.target.value)} placeholder="docx xlsx pdf txt" /></label>
        <label className="field"><span>附加排除规则（每行一条：文件夹路径，或带 * ? 通配符的文件名）</span>
          <textarea rows={4} value={exc} onChange={(e) => setExc(e.target.value)} /></label>
        <div className="btns"><button onClick={() => setPicking(true)}>选择要排除的子文件夹…</button></div>
        {picking && <FolderPicker title="选择要排除的文件夹（里面的文件不再索引）" start={root.path} onClose={() => setPicking(false)}
          onPick={(p) => { setPicking(false); setExc((exc.trim() ? exc.trim() + "\n" : "") + p); }} />}
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

const PAGE = 200;
const EXT_SHOW = 24;

// FailedTab：失败列表。按原因类别、扩展名、路径关键字筛选（数量由后台统计），
// 每次加载 200 个，滚动到底自动加载下一批；「重试这一类」「导出」作用于筛选出的全部文件。
function FailedTab({ refresh }: { refresh: () => void }) {
  const [f, setF] = useState<FailedFilter>({ cat: "", ext: "", q: "" });
  const [qInput, setQInput] = useState("");
  const [sum, setSum] = useState<FailedSummary | null>(null);
  const [items, setItems] = useState<FailedItem[] | null>(null);
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [sel, setSel] = useState<Set<number>>(new Set());
  const [allExts, setAllExts] = useState(false);
  const [msg, setMsg] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(""); // 正在进行的操作（重试、导出、排除）：按钮暂时不能再点
  const [folders, setFolders] = useState<FailedFolders | null>(null);
  const [allFolders, setAllFolders] = useState(false);
  const seq = useRef(0);
  const end = useRef<HTMLDivElement>(null);

  const reload = (flt: FailedFilter) => {
    const n = ++seq.current;
    setLoading(true);
    setErr("");
    Promise.all([api.failedSummary(flt), api.failed(flt, 0, PAGE)])
      .then(([s, page]) => {
        if (n !== seq.current) return;
        setSum(s); setItems(page.items); setMore(page.more); setSel(new Set());
      })
      .catch((e) => { if (n === seq.current) setErr(e.message); })
      .finally(() => { if (n === seq.current) setLoading(false); });
  };
  const loadMore = () => {
    if (!items || !items.length || loading) return;
    const n = seq.current;
    setLoading(true);
    api.failed(f, items[items.length - 1].id, PAGE)
      .then((page) => { if (n === seq.current) { setItems([...items, ...page.items]); setMore(page.more); } })
      .catch((e) => { if (n === seq.current) setErr(e.message); })
      .finally(() => { if (n === seq.current) setLoading(false); });
  };
  useEffect(() => { reload(f); }, [f]);
  // 选了索引根：按文件夹查看（当前文件夹下一级子文件夹各有多少）
  useEffect(() => {
    setAllFolders(false);
    if (!f.root) { setFolders(null); return; }
    api.failedFolders(f).then(setFolders).catch(() => setFolders(null));
  }, [f]);
  // 关键字输入停顿后再查
  useEffect(() => {
    const t = setTimeout(() => { if (qInput !== f.q) setF({ ...f, q: qInput }); }, 300);
    return () => clearTimeout(t);
  }, [qInput]);
  // 列表底部出现在视野里时自动加载下一批
  useEffect(() => {
    const el = end.current;
    if (!el || !more) return;
    const io = new IntersectionObserver((es) => { if (es[0].isIntersecting) loadMore(); });
    io.observe(el);
    return () => io.disconnect();
  });

  // label：操作名称，点下后立即显示「正在……」，完成前按钮不能再点
  const run = (p: Promise<unknown>, done?: (r: any) => void, label?: string) => {
    setErr("");
    if (label) { setBusy(label); setMsg(label + "…"); }
    return p.then((r) => { done?.(r); reload(f); refresh(); })
      .catch((e) => { setErr(e.message); if (label) setMsg(""); })
      .finally(() => setBusy(""));
  };
  // 打开、打开所在文件夹：不用刷新列表
  const act = (p: Promise<unknown>) => p.then(() => setErr("")).catch((e) => setErr(e.message));
  if (!sum || !items) return <div className="dim pad">{err ? <span className="error">{err}</span> : "加载中…"}</div>;

  const failCats = sum.cats.filter((c) => !c.meta);
  const metaCats = sum.cats.filter((c) => c.meta);
  const total = (cs: ProblemCat[]) => cs.reduce((a, c) => a + c.count, 0);
  const shown = f.cat === "" ? total(failCats) : f.cat === "meta" ? total(metaCats) : (sum.cats.find((c) => c.key === f.cat)?.count ?? 0);
  const isMeta = f.cat === "meta" || !!metaCats.find((c) => c.key === f.cat);
  const scope = f.cat === "" ? "失败和已加密" : f.cat === "meta" ? "仅文件名" : sum.cats.find((c) => c.key === f.cat)?.label ?? "";
  const pick = (cat: string) => setF({ ...f, cat });
  const chip = (key: string, label: string, n: number) => (
    <span key={key} className={"chip" + (f.cat === key ? " on" : "")} onClick={() => pick(key)}>{label} {n}</span>
  );
  const exts = sum.exts.length > EXT_SHOW && !allExts ? sum.exts.slice(0, EXT_SHOW) : [...sum.exts];
  if (f.ext && !exts.find((e) => e.ext === f.ext)) exts.push({ ext: f.ext, count: 0 });
  const ids = [...sel];
  const allSel = items.length > 0 && items.every((x) => sel.has(x.id));

  return (
    <div className="failed">
      <input className="failed-q" value={qInput} onChange={(e) => setQInput(e.target.value)}
        placeholder="按路径或文件名筛选（可以粘贴文件夹路径）" />
      {/* 失败和「仅文件名」分开看：仅文件名是按设置有意跳过内容，不是失败 */}
      <div className="seg">
        <button className={isMeta ? "" : "on"} onClick={() => setF({ ...f, cat: "", ext: "" })}>失败和已加密 {total(failCats)}</button>
        <button className={isMeta ? "on" : ""} onClick={() => setF({ ...f, cat: "meta", ext: "" })}>仅文件名 {total(metaCats)}（不是失败）</button>
      </div>
      {isMeta && (
        <div className="dim small">这些文件按设置只索引了文件名，没有提取内容，不是处理失败。放宽相应设置（文件大小上限、OCR 等）后，
          在「索引」页点「扫描」会自动处理。</div>
      )}
      <div className="failed-facet"><span className="dim">原因</span>
        <div className="chips">
          {chip(isMeta ? "meta" : "", "全部", total(isMeta ? metaCats : failCats))}
          {(isMeta ? metaCats : failCats).filter((c) => c.count > 0 || c.key === f.cat).map((c) => chip(c.key, c.label, c.count))}
        </div>
      </div>
      <div className="failed-facet"><span className="dim">扩展名</span>
        <div className="chips">
          <span className={"chip" + (f.ext === "" ? " on" : "")} onClick={() => setF({ ...f, ext: "" })}>全部</span>
          {exts.map((e) => (
            <span key={e.ext} className={"chip" + (f.ext === e.ext ? " on" : "")} onClick={() => setF({ ...f, ext: f.ext === e.ext ? "" : e.ext })}>
              {e.ext === "." ? "无扩展名" : e.ext} {e.count}
            </span>
          ))}
          {sum.exts.length > EXT_SHOW && (
            <span className="chip" onClick={() => setAllExts(!allExts)}>{allExts ? "收起" : `更多（${sum.exts.length - EXT_SHOW}）`}</span>
          )}
        </div>
      </div>
      {sum.roots.some((r) => r.count > 0 || r.id === f.root) && (
        <div className="failed-facet"><span className="dim">位置</span>
          <div className="chips">
            <span className={"chip" + (!f.root ? " on" : "")} onClick={() => setF({ ...f, root: undefined, dir: undefined })}>全部索引根</span>
            {sum.roots.filter((r) => r.count > 0 || r.id === f.root).map((r) => (
              <span key={r.id} className={"chip" + (f.root === r.id ? " on" : "")} title={r.name}
                onClick={() => setF({ ...f, root: r.id, dir: undefined })}>{r.name} {r.count}</span>
            ))}
          </div>
        </div>
      )}
      {f.root && folders && (() => {
        const root = sum.roots.find((r) => r.id === f.root);
        const base = root?.rel ?? "";
        const rest = folders.dir.length > base.length ? folders.dir.slice(base ? base.length + 1 : 0).split("/") : [];
        const go = (i: number) => setF({ ...f, dir: [base, ...rest.slice(0, i)].filter(Boolean).join("/") || undefined });
        const list = folders.folders.length > 40 && !allFolders ? folders.folders.slice(0, 40) : folders.folders;
        return (
          <div className="failed-facet"><span className="dim">文件夹</span>
            <div className="chips">
              <span className="crumbs">
                <a onClick={() => go(0)}>{root?.name}</a>
                {rest.map((seg, i) => <span key={i}> › <a onClick={() => go(i + 1)}>{seg}</a></span>)}
              </span>
              {folders.here > 0 && folders.folders.length > 0 && <span className="dim small">（直接在这里的 {folders.here} 个）</span>}
              {list.map((x) => (
                <span key={x.name} className="chip" onClick={() => setF({ ...f, dir: (folders.dir ? folders.dir + "/" : "") + x.name })}>📁 {x.name} {x.count}</span>
              ))}
              {folders.folders.length > 40 && (
                <span className="chip" onClick={() => setAllFolders(!allFolders)}>{allFolders ? "收起" : `更多（${folders.folders.length - 40}）`}</span>
              )}
            </div>
          </div>
        );
      })()}
      <div className="btns">
        <button disabled={!shown || isMeta || !!busy}
          title={isMeta ? "这些文件按设置只索引文件名。放宽设置（大小上限、OCR 等）后在「索引」页点「扫描」会自动处理" : "把筛选出的全部文件重新排队处理"}
          onClick={() => { if (confirm(`重新处理「${scope}」中筛选出的 ${shown} 个文件？`)) run(api.retryFiltered(f), (r) => setMsg(`已重新排队 ${r.count} 个文件`), "正在重新排队"); }}>
          {busy === "正在重新排队" && !ids.length ? "正在重试…" : `重试这一类（${shown}）`}</button>
        <button disabled={!shown || !!busy} onClick={() => run(api.exportFailed(f), (r) => setMsg(`已导出 ${r.count} 行：${r.path}`), "正在导出")}>
          {busy === "正在导出" ? "正在导出…" : "导出这一类为 CSV"}</button>
        <button disabled={!ids.length || isMeta || !!busy} onClick={() => run(api.retry(ids), () => setMsg(`已重新排队所选的 ${ids.length} 个文件`), "正在重新排队")}>
          {busy === "正在重新排队" && ids.length ? "正在重试…" : "重试所选"}</button>
        <button disabled={!ids.length} onClick={() => {
          copyText(items.filter((x) => sel.has(x.id)).map((x) => x.path).join("\r\n"));
          setMsg(`已复制所选 ${ids.length} 个文件的路径`);
        }}>复制所选路径</button>
        <button disabled={!ids.length || !!busy} onClick={() => { if (confirm("把所选文件加入排除规则并从索引移除？")) run(api.exclude(ids), undefined, "正在排除"); }}>加入排除规则</button>
        <button disabled={!ids.length || !!busy} title="把所选文件所在的文件夹加入排除规则：文件夹里的全部文件移出索引，以后也不再索引"
          onClick={() => {
            const fs = [...new Set(items.filter((x) => sel.has(x.id)).map((x) => folderOf(x.path)))];
            if (confirm(`把所选文件所在的 ${fs.length} 个文件夹加入排除规则？\n\n${fs.slice(0, 10).join("\n")}${fs.length > 10 ? "\n……" : ""}\n\n文件夹里的全部文件（含子文件夹）会移出索引，以后也不再索引。`))
              run(api.excludeFolders(ids), (r) => setMsg(`已排除 ${r.folders.length} 个文件夹：${r.folders.join("、")}`), "正在排除");
          }}>排除所在文件夹</button>
      </div>
      {msg && <div className="small ok">{msg}</div>}
      {err && <div className="error">{err}</div>}
      <div className="dim small">共 {shown} 个{items.length < shown ? `，已显示 ${items.length} 个` : ""}{ids.length ? `，已选 ${ids.length} 个` : ""}</div>
      {items.length === 0 && !loading && <div className="dim pad">{f.q || f.ext || f.cat ? "没有符合条件的文件。" : "没有失败的文件。"}</div>}
      {items.length > 0 && (
        <table className="grid">
          <thead>
            <tr>
              <th><input type="checkbox" title="全选已显示的" checked={allSel}
                onChange={() => setSel(allSel ? new Set() : new Set(items.map((x) => x.id)))} /></th>
              <th>路径</th>
              <th className="col-why">原因</th>
              <th className="col-act"></th>
            </tr>
          </thead>
          <tbody>
            {items.map((x) => (
              <tr key={x.id}>
                <td><input type="checkbox" checked={sel.has(x.id)} onChange={() => { const n = new Set(sel); n.has(x.id) ? n.delete(x.id) : n.add(x.id); setSel(n); }} /></td>
                <td className="failed-path" title={x.path}><div>{x.name}</div><div className="dim small">{x.path}</div></td>
                <td className="failed-why"><span className={"badge " + (x.state === 2 ? "bad" : "")}>{x.cat}</span> {x.reason}</td>
                <td className="failed-act">
                  <button onClick={() => act(api.open(x.id, "open"))}>打开</button>
                  <button onClick={() => act(api.open(x.id, "folder"))}>所在文件夹</button>
                  <button onClick={() => { copyText(x.path); setMsg("已复制路径：" + x.path); }}>复制路径</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div ref={end} className="dim small pad">
        {more ? (loading ? "正在加载…" : <button onClick={loadMore}>加载更多</button>) : items.length > PAGE ? "已全部显示" : ""}
      </div>
    </div>
  );
}

const lines = (s: string) => s.split("\n").map((x) => x.trim()).filter(Boolean);

function SettingsTab() {
  const [c, setC] = useState<Config | null>(null);
  const [picking, setPicking] = useState(false);
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
      {listField("excludePrefixes", "排除的文件夹（路径前缀，里面的文件和子文件夹都不索引）")}
      <div className="btns"><button onClick={() => setPicking(true)}>选择要排除的文件夹…</button>
        <span className="dim small">选好后点页面底部的「保存设置」生效，已索引的文件会在重新扫描时移出索引</span></div>
      {picking && <FolderPicker title="选择要排除的文件夹（里面的文件不再索引）" onClose={() => setPicking(false)}
        onPick={(p) => { setPicking(false); if (!(c.excludePrefixes ?? []).includes(p)) setC({ ...c, excludePrefixes: [...(c.excludePrefixes ?? []), p] }); }} />}
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
      {num("legacyWorkers", "老格式转换并发数", "只用于内置解析读不了的老格式文件（LibreOffice 转换）")}
      {chk("legacyLOOnly", "老格式只用 LibreOffice 转换，不用内置解析（较慢；仅在内置解析结果有问题时使用）")}
      {chk("ocrEnabled", "识别图片和扫描 PDF 里的文字（OCR）")}
      {num("ocrWorkers", "OCR 进程数", "0 = 自动（推荐）：在这台电脑上用样张实测，选出最快的进程数和每个进程的线程数（通常是每个 CPU 核心一个进程），进程数不超过内存允许的数量。手动设为 N 时，每个进程用「CPU 核心数 ÷ N」个线程")}
      <OCRSpeed />
      {chk("pauseOnBattery", "使用电池时暂停 OCR 和格式转换")}
      {chk("keepAwake", "建立索引期间不让电脑自动睡眠（只在接通电源时；屏幕保护、关闭显示器不受影响；索引完成后自动恢复）")}

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
      <LegacyTest />

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

// 本机 OCR 测速：每台电脑测一次（便携版换了电脑会重新测），按最快的搭配开进程。
function OCRSpeed() {
  const [st, setSt] = useState<OCRBenchInfo | null>(null);
  const [err, setErr] = useState("");
  const load = () => api.ocrBench().then(setSt).catch(() => {});
  useEffect(() => { load(); }, []);
  useEffect(() => {
    if (!st?.running && !st?.asked) return;
    const t = setInterval(load, 1500);
    return () => clearInterval(t);
  }, [st?.running, st?.asked]);
  if (!st || !st.ocr) return null;
  const b = st.bench;
  const plans = b?.plans ?? [];
  const best = b && b.best >= 0 && b.best < plans.length ? plans[b.best] : null;
  const memNote = st.memCap > 0 && st.memCap < st.cores ? `（内存只够开 ${st.memCap} 个 OCR 进程）` : "";
  return (
    <div className="field">
      <div className="dim small">
        {st.manual
          ? `OCR 进程数是手动设置的（现在 ${st.plan}），不测速。`
          : best && b
            ? `本机实测（${new Date(b.at * 1000).toLocaleDateString()}，${b.cpu}，${b.cores} 个物理核心${(b.logical ?? 0) > b.cores ? `、${b.logical} 个逻辑处理器` : ""}）：${best.procs} 个进程 × ${best.threads} 线程最快，约每分钟 ${best.perMin.toFixed(1)} 页 A4 扫描页。现在用 ${st.plan}${memNote}。普通文件还在处理或你正在用电脑时，OCR 只用一半核心。`
            : "还没有测过本机的 OCR 速度：第一次有图片要识别时自动测（约 1～2 分钟，期间普通文件暂停）。"}
      </div>
      {plans.length > 1 && b && (
        <table className="grid">
          <thead><tr><th>搭配</th><th>每分钟页数</th><th>每个进程的内存</th></tr></thead>
          <tbody>
            {plans.map((p, i) => (
              <tr key={i}>
                <td>{p.procs} 个进程 × {p.threads} 线程{p.procs * p.threads > b.budget ? "（用上超线程）" : ""}{i === b.best ? "（最快）" : ""}</td>
                <td className="num">{p.err ? `失败：${p.err}` : p.perMin.toFixed(1)}</td>
                <td className="num">{p.slotMem > 0 ? `${Math.round(p.slotMem / 1048576)} MB` : "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {st.note && <div className="small">{st.note}</div>}
      {!st.manual && (
        <div className="btns">
          <button disabled={st.running || st.asked} onClick={() => { setErr(""); api.rerunOCRBench().then(load).catch((e) => setErr(e.message)); }}>
            {st.running ? "正在测速…" : st.asked ? "等正在识别的文件做完后测速…" : "重新测速"}
          </button>
        </div>
      )}
      {err && <div className="error small">{err}</div>}
    </div>
  );
}

// 老格式测试：抽样比较内置解析和 LibreOffice 的结果。
function LegacyTest() {
  const [st, setSt] = useState<LegacyTestState | null>(null);
  const [err, setErr] = useState("");
  const [perExt, setPerExt] = useState(20);
  const load = () => api.legacyTest().then(setSt).catch(() => {});
  useEffect(() => { load(); }, []);
  useEffect(() => {
    if (!st?.running) return;
    const t = setInterval(load, 1000);
    return () => clearInterval(t);
  }, [st?.running]);
  const r = st?.report;
  const pct = (x: number) => (x * 100).toFixed(1) + "%";
  return (
    <div className="field">
      <div className="dim small">老格式测试：从已索引的老格式文件（doc、xls、ppt、wps、et、dps、rtf 等）中每种随机抽取若干个，
        分别用内置解析和 LibreOffice 提取，比较成功率、用时和文字覆盖率。只统计数字，不改动索引；结果会包含在诊断信息里。</div>
      <div className="btns">
        {st?.running ? (
          <>
            <span>正在测试 {st.done} / {st.total}…</span>
            <button onClick={() => api.cancelLegacyTest().then(load)}>停止</button>
          </>
        ) : (
          <>
            <label className="field inline"><span>每种抽取</span>
              <input type="number" min={1} max={500} value={perExt} onChange={(e) => setPerExt(Number(e.target.value))} /></label>
            <button onClick={() => { setErr(""); api.startLegacyTest(perExt).then(load).catch((e) => setErr(e.message)); }}>开始老格式测试</button>
          </>
        )}
      </div>
      {err && <div className="error small">{err}</div>}
      {r && (
        <>
          <div className="small">{r.canceled ? "上次测试（中途停止）" : "上次测试"}：{new Date(r.startedAt).toLocaleString()}，抽样 {r.sampled} 个文件</div>
          <table className="grid">
            <thead><tr><th>类型</th><th>抽样</th><th>内置成功</th><th>内置不支持</th><th>LibreOffice 成功</th>
              <th>文字覆盖率（平均/最低）</th><th>只有 LibreOffice 成功</th><th>平均用时（内置/LibreOffice）</th></tr></thead>
            <tbody>
              {(r.rows ?? []).map((w) => (
                <tr key={w.ext}>
                  <td>{w.ext}</td><td className="num">{w.files}</td><td className="num">{w.nativeOk}</td><td className="num">{w.needLo}</td>
                  <td className="num">{w.loOk}</td>
                  <td className="num">{w.bothOk > 0 ? `${pct(w.avgCoverage)} / ${pct(w.minCoverage)}` : "—"}{w.lowCoverage > 0 && <span className="error">（{w.lowCoverage} 个低于 90%）</span>}</td>
                  <td className="num">{w.onlyLo}</td>
                  <td className="num">{w.nativeMs.toFixed(0)} ms / {w.loMs.toFixed(0)} ms</td>
                </tr>
              ))}
            </tbody>
          </table>
          {(r.problems ?? []).length > 0 && (
            <details>
              <summary className="small">需要关注的文件（{r.problems!.length} 个）</summary>
              <table className="grid">
                <thead><tr><th>文件</th><th>内置</th><th>LibreOffice</th><th>覆盖率</th></tr></thead>
                <tbody>
                  {r.problems!.map((c) => (
                    <tr key={c.id}>
                      <td className="small">{c.path}</td>
                      <td className="small">{c.native}，{c.nativeChars} 字{c.nativeErr ? "：" + c.nativeErr : ""}</td>
                      <td className="small">{c.lo}，{c.loChars} 字{c.loErr ? "：" + c.loErr : ""}</td>
                      <td className="num">{c.native === "成功" && c.lo === "成功" ? pct(c.coverage) : "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </details>
          )}
          {(r.loCrashes ?? []).length > 0 && (
            <details>
              <summary className="small">LibreOffice 转换时崩溃、内置解析正常的文件（{r.loCrashes!.length} 个，不影响索引）</summary>
              <div className="dim small">这些文件建索引时用的是内置解析的结果，可以正常搜到。只是对照用的 LibreOffice 处理它们时崩溃了（重试一次仍然崩溃）。</div>
              <table className="grid">
                <thead><tr><th>文件</th><th>内置</th><th>LibreOffice</th></tr></thead>
                <tbody>
                  {r.loCrashes!.map((c) => (
                    <tr key={c.id}>
                      <td className="small">{c.path}</td>
                      <td className="small">{c.native}，{c.nativeChars} 字</td>
                      <td className="small">{c.loErr}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </details>
          )}
        </>
      )}
    </div>
  );
}

function Diagnostics() {
  const [anon, setAnon] = useState(true);
  const [st, setSt] = useState<DiagState | null>(null);
  const [err, setErr] = useState("");
  const load = () => api.diagnosticsStatus().then(setSt).catch(() => {});
  useEffect(() => { load(); }, []);
  useEffect(() => {
    if (!st?.running) return;
    const t = setInterval(load, 1000);
    return () => clearInterval(t);
  }, [st?.running]);
  return (
    <div className="field">
      <div className="dim small">遇到问题时导出诊断信息（日志、当前状态、设置、运行环境），把生成的 zip 文件发给开发者。</div>
      <label className="check"><input type="checkbox" checked={anon} onChange={(e) => setAnon(e.target.checked)} />隐藏文件名和路径（替换成代号，保留扩展名和目录层级）</label>
      <div className="btns">
        <button disabled={!!st?.running} onClick={() => {
          setErr("");
          api.diagnostics(anon).then(setSt).catch((e) => setErr(e.message));
        }}>{st?.running ? "正在导出…" : "导出诊断信息"}</button>
      </div>
      {st?.running && <div className="small">正在导出：{st.step}（已用 {st.seconds ?? 0} 秒）</div>}
      {!st?.running && st?.path && <div className="small">已生成：{st.path}</div>}
      {!st?.running && st?.err && <div className="error small">导出失败：{st.err}</div>}
      {err && <div className="error small">{err}</div>}
    </div>
  );
}
