export function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  const u = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${u[i]}`;
}

export function fmtTime(sec: number): string {
  if (!sec) return "—";
  const d = new Date(sec * 1000);
  const p = (x: number) => String(x).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function fmtDuration(sec: number): string {
  if (sec < 60) return `${Math.max(1, Math.round(sec))} 秒`;
  if (sec < 3600) return `${Math.round(sec / 60)} 分钟`;
  return `${(sec / 3600).toFixed(1)} 小时`;
}

export const KIND_LABEL: Record<string, string> = {
  doc: "文档", sheet: "表格", slide: "演示", pdf: "PDF", image: "图片",
  text: "文本", code: "代码", archive: "压缩包", mail: "邮件", other: "其他",
};

/** 正则和全量扫描需要按回车才执行，避免边输入边扫描。 */
export function needsEnter(q: string): boolean {
  return /(^|[\s(-])\/.+\/(?=$|[\s)])/.test(q);
}

/** 把文本按高亮区间切成片段。marks 为字符（码点）偏移。 */
export function splitMarks(text: string, marks: [number, number][]): { t: string; m: boolean }[] {
  const cps = Array.from(text);
  const out: { t: string; m: boolean }[] = [];
  let pos = 0;
  const sorted = [...marks].sort((a, b) => a[0] - b[0]);
  for (const [s, e] of sorted) {
    if (s < pos) continue;
    if (s > pos) out.push({ t: cps.slice(pos, s).join(""), m: false });
    out.push({ t: cps.slice(s, Math.min(e, cps.length)).join(""), m: true });
    pos = e;
  }
  if (pos < cps.length) out.push({ t: cps.slice(pos).join(""), m: false });
  return out;
}

export function copyText(s: string) {
  if (navigator.clipboard?.writeText) {
    navigator.clipboard.writeText(s).catch(() => legacyCopy(s));
  } else legacyCopy(s);
}

function legacyCopy(s: string) {
  const ta = document.createElement("textarea");
  ta.value = s;
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try {
    document.execCommand("copy");
  } catch {
    /* 忽略 */
  }
  document.body.removeChild(ta);
}

// folderOf：文件所在的文件夹（压缩包里的条目按压缩包所在的文件夹）。
export function folderOf(p: string): string {
  const c = p.split(" › ")[0];
  const i = Math.max(c.lastIndexOf("\\"), c.lastIndexOf("/"));
  return i > 0 ? c.slice(0, i) : c;
}

