import { useEffect, useMemo, useRef } from "react";
import type { Result } from "./types";
import { KIND_LABEL, fmtSize, fmtTime, splitMarks } from "./util";

const BASE = 62;
const SNIP = 22;

function rowHeight(r: Result): number {
  return BASE + SNIP * (r.snippets?.length ?? 0) + (r.dups?.length ? 22 : 0);
}

interface Props {
  results: Result[];
  total: number;
  selected: number;
  loading: boolean;
  onSelect: (i: number) => void;
  onOpen: (r: Result) => void;
  onNeedMore: () => void;
}

function Marked({ text, marks }: { text: string; marks?: [number, number][] }) {
  if (!marks || marks.length === 0) return <>{text}</>;
  return (
    <>
      {splitMarks(text, marks).map((p, i) => (p.m ? <mark key={i}>{p.t}</mark> : <span key={i}>{p.t}</span>))}
    </>
  );
}

export function ResultList({ results, total, selected, loading, onSelect, onOpen, onNeedMore }: Props) {
  const box = useRef<HTMLDivElement>(null);
  const scrollTop = useRef(0);
  const [, force] = useForce();

  const { offsets, height } = useMemo(() => {
    const o: number[] = [];
    let y = 0;
    for (const r of results) {
      o.push(y);
      y += rowHeight(r);
    }
    return { offsets: o, height: y };
  }, [results]);

  const viewH = box.current?.clientHeight ?? 600;
  const top = scrollTop.current;
  let first = 0;
  {
    let lo = 0, hi = offsets.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (offsets[mid] <= top) lo = mid;
      else hi = mid - 1;
    }
    first = Math.max(0, lo - 3);
  }
  let last = first;
  while (last < results.length && offsets[last] < top + viewH + 400) last++;

  // 选中项滚入可视区域
  useEffect(() => {
    const el = box.current;
    if (!el || selected < 0 || selected >= results.length) return;
    const y = offsets[selected];
    const h = rowHeight(results[selected]);
    if (y < el.scrollTop) el.scrollTop = y;
    else if (y + h > el.scrollTop + el.clientHeight) el.scrollTop = y + h - el.clientHeight;
  }, [selected]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (last >= results.length - 10 && results.length < total && !loading) onNeedMore();
  }, [last, results.length, total, loading]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div
      className="results"
      ref={box}
      onScroll={(e) => {
        scrollTop.current = (e.target as HTMLDivElement).scrollTop;
        force();
      }}
    >
      <div style={{ height, position: "relative" }}>
        {results.slice(first, last).map((r, k) => {
          const i = first + k;
          return (
            <div
              key={r.id}
              className={"row" + (i === selected ? " sel" : "") + (r.online ? "" : " offline")}
              style={{ top: offsets[i], height: rowHeight(r) }}
              onClick={() => onSelect(i)}
              onDoubleClick={() => onOpen(r)}
            >
              <div className="row-title">
                <span className={"kind kind-" + r.kind}>{KIND_LABEL[r.kind] ?? r.kind}</span>
                <span className="name">
                  <Marked text={r.name} marks={r.nameMarks} />
                </span>
                {r.ocr && <span className="badge ocr" title="文字来自 OCR，可能有识别误差">OCR</span>}
                {!r.online && <span className="badge off" title="所在磁盘当前未接入">离线 · {r.volLabel}</span>}
                {r.fuzzy && <span className="badge fz">模糊</span>}
                {r.state === 2 && <span className="badge bad" title={r.err}>失败</span>}
                {r.state === 3 && <span className="badge bad" title={r.err}>已加密</span>}
                {r.hits > 0 && <span className="hits">{r.hits} 处</span>}
              </div>
              <div className="row-path" title={r.path}>{r.path}</div>
              <div className="row-meta">
                {fmtTime(r.mtime)} · {fmtSize(r.size)}
                {r.dups && r.dups.length > 0 && <span className="dups" title={r.dups.join("\n")}> · 另有 {r.dups.length} 个相同副本</span>}
              </div>
              {r.snippets?.map((s, j) => (
                <div className="snippet" key={j}>
                  {s.loc && <span className="loc">{s.loc}</span>}
                  <Marked text={s.text} marks={s.marks} />
                </div>
              ))}
            </div>
          );
        })}
      </div>
      {results.length === 0 && !loading && <div className="empty">没有结果</div>}
    </div>
  );
}

import { useReducer } from "react";
function useForce(): [number, () => void] {
  const [n, f] = useReducer((x: number) => x + 1, 0);
  return [n, f as () => void];
}
