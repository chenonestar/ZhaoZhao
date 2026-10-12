import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "./api";
import type { Notice, Preview as PV, Result, Saved, SearchOpts, Status } from "./types";
import { ResultList } from "./ResultList";
import { Preview } from "./Preview";
import { Manage } from "./Manage";
import { AdvancedSearch } from "./AdvancedSearch";
import { copyText, needsEnter } from "./util";

const PAGE = 100;
const OPTS_KEY = "zz.opts";

function loadOpts(): SearchOpts {
  try {
    const o = JSON.parse(localStorage.getItem(OPTS_KEY) || "");
    return { scope: "all", sort: "relevance", fuzzy: false, cs: false, ...o };
  } catch {
    return { scope: "all", sort: "relevance", fuzzy: false, cs: false };
  }
}

type Tab = "index" | "failed" | "settings" | "help";

export function App() {
  const [query, setQuery] = useState("");
  const [opts, setOpts] = useState<SearchOpts>(loadOpts);
  const [results, setResults] = useState<Result[]>([]);
  const [total, setTotal] = useState(0);
  const [elapsed, setElapsed] = useState(0);
  const [note, setNote] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [sel, setSel] = useState(-1);
  const [pv, setPv] = useState<PV | null>(null);
  const [pvLoading, setPvLoading] = useState(false);
  const [hitIdx, setHitIdx] = useState(0);
  const [msg, setMsg] = useState("");
  const [status, setStatus] = useState<Status | null>(null);
  const [panel, setPanel] = useState<Tab | null>(null);
  const [adv, setAdv] = useState(false);
  const [history, setHistory] = useState<string[]>([]);
  const [saved, setSaved] = useState<Saved[]>([]);
  const [showHist, setShowHist] = useState(false);
  const [notices, setNotices] = useState<Notice[]>([]);
  const input = useRef<HTMLInputElement>(null);
  const abort = useRef<AbortController | null>(null);
  const lastNotice = useRef(0);
  const ranQuery = useRef("");

  useEffect(() => { localStorage.setItem(OPTS_KEY, JSON.stringify(opts)); }, [opts]);

  const runSearch = useCallback(async (q: string, o: SearchOpts, offset: number, record: boolean) => {
    abort.current?.abort();
    if (!q.trim()) {
      setResults([]); setTotal(0); setError(""); setNote(""); setSel(-1); setPv(null); setLoading(false);
      ranQuery.current = "";
      return;
    }
    const ac = new AbortController();
    abort.current = ac;
    setLoading(true);
    try {
      const r = await api.search(q, o, offset, PAGE, ac.signal);
      if (ac.signal.aborted) return;
      ranQuery.current = q;
      setError("");
      setTotal(r.total);
      setElapsed(r.elapsedMs);
      setNote(r.truncated ? "候选文件过多，仅校验了一部分，请缩小范围" : "");
      setResults((prev) => (offset === 0 ? r.results : [...prev, ...r.results]));
      if (offset === 0) {
        setSel(r.results.length ? 0 : -1);
        if (record) api.addHistory(q).then(setHistory).catch(() => {});
      }
    } catch (e: any) {
      if (e?.name === "AbortError") return;
      setError(e.message);
      if (offset === 0) { setResults([]); setTotal(0); }
    } finally {
      if (!ac.signal.aborted) setLoading(false);
    }
  }, []);

  // 边输入边搜索（防抖 200 毫秒）；正则需按回车
  useEffect(() => {
    if (needsEnter(query)) return;
    const t = setTimeout(() => runSearch(query, opts, 0, query.trim().length > 1), 200);
    return () => clearTimeout(t);
  }, [query, opts, runSearch]);

  // 预览
  const current = sel >= 0 && sel < results.length ? results[sel] : null;
  useEffect(() => {
    if (!current) { setPv(null); return; }
    let dead = false;
    setPvLoading(true);
    setMsg("");
    api.preview(current.id, ranQuery.current || query, opts)
      .then((d) => { if (!dead) { setPv(d); setHitIdx(0); } })
      .catch(() => { if (!dead) setPv(null); })
      .finally(() => { if (!dead) setPvLoading(false); });
    return () => { dead = true; };
  }, [current?.id, results]); // eslint-disable-line react-hooks/exhaustive-deps

  // 状态与通知
  // 连续几次取不到状态，说明找找主程序已经退出或重启：提示用户，而不是留下一个不会动的界面
  const [lost, setLost] = useState(false);
  const fails = useRef(0);
  const refreshStatus = useCallback(() => api.status()
    .then((s) => { fails.current = 0; setLost(false); setStatus(s); })
    .catch(() => { if (++fails.current >= 2) setLost(true); }), []);
  useEffect(() => {
    refreshStatus();
    const busy = status && !status.idle;
    const t = setInterval(refreshStatus, panel || busy ? 1500 : 6000);
    return () => clearInterval(t);
  }, [refreshStatus, panel, status?.idle]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    api.history().then(setHistory).catch(() => {});
    api.saved().then(setSaved).catch(() => {});
    const poll = () => api.notices(lastNotice.current).then((ns) => {
      if (ns.length) { lastNotice.current = ns[ns.length - 1].id; setNotices((p) => [...p, ...ns]); }
    }).catch(() => {});
    poll();
    const t = setInterval(poll, 5000);
    return () => clearInterval(t);
  }, []);

  const doOpen = useCallback(async (r: Result | null, action: "open" | "folder") => {
    if (!r) return;
    try {
      const res = await api.open(r.id, action);
      setMsg(res.readOnlyCopy ? "已解出到临时目录并打开：这是只读副本，修改不会写回原压缩包或邮件。" : "");
    } catch (e: any) {
      setMsg(e.message);
    }
  }, []);

  // 键盘操作
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const inInput = e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement || e.target instanceof HTMLSelectElement;
      if (panel || adv) { if (e.key === "Escape") { setPanel(null); setAdv(false); } return; }
      if (e.key === "ArrowDown") { e.preventDefault(); setSel((s) => Math.min(results.length - 1, s + 1)); }
      else if (e.key === "ArrowUp") { e.preventDefault(); setSel((s) => Math.max(0, s - 1)); }
      else if (e.key === "Enter" && !(e.target instanceof HTMLButtonElement)) {
        if (e.target === input.current && (ranQuery.current !== query || needsEnter(query))) {
          runSearch(query, opts, 0, true);
          return;
        }
        e.preventDefault();
        doOpen(current, e.ctrlKey || e.metaKey ? "folder" : "open");
      }
      else if ((e.key === "c" || e.key === "C") && (e.ctrlKey || e.metaKey) && current) {
        const sl = window.getSelection()?.toString();
        const inputSel = inInput && (e.target as HTMLInputElement).selectionStart !== (e.target as HTMLInputElement).selectionEnd;
        if (!sl && !inputSel) { e.preventDefault(); copyText(current.path); setMsg("已复制路径"); }
      }
      else if (e.key === "F3") { e.preventDefault(); const n = pv?.hits.length ?? 0; if (n) setHitIdx((h) => (e.shiftKey ? (h - 1 + n) % n : (h + 1) % n)); }
      else if (e.key === "Escape") {
        if (query) { setQuery(""); setShowHist(false); }
        else api.hide().then((r) => { if (!r.hidden) window.close(); }).catch(() => window.close());
      }
      else if (e.key === "f" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); input.current?.focus(); input.current?.select(); }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [results.length, current, query, opts, panel, adv, pv, runSearch, doOpen]);

  useEffect(() => { input.current?.focus(); }, []);
  useEffect(() => {
    const onFocus = () => { input.current?.focus(); input.current?.select(); };
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, []);

  const setOpt = <K extends keyof SearchOpts>(k: K, v: SearchOpts[K]) => setOpts({ ...opts, [k]: v });
  const idleText = status
    ? status.paused ? "索引已暂停" : status.rebuilding ? "正在重建全文索引…" : status.scanning ? "正在扫描目录…"
      : status.totals.Pending > 0 ? `正在建立索引，剩余 ${status.totals.Pending} 个文件` : "索引已是最新"
    : "";
  const failedN = status ? status.totals.Failed : 0;

  return (
    <div className="app">
      <header>
        <div className="brand">找找</div>
        <div className="searchbox">
          <input
            ref={input}
            value={query}
            placeholder="输入要查找的文字；支持 空格(与) OR -排除 &quot;短语&quot; NEAR/n ~模糊 /正则/ path: ext: modified: size:"
            onChange={(e) => { setQuery(e.target.value); setShowHist(false); }}
            onFocus={() => setShowHist(!query)}
            onBlur={() => setTimeout(() => setShowHist(false), 150)}
            spellCheck={false}
          />
          {query && <button className="clear" onClick={() => { setQuery(""); input.current?.focus(); }} title="清空 (Esc)">×</button>}
          {showHist && (history.length > 0 || saved.length > 0) && (
            <div className="dropdown">
              {saved.length > 0 && <div className="dd-title">常用搜索</div>}
              {saved.map((s) => (
                <div className="dd-item" key={s.name} onMouseDown={() => setQuery(s.query)}>
                  <span>⭐ {s.name}</span><code>{s.query}</code>
                  <button onMouseDown={(e) => { e.stopPropagation(); api.unsave(s.name).then(setSaved); }}>删除</button>
                </div>
              ))}
              {history.length > 0 && <div className="dd-title">最近搜索 <button onMouseDown={() => api.clearHistory().then(setHistory)}>清空</button></div>}
              {history.slice(0, 12).map((h) => (
                <div className="dd-item" key={h} onMouseDown={() => setQuery(h)}><span>{h}</span></div>
              ))}
            </div>
          )}
        </div>
        <select value={opts.scope} onChange={(e) => setOpt("scope", e.target.value as SearchOpts["scope"])} title="搜索范围">
          <option value="all">内容和文件名</option>
          <option value="content">仅内容</option>
          <option value="name">仅文件名</option>
        </select>
        <select value={opts.sort} onChange={(e) => setOpt("sort", e.target.value as SearchOpts["sort"])} title="排序">
          <option value="relevance">按相关度</option>
          <option value="mtime">按修改时间</option>
          <option value="name">按文件名</option>
          <option value="path">按路径</option>
        </select>
        <label className="toggle" title="对所有词启用模糊匹配（也可在单个词后加 ~）">
          <input type="checkbox" checked={opts.fuzzy} onChange={(e) => setOpt("fuzzy", e.target.checked)} />模糊
        </label>
        <label className="toggle" title="正则表达式是否区分大小写">
          <input type="checkbox" checked={opts.cs} onChange={(e) => setOpt("cs", e.target.checked)} />区分大小写
        </label>
        <button onClick={() => setAdv(true)}>高级搜索</button>
        <button
          disabled={!query.trim()}
          title="保存为常用搜索"
          onClick={() => { const n = prompt("为这个搜索起个名字：", query.slice(0, 20)); if (n) api.save(n, query).then(setSaved); }}
        >⭐</button>
        <button onClick={() => setPanel("index")}>索引与设置</button>
      </header>

      {notices.filter((n) => n.kind !== "info").slice(-1).map((n) => (
        <div className="notice" key={n.id}>
          <span>{n.message}</span>
          {n.kind === "new-volume" && n.path && (
            <button className="primary" onClick={() => { api.addRoot(n.path!).then(() => { setNotices([]); refreshStatus(); }).catch((e) => setMsg(e.message)); }}>加入索引</button>
          )}
          <button onClick={() => setNotices([])}>忽略</button>
        </div>
      ))}

      <main>
        <section className="left">
          <div className="listhead">
            {error ? <span className="error">{error}</span> : query.trim()
              ? <span>{loading && results.length === 0 ? "搜索中…" : `共 ${total} 个结果 · ${elapsed} 毫秒`}{note && <em className="warn"> · {note}</em>}</span>
              : <span className="dim">在上方输入要查找的文字</span>}
          </div>
          <ResultList
            results={results}
            total={total}
            selected={sel}
            loading={loading}
            onSelect={setSel}
            onOpen={(r) => doOpen(r, "open")}
            onNeedMore={() => runSearch(query, opts, results.length, false)}
          />
        </section>
        <section className="right">
          <Preview result={current} data={pv} loading={pvLoading} hitIdx={hitIdx} setHitIdx={setHitIdx} onOpen={(a) => doOpen(current, a)} msg={msg} />
        </section>
      </main>

      {lost && (
        <div className="lost">
          与找找主程序的连接已断开（程序可能已退出或正在重启）。
          <button className="primary" onClick={() => location.reload()}>重新连接</button>
        </div>
      )}
      <footer onClick={() => setPanel("index")}>
        <span>{idleText}</span>
        {status && failedN > 0 && <span className="warn"> · {failedN} 个文件处理失败</span>}
        {status?.corrupt && <span className="error"> · 索引库可能已损坏，请在「索引与设置」中重建</span>}
        <span className="grow" />
        {status && <span className="dim">共 {status.totals.Total} 个文件</span>}
      </footer>

      {panel && <Manage status={status} tab={panel} setTab={setPanel} onClose={() => setPanel(null)} refresh={refreshStatus} />}
      {adv && <AdvancedSearch initial={query} onClose={() => setAdv(false)} onApply={(q) => { setQuery(q); setAdv(false); }} />}
    </div>
  );
}
