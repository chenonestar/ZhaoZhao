import type {
  Config, FailedFilter, FailedPage, FailedSummary, FsListing, DiagState, FailedFolders, HotkeyState, LegacyTestState, Notice, OCRBenchInfo, SpaceDetail, Preview, Saved, SearchOpts, SearchResponse, Status,
} from "./types";

export class ApiError extends Error {
  offline?: boolean;
  constructor(msg: string, offline?: boolean) {
    super(msg);
    this.offline = offline;
  }
}

async function call<T>(url: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = { "X-ZhaoZhao": "1" };
  if (init?.body) headers["Content-Type"] = "application/json";
  const r = await fetch(url, { ...init, headers: { ...headers, ...(init?.headers as Record<string, string>) } });
  const text = await r.text();
  let data: any = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    /* 非 JSON 响应 */
  }
  if (!r.ok) throw new ApiError(data?.error || text || `请求失败 (${r.status})`, data?.offline);
  return data as T;
}

const post = <T>(url: string, body?: unknown) =>
  call<T>(url, { method: "POST", body: JSON.stringify(body ?? {}) });

function qs(o: Record<string, string | number | boolean | undefined>): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(o)) {
    if (v === undefined || v === false || v === "") continue;
    p.set(k, v === true ? "1" : String(v));
  }
  return p.toString();
}

export const api = {
  search: (q: string, o: SearchOpts, offset: number, limit: number, signal?: AbortSignal) =>
    call<SearchResponse>(`/api/search?${qs({ q, scope: o.scope, sort: o.sort, fuzzy: o.fuzzy, cs: o.cs, offset, limit })}`, { signal }),
  preview: (id: number, q: string, o: SearchOpts) =>
    call<Preview>(`/api/preview?${qs({ id, q, scope: o.scope, fuzzy: o.fuzzy, cs: o.cs })}`),
  open: (id: number, action: "open" | "folder") => post<{ ok: boolean; readOnlyCopy?: boolean }>("/api/open", { id, action }),
  status: () => call<Status>("/api/status"),
  checkComponents: () => post("/api/components/check"),
  hotkey: () => call<HotkeyState>("/api/hotkey"),
  uiPending: () => call<{ panel: string }>("/api/ui/pending"),
  spaceStats: () => call<SpaceDetail>("/api/space/stats"),
  compact: () => post("/api/space/compact"),
  excludeFolders: (ids: number[]) => post<{ folders: string[] }>("/api/exclude/folders", { ids }),
  ocrBench: () => call<OCRBenchInfo>("/api/ocrbench"),
  rerunOCRBench: () => post("/api/ocrbench/rerun"),
  legacyTest: () => call<LegacyTestState>("/api/legacytest"),
  startLegacyTest: (perExt: number) => post("/api/legacytest/start", { perExt }),
  cancelLegacyTest: () => post("/api/legacytest/cancel"),
  diagnostics: (anonymize: boolean) => post<DiagState>("/api/diagnostics", { anonymize }),
  diagnosticsStatus: () => call<DiagState>("/api/diagnostics/status"),
  pause: (root?: number) => post("/api/pause", { root: root ?? 0 }),
  resume: (root?: number) => post("/api/resume", { root: root ?? 0 }),
  addRoot: (path: string) => post("/api/roots/add", { path }),
  removeRoot: (id: number) => post("/api/roots/remove", { id }),
  rescan: (id?: number) => post("/api/roots/rescan", { id: id ?? 0 }),
  rebuild: (id?: number) => post("/api/roots/rebuild", { id: id ?? 0 }),
  rootOpts: (id: number, opts: unknown) => post("/api/roots/opts", { id, opts }),
  deleteVolume: (id: string) => post("/api/volumes/delete", { id }),
  failed: (f: FailedFilter, after: number, limit = 200) => call<FailedPage>("/api/failed?" + qs({ ...f, after, limit })),
  failedSummary: (f: FailedFilter) => call<FailedSummary>("/api/failed/summary?" + qs({ ...f })),
  failedFolders: (f: FailedFilter) => call<FailedFolders>("/api/failed/folders?" + qs({ ...f })),
  retry: (ids: number[]) => post("/api/failed/retry", { ids }),
  retryFiltered: (filter: FailedFilter) => post<{ count: number }>("/api/failed/retry", { filter }),
  exportFailed: (f: FailedFilter) => post<{ path: string; count: number }>("/api/failed/export", f),
  exclude: (ids: number[]) => post("/api/failed/exclude", { ids }),
  config: () => call<Config>("/api/config"),
  saveConfig: (c: Config) => post<Config>("/api/config", c),
  defaults: () => call<Config>("/api/config/defaults"),
  notices: (after: number) => call<Notice[]>(`/api/notices?after=${after}`),
  dismissNotice: (id: number) => post("/api/notices/dismiss", { id }),
  history: () => call<string[]>("/api/history"),
  addHistory: (query: string) => post<string[]>("/api/history", { query }),
  clearHistory: () => post<string[]>("/api/history", { clear: true }),
  saved: () => call<Saved[]>("/api/saved"),
  save: (name: string, query: string) => post<Saved[]>("/api/saved", { name, query }),
  unsave: (name: string) => post<Saved[]>("/api/saved", { name, delete: true }),
  fs: (path: string) => call<FsListing>(`/api/fs?${qs({ path })}`),
  hide: () => post<{ hidden: boolean }>("/api/hide"),
  quit: () => post("/api/quit"),
};
