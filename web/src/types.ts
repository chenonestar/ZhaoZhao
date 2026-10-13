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
  path: string;
  kind: string;
  stage: string;
  progress: string;
  seconds: number;
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
  totals: Counts;
  ocrQueue: number;
  legacyQueue: number;
  ocrEtaSec: number;
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
  state: number;
  reason: string;
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
  networkScanMin: number;
  networkReadConc: number;
  pauseOnBattery: boolean;
  containerMaxDepth: number;
  archiveMaxMB: number;
  archiveMaxRatio: number;
  autoDeleteDays: number;
  hotkey: string;
  ocrEnabled: boolean;
  ocrWorkers: number;
  logLevel: string;
  configVersion: number;
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
