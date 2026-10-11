import { useMemo, useState } from "react";

interface Props {
  initial: string;
  onApply: (q: string) => void;
  onClose: () => void;
}

const TYPES = ["文档", "表格", "演示", "PDF", "图片", "文本", "代码", "压缩包", "邮件"];

function quoteIfNeeded(s: string): string {
  return /[\s()]/.test(s) ? `"${s}"` : s;
}

export function AdvancedSearch({ onApply, onClose }: Props) {
  const [f, setF] = useState({
    all: "", phrase: "", any: "", none: "",
    nearA: "", nearB: "", nearN: "10",
    path: "", ext: "", vol: "", nameOnly: "",
    from: "", to: "", smin: "", smax: "",
  });
  const [types, setTypes] = useState<string[]>([]);
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value });

  const query = useMemo(() => {
    const p: string[] = [];
    if (f.all.trim()) p.push(f.all.trim());
    if (f.phrase.trim()) p.push(`"${f.phrase.trim()}"`);
    const any = f.any.trim().split(/\s+/).filter(Boolean);
    if (any.length > 1) p.push(`(${any.join(" OR ")})`);
    else if (any.length === 1) p.push(any[0]);
    if (f.nearA.trim() && f.nearB.trim()) p.push(`${f.nearA.trim()} NEAR/${parseInt(f.nearN) || 10} ${f.nearB.trim()}`);
    for (const w of f.none.trim().split(/\s+/).filter(Boolean)) p.push("-" + w);
    if (f.nameOnly.trim()) p.push("name:" + quoteIfNeeded(f.nameOnly.trim()));
    if (f.path.trim()) p.push("path:" + quoteIfNeeded(f.path.trim()));
    if (types.length) p.push("type:" + types.join(","));
    if (f.ext.trim()) p.push("ext:" + f.ext.trim().replace(/[\s.]+/g, ","));
    if (f.from || f.to) p.push(`modified:${f.from}..${f.to}`);
    if (f.smin && f.smax) p.push(`size:${f.smin}..${f.smax}`);
    else if (f.smin) p.push(`size:>${f.smin}`);
    else if (f.smax) p.push(`size:<${f.smax}`);
    if (f.vol.trim()) p.push("vol:" + quoteIfNeeded(f.vol.trim()));
    return p.join(" ");
  }, [f, types]);

  const row = (label: string, k: keyof typeof f, ph = "") => (
    <label className="adv-row"><span>{label}</span><input value={f[k]} onChange={set(k)} placeholder={ph} /></label>
  );

  return (
    <div className="modal-back" onClick={onClose}>
      <div className="modal adv" onClick={(e) => e.stopPropagation()}>
        <h3>高级搜索</h3>
        <div className="adv-grid">
          {row("包含所有词", "all", "合同 违约金")}
          {row("包含完整短语", "phrase", "不可抗力条款")}
          {row("包含任一词", "any", "报销 差旅")}
          {row("不包含", "none", "草稿")}
          <div className="adv-row">
            <span>邻近</span>
            <div className="adv-near">
              <input value={f.nearA} onChange={set("nearA")} placeholder="甲方" />
              <span>相距不超过</span>
              <input className="w60" value={f.nearN} onChange={set("nearN")} />
              <span>字</span>
              <input value={f.nearB} onChange={set("nearB")} placeholder="违约" />
            </div>
          </div>
          {row("文件名包含", "nameOnly")}
          {row("目录", "path", "D:\\项目\\2025")}
          <div className="adv-row">
            <span>类型</span>
            <div className="chips">
              {TYPES.map((t) => (
                <label key={t} className={"chip" + (types.includes(t) ? " on" : "")}>
                  <input type="checkbox" checked={types.includes(t)} onChange={() => setTypes(types.includes(t) ? types.filter((x) => x !== t) : [...types, t])} />
                  {t}
                </label>
              ))}
            </div>
          </div>
          {row("扩展名", "ext", "xlsx docx")}
          <div className="adv-row">
            <span>修改时间</span>
            <div className="adv-near">
              <input value={f.from} onChange={set("from")} placeholder="2025-01" />
              <span>至</span>
              <input value={f.to} onChange={set("to")} placeholder="2025-06" />
            </div>
          </div>
          <div className="adv-row">
            <span>文件大小</span>
            <div className="adv-near">
              <input value={f.smin} onChange={set("smin")} placeholder="1MB" />
              <span>至</span>
              <input value={f.smax} onChange={set("smax")} placeholder="10MB" />
            </div>
          </div>
          {row("所在磁盘（卷标）", "vol", "备份盘A")}
        </div>
        <div className="adv-preview">
          <span className="dim">等价的查询语句</span>
          <code>{query || "（空）"}</code>
        </div>
        <div className="modal-foot">
          <button onClick={onClose}>取消</button>
          <button className="primary" disabled={!query} onClick={() => onApply(query)}>搜索</button>
        </div>
      </div>
    </div>
  );
}
