/** 与后端契约的公共类型（源自文档 04） */

export type BuildStatus =
  | 'queued'
  | 'running'
  | 'success'
  | 'failed'
  | 'blocked'
  | 'awaiting_approval'
  | 'cancelled'

export interface Project {
  id: string
  name: string
  localPath: string
  branch: string
  headAligned: boolean
  loaDefault: number
  healthStatus: string
  cpu: string
  memory: string
  status: 'active' | 'paused' | 'archived'
}

export interface Build {
  id: string
  number: number
  projectId: string
  projectName: string
  triggerSource: 'push' | 'cron' | 'manual' | 'rr_merge' | 'retry' | 'defense'
  triggerDetail: string
  branch: string
  commitSha: string
  status: BuildStatus
  blockedStage?: string
  exitCode?: number
  durationMs?: number
  createdAt: number
}

export interface BuildStage {
  seq: number
  name: string
  status: 'pending' | 'running' | 'success' | 'failed' | 'skipped'
  startedAt?: number
  endedAt?: number
}

export interface Deploy {
  id: string
  number: string
  projectId: string
  projectName: string
  imageTag: string
  targetName: string
  buildNumber: number
  status: 'pending' | 'running' | 'success' | 'failed' | 'rolled_back' | 'awaiting_approval'
  healthNote?: string
  durationMs?: number
  createdAt: number
}

export type ReviewStatus = 'pending' | 'reviewing' | 'awaiting_approval' | 'approved' | 'merged' | 'rejected'

export interface ReviewRequest {
  id: string
  number: string
  projectId: string
  projectName: string
  title: string
  sourceBranch: string
  baseBranch: string
  author: string
  additions: number
  deletions: number
  changedFiles: number
  aiSummary?: string
  aiAdoption?: string
  status: ReviewStatus
  createdAt: number
}

export interface Approval {
  id: string
  actionType: string
  title: string
  loa: number
  triggerActor: string
  geoInfo?: string
  evidenceRefs: string[]
  recommended?: string
  countdownSec: number
  autoOnExpire: 'approve' | 'reject'
  status: 'pending' | 'approved' | 'rejected' | 'auto_approved' | 'auto_rejected'
  createdAt: number
}

export interface KnowledgeEntry {
  id: string
  domain: 'build' | 'review' | 'deploy' | 'defense' | 'config'
  title: string
  errorPattern: string
  verifyCount: number
  minVerifications: number
  hitCount: number
  modelSaved: number
  confidence: number
  status: 'candidate' | 'pending_verify' | 'verified' | 'promoted' | 'stale' | 'false_positive'
  updatedAt: number
}

export interface AuditLog {
  id: string
  ts: number
  domain: string
  actor: string
  action: string
  loa: number
  result: 'success' | 'failed' | 'retry' | 'rolled_back' | 'escalated' | 'circuit_broken'
  detail: string
}

export interface DefenseEvent {
  id: string
  ts: number
  sourceIp: string
  asn: string
  ruleId: string
  rateWindow: string
  action: string
  severity: 'high' | 'medium' | 'low'
  status: 'active' | 'expired' | 'unbanned'
}

export interface SecretItem {
  id: string
  name: string
  type: 'ssh_key' | 'registry' | 'env_var'
  scope: string
  fingerprint: string
  linkedTargets: number
  expiresAt?: number
  readCountWeek: number
}

export interface DeployTarget {
  id: string
  name: string
  sshAddr: string
  sshUser: string
  runtimeVersion: string
  remoteWorkdir: string
  healthPass: number
  healthTotal: number
  keepTags: number
  status: 'online' | 'degraded' | 'offline'
  lastSeenAt: number
}

export interface ScanFinding {
  id: string
  severity: 'high' | 'medium' | 'low' | 'nit'
  category: string
  ruleId: string
  title: string
  filePath: string
  line: number
  exempted: boolean
  status: 'open' | 'fixed' | 'exempted'
}

export interface LogLine {
  seq: number
  stream: 'stdout' | 'stderr' | 'system'
  content: string
  ts: number
}
