export interface Snippet {
  text: string;
  marks: [number, number][];
  loc?: string;
}

export interface Result {
  id: number;
  name: string;
  ext: string;
  kind: string;
  path: string;
  size: number;
  mtime: number;
  state: number;
  err?: string;
  ocr?: boolean;
  online: boolean;
  volLabel?: string;
  score: number;
  hits: number;
  snippets?: Snippet[];
  nameMarks?: [number, number][];
  dups?: string[];
  fuzzy?: boolean;
}

export interface SearchResponse {
  total: number;
  results: Result[];
  elapsedMs: number;
  truncated?: boolean;
  exact: boolean;
}

export interface PreviewHit {
  s: number;
  e: number;
  loc?: string;
}

export interface Preview {
  id: number;
  name: string;
  path: string;
  text: string;
  truncated?: boolean;
  hits: PreviewHit[];
  nameMarks?: [number, number][];
  ocr?: boolean;
  online: boolean;
  state: number;
  err?: string;
  header?: string;
  kind: string;
}

export interface Counts {
  Total: number;
  Pending: number;
  Done: number;
  Failed: number;
  Encrypted: number;
  MetaOnly: number;
  OCR: number;
}

export interface RootStatus {
  id: number;
  volumeId: string;
  relPath: string;
  paused: boolean;
  path: string;
  label: string;
  kind: string;
  online: boolean;
  scanning: boolean;
  scanned: number;
  counts: Counts;
  lastScan: number;
  opts?: { includeExts?: string[]; excludeGlobs?: string[]; followLinks?: boolean; ocr?: boolean };
  legacyPending: number;
  ocrPending: number;
}

export interface VolumeStatus {
  id: string;
  label: string;
  kind: string;
  online: boolean;
  mount: string;
  lastMount: string;
  lastSeen: number;
  files: number;
  indexBytes: number;
  syncing: boolean;
  capacity: number;
}

export interface CurrentJob {
  id: number;
  path: string;
  kind: string;
  stage: string;
  progress: string;
  seconds: number;
  waiting?: boolean; // 已从队列取出、还在等空闲的处理线程（OCR 进程）
}

export interface SlotStatus {
  used: number;
  cap: number;
}

export interface Status {
  paused: boolean;
  idle: boolean;
  workers: number;
  active: number;
  current: CurrentJob[];
  keepingAwake: boolean;
  ocrThreads: number;
  ocrPagesPerMin: number;
  ocrTuning?: string;
  totals: Counts;
  ocrQueue: number;
  legacyQueue: number;
  ocrEtaSec: number;
  legacyEtaSec: number;
  fastEtaSec: number;
  fastQueue: number;
  legacy: SlotStatus;
  ocr: SlotStatus;
  net: SlotStatus;
  stalledSec: number;
  rebuilding: boolean;
  corrupt: boolean;
  onBattery: boolean;
  scanning: boolean;
  processed: number;
  roots: RootStatus[];
  volumes: VolumeStatus[];
  components: ComponentStatus[] | null;
  componentsChecking: boolean;
  maintenance?: string;
  space: SpaceStatus;
}

export interface SpaceStatus {
  dbBytes: number;
  walBytes: number;
  freeBytes: number;
  incremental: boolean;
  compacting: boolean;
  lastCompact?: string;
}

export interface SpaceItem {
  name: string;
  bytes: number;
  count?: number;
}

export interface SpaceDetail extends SpaceStatus {
  parts: SpaceItem[] | null;
  byExt: SpaceItem[] | null;
  biggest: { fileId: number; bytes: number; chars: number; sharers: number }[] | null;
  biggestPaths: string[] | null;
  texts: number;
  textFiles: number;
  rawChars: number;
  legacy: number;
  fts?: { segments: number; pages: number; largest: number; entries: number; deleted: number; merging: number };
}

export interface HotkeyState {
  hotkey: string;
  active: boolean;
  message: string;
}

export interface ComponentStatus {
  name: string;
  path: string;
  ok: boolean;
  message: string;
  seconds?: number;
}

export interface FailedItem {
  id: number;
  name: string;
  path: string;
  ext: string;
  state: number;
  cat: string; // 原因类别名称
  reason: string;
}

export interface FailedPage {
  items: FailedItem[];
  more: boolean;
}

// 失败列表的筛选：cat 为 "" 时是失败和已加密，"meta" 是全部仅文件名；ext 为 "." 表示没有扩展名
export interface FailedFilter {
  cat: string;
  ext: string;
  q: string;
  root?: number; // 索引根
  dir?: string; // 文件夹（卷内相对路径，含子文件夹）
}

// 失败列表按文件夹查看：dir 下一级子文件夹各有多少，here 是直接放在 dir 里的
export interface FailedFolders {
  dir: string;
  folders: { name: string; count: number }[];
  here: number;
}

export interface ProblemCat {
  key: string;
  label: string;
  meta: boolean;
  count: number;
}

export interface FailedSummary {
  cats: ProblemCat[];
  exts: { ext: string; count: number }[];
  roots: { id: number; name: string; rel: string; count: number }[];
}

export interface Config {
  excludeDirNames: string[];
  excludePrefixes: string[];
  excludeGlobs: string[];
  excludeExts: string[];
  maxTextMB: number;
  maxOtherMB: number;
  followLinks: boolean;
  sniffUnknown: boolean;
  skipCloud: boolean;
  simplify: boolean;
  workers: number;
  timeoutSec: number;
  legacyWorkers: number;
  legacyLOOnly: boolean;
  networkScanMin: number;
  networkReadConc: number;
  pauseOnBattery: boolean;
  keepAwake: boolean;
  containerMaxDepth: number;
  archiveMaxMB: number;
  archiveMaxRatio: number;
  autoDeleteDays: number;
  hotkey: string;
  ocrEnabled: boolean;
  ocrWorkers: number;
  ocrModel?: string; // 认字模型：空为自动，或 v6、v6q（量化版）、v4（旧模型）、win（Windows 自带的 OCR，实验）
  ocrHalfWhenActive: boolean; // 用电脑时放慢建索引（沿用 0.2.5 的名字）
  logLevel: string;
  configVersion: number;
}

export interface LegacyTestRow {
  ext: string;
  files: number;
  nativeOk: number;
  needLo: number;
  loOk: number;
  bothOk: number;
  onlyLo: number;
  onlyNative: number;
  avgCoverage: number;
  minCoverage: number;
  lowCoverage: number;
  nativeMs: number;
  loMs: number;
}

export interface LegacyTestCase {
  id: number;
  path: string;
  ext: string;
  size: number;
  native: string;
  nativeErr?: string;
  nativeChars: number;
  lo: string;
  loErr?: string;
  loChars: number;
  coverage: number;
}

// 诊断信息导出（后台进行）。
export interface DiagState {
  running: boolean;
  step?: string;
  seconds?: number;
  path?: string;
  err?: string;
}

export interface OCRBenchPlan {
  procs: number;
  threads: number;
  perMin: number; // 各轮的中位数
  runs?: number[] | null; // 各轮的每分钟页数
  slotMem: number;
  cpu?: number; // 测速期间子进程实际平均占用几个逻辑处理器（只有 Windows 量得出来）
  err?: string;
}

export interface OCRBenchModel {
  name: string;
  perMin: number;
  agree?: number; // 认出的字和比较搭配时用的模型一致的比例（%）
  err?: string;
}

// 本机的 OCR 测速情况（每台电脑测一次，按电脑保存）。
export interface OCRBenchInfo {
  bench: {
    cpu: string; cores: number; logical?: number; memGB: number; at: number; budget: number; plans: OCRBenchPlan[] | null; best: number;
    planModel?: string; models?: OCRBenchModel[] | null; model?: string; cpuNote?: string;
  } | null;
  model: string; // 现在用的认字模型
  models: string[] | null; // 组件里有的认字模型
  reocr: number; // 用别的认字模型识别过的文件数（可以用现在的模型重新识别）
  reocrUnknown: number; // 其中 0.2.8 以前识别的（没记下模型）
  smtNote?: string; // 为什么用或不用超线程
  planNote?: string; // 现在的搭配没有直接测过时的说明
  running: boolean;
  asked: boolean;
  note?: string;
  manual: boolean;
  ocr: boolean;
  plan: string;
  memCap: number;
  cores: number;
}

export interface OCRTestModel {
  name: string;
  files: number;
  failed: number;
  pages: number;
  chars: number;
  msPage: number;
  agree: number; // 和基准模型认出的字相同的比例（%）
  charsVsBase: number; // 认出的字数比基准多（正）或少（负）百分之几
  lowFiles: number;
  cover: number; // 不看顺序：基准认出的内容它认到了多少（%）
  coverBack: number; // 不看顺序：它认出的内容基准也有多少（%）
  garbled: number; // 疑似认出乱码的文件数
  // 比基准多认出的内容：相邻两字个数；另一个引擎也认到的比例（%，-1 没法比）；像真字的程度（0～100，-1 没法算）；
  // 在字典里出现过的比例（多认出的、两个都认出的、打乱重配的，%）；判断为真内容、像乱码、说不准的文件数
  extra?: number;
  extraOther?: number;
  extraReal?: number;
  dictExtra?: number;
  dictReal?: number;
  dictFake?: number;
  realFiles?: number;
  fakeFiles?: number;
  unsureFiles?: number;
}

export interface OCRTestFile {
  id: number;
  path: string;
  ext: string;
  size: number;
  pages: number;
  items: { model: string; chars: number; ms: number; agree: number; cover?: number; coverBack?: number; garbled?: boolean;
    extra?: number; extraOther?: number; extraReal?: number; verdict?: string; err?: string; sample?: string }[] | null;
}

// OCR 引擎对比的进度和最近一次结果。
export interface OCRTestState {
  running: boolean;
  done: number;
  total: number;
  note?: string;
  report?: {
    startedAt: string;
    finishedAt: string;
    sampled: number;
    canceled?: boolean;
    base: string;
    dictDocs?: number; // 判断多认的内容时，字典读了几份文字
    models: OCRTestModel[] | null;
    files: OCRTestFile[] | null;
  };
}

export interface LegacyTestState {
  running: boolean;
  done: number;
  total: number;
  err?: string;
  report?: {
    startedAt: string;
    finishedAt: string;
    sampled: number;
    canceled?: boolean;
    rows: LegacyTestRow[] | null;
    problems: LegacyTestCase[] | null;
    loCrashes?: LegacyTestCase[] | null;
  };
}

export interface Notice {
  id: number;
  kind: string;
  message: string;
  volume?: string;
  path?: string;
  time: number;
}

export interface FsListing {
  path: string;
  parent: string;
  entries: { name: string; path: string }[];
}

export interface Saved {
  name: string;
  query: string;
}

export interface SearchOpts {
  scope: "all" | "content" | "name";
  sort: "relevance" | "mtime" | "name" | "path";
  fuzzy: boolean;
  cs: boolean;
}

// 「关于」里显示的程序信息。
export interface About {
  version: string; // 如 v0.2.11
  built?: string; // 打包日期
  exe?: string;
  dataDir?: string;
  portable?: boolean;
  started?: string;
  pid?: number;
  indexBytes?: number;
}
