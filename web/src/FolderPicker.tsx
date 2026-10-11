import { useEffect, useState } from "react";
import { api } from "./api";
import type { FsListing } from "./types";

interface Props {
  onPick: (path: string) => void;
  onClose: () => void;
}

export function FolderPicker({ onPick, onClose }: Props) {
  const [cur, setCur] = useState<FsListing | null>(null);
  const [err, setErr] = useState("");
  const [typed, setTyped] = useState("");

  const load = (p: string) =>
    api.fs(p).then((l) => { setCur(l); setTyped(l.path); setErr(""); }).catch((e) => setErr(e.message));

  useEffect(() => { load(""); }, []);

  return (
    <div className="modal-back" onClick={onClose}>
      <div className="modal picker" onClick={(e) => e.stopPropagation()}>
        <h3>选择要建立索引的目录</h3>
        <div className="picker-path">
          <input
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") load(typed); }}
            placeholder="输入路径后回车"
          />
          <button onClick={() => load(typed)}>转到</button>
        </div>
        {err && <div className="error">{err}</div>}
        <div className="picker-list">
          {cur && cur.path && (
            <div className="picker-item" onClick={() => load(cur.parent)}>⬆ 上一级</div>
          )}
          {cur?.entries.map((e) => (
            <div className="picker-item" key={e.path} onClick={() => load(e.path)}>📁 {e.name}</div>
          ))}
          {cur && cur.entries.length === 0 && <div className="dim pad">没有子目录</div>}
        </div>
        <div className="modal-foot">
          <button onClick={onClose}>取消</button>
          <button className="primary" disabled={!cur?.path} onClick={() => cur && onPick(cur.path)}>
            选择此目录
          </button>
        </div>
      </div>
    </div>
  );
}
