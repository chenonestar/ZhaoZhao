import { useEffect, useMemo, useRef, useState } from "react";
import type { Preview as PV, Result } from "./types";
import { api } from "./api";
import { copyText, folderOf } from "./util";

interface Props {
  result: Result | null;
  data: PV | null;
  loading: boolean;
  hitIdx: number;
  setHitIdx: (n: number) => void;
  onOpen: (action: "open" | "folder") => void;
  msg: string;
}

export function Preview({ result, data, loading, hitIdx, setHitIdx, onOpen, msg }: Props) {
  const cur = useRef<HTMLElement | null>(null);
  const [copied, setCopied] = useState("");

  const parts = useMemo(() => {
    if (!data) return [];
    const cps = Array.from(data.text);
    const out: { t: string; hit: number }[] = [];
    let pos = 0;
    data.hits.forEach((h, i) => {
      if (h.s < pos) return;
      if (h.s > pos) out.push({ t: cps.slice(pos, h.s).join(""), hit: -1 });
      out.push({ t: cps.slice(h.s, h.e).join(""), hit: i });
      pos = h.e;
    });
    if (pos < cps.length) out.push({ t: cps.slice(pos).join(""), hit: -1 });
    return out;
  }, [data]);

  useEffect(() => {
    cur.current?.scrollIntoView({ block: "center" });
  }, [hitIdx, parts]);

  if (!result) return <div className="preview empty">选择左侧的结果查看内容</div>;

  const n = data?.hits.length ?? 0;
  const hit = n > 0 ? data!.hits[Math.min(hitIdx, n - 1)] : null;
  const flash = (s: string) => {
    setCopied(s);
    setTimeout(() => setCopied(""), s.length > 10 ? 5000 : 1200);
  };

  return (
    <div className="preview">
      <div className="pv-head">
        <div className="pv-title" title={result.path}>{result.name}</div>
        <div className="pv-path">{result.path}</div>
        <div className="pv-actions">
          <button onClick={() => onOpen("open")} title="Enter">打开</button>
          <button onClick={() => onOpen("folder")} title="Ctrl+Enter">打开所在文件夹</button>
          <button onClick={() => { copyText(result.path); flash("已复制路径"); }} title="Ctrl+C">复制路径</button>
          <button
            disabled={!result.snippets?.length}
            onClick={() => { copyText(result.snippets![0].text.trim()); flash("已复制片段"); }}
          >复制片段</button>
          <button title="把这个文件所在的文件夹加入排除规则：文件夹里的全部文件移出索引，以后也不再索引"
            onClick={() => {
              const dir = folderOf(result.path);
              if (!confirm(`把「${dir}」加入排除规则？\n\n这个文件夹里的全部文件（含子文件夹）会移出索引，以后也不再索引。\n需要时可以在「设置 → 排除规则」里删掉这一条。`)) return;
              api.excludeFolders([result.id]).then((r) => flash(`已排除 ${r.folders.join("、")}，重新搜索后不再出现`)).catch((e) => flash(e.message));
            }}>排除所在文件夹</button>
          {copied && <span className="flash">{copied}</span>}
        </div>
        <div className="pv-nav">
          {n > 0 ? (
            <>
              <button onClick={() => setHitIdx((hitIdx - 1 + n) % n)} title="Shift+F3">上一处</button>
              <button onClick={() => setHitIdx((hitIdx + 1) % n)} title="F3">下一处</button>
              <span>第 {Math.min(hitIdx, n - 1) + 1} / {n} 处{hit?.loc ? ` · ${hit.loc}` : ""}</span>
            </>
          ) : (
            <span className="dim">{loading ? "加载中…" : "没有需要高亮的命中"}</span>
          )}
          {data?.ocr && <span className="badge ocr">OCR</span>}
        </div>
      </div>
      {msg && <div className="pv-msg">{msg}</div>}
      <div className="pv-body">
        {data?.header && <pre className="mail-head">{data.header}</pre>}
        {data && data.text === "" && (
          <div className="dim pad">
            {data.state === 3 ? "文件已加密，无法提取内容。" : data.err ? `未提取内容：${data.err}` : "没有可预览的文字内容。"}
          </div>
        )}
        <pre className="pv-text">
          {parts.map((p, i) =>
            p.hit >= 0 ? (
              <mark key={i} className={p.hit === Math.min(hitIdx, n - 1) ? "cur" : ""} ref={p.hit === Math.min(hitIdx, n - 1) ? (el) => { cur.current = el; } : undefined}>
                {p.t}
              </mark>
            ) : (
              <span key={i}>{p.t}</span>
            ),
          )}
        </pre>
        {data?.truncated && <div className="dim pad">内容过长，仅显示前 200 万字。</div>}
      </div>
    </div>
  );
}
