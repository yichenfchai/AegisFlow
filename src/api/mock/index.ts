/**
 * Mock API：函数签名对齐后端接口（文档 04）。
 * 后端就绪后：client.ts 中 useMock=false 时走真实 fetch，页面代码零改动。
 */
import { mockApprovals, mockAuditLogs, mockBuilds, mockDefenseEvents, mockDeployTargets, mockDeploys, mockKnowledge, mockProjects, mockReviews, mockScanFindings, mockSecrets } from './data'
import type { Approval, AuditLog, Build, DefenseEvent, Deploy, DeployTarget, KnowledgeEntry, Project, ReviewRequest, ScanFinding, SecretItem } from '../types'

const delay = (ms = 120) => new Promise((r) => setTimeout(r, ms))

export const api = {
  async projects(): Promise<Project[]> {
    await delay()
    return [...mockProjects]
  },

  async builds(): Promise<Build[]> {
    await delay()
    return [...mockBuilds]
  },

  async deploys(): Promise<Deploy[]> {
    await delay()
    return [...mockDeploys]
  },

  async reviews(): Promise<ReviewRequest[]> {
    await delay()
    return [...mockReviews]
  },

  async approvals(): Promise<Approval[]> {
    await delay()
    return [...mockApprovals]
  },

  async knowledge(): Promise<KnowledgeEntry[]> {
    await delay()
    return [...mockKnowledge]
  },

  async auditLogs(): Promise<AuditLog[]> {
    await delay()
    return [...mockAuditLogs]
  },

  async defenseEvents(): Promise<{
    items: DefenseEvent[]
    summary: { todayTotal: number; bandwidthSaved: string; activeBans: number; kbCards: number }
  }> {
    await delay(200)
    return {
      items: [...mockDefenseEvents],
      summary: { todayTotal: 1428, bandwidthSaved: '99.4%', activeBans: 12, kbCards: 13 },
    }
  },

  async secrets(): Promise<SecretItem[]> {
    await delay()
    return [...mockSecrets]
  },

  async deployTargets(): Promise<DeployTarget[]> {
    await delay()
    return [...mockDeployTargets]
  },

  async scanFindings(): Promise<ScanFinding[]> {
    await delay()
    return [...mockScanFindings]
  },
}

export type Api = typeof api
