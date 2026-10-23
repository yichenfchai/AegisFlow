import { createBrowserRouter, Navigate, type RouteObject } from 'react-router'
import { BusinessGate } from '@/features/auth/BusinessGate'
import { AppShell } from './layout/AppShell'
import { PlaceholderPage } from './PlaceholderPage'
import { DeployListPage } from '@/features/deploys/DeployListPage'
import { DeployTargetListPage, DeployTargetFormPage } from '@/features/deploy-targets/DeployTargetPages'
import { SecretsPage } from '@/features/secrets/SecretsPage'
import { SettingsPage } from '@/features/settings/SettingsPages'
import { PoliciesPage } from '@/features/policies/PoliciesPage'
import { KnowledgePage } from '@/features/knowledge/KnowledgePage'
import { BuildListPage } from '@/features/builds/BuildListPage'
import { BuildDetailPage } from '@/features/builds/BuildDetailPage'
import { DiagnosisPage } from '@/features/builds/DiagnosisPage'
import { ApprovalsPage } from '@/features/approvals/ApprovalsPage'
import { ReviewListPage } from '@/features/reviews/ReviewListPage'
import { ReviewDetailPage } from '@/features/reviews/ReviewDetailPage'
import { ReviewNewPage } from '@/features/reviews/ReviewNewPage'
import { ScansPage } from '@/features/scans/ScansPage'
import { DashboardPage } from '@/features/dashboard/DashboardPage'
import { AuditPage } from '@/features/audit/AuditPage'
import { DefensePage } from '@/features/defense/DefensePage'
import { LoginPage } from '@/features/auth/LoginPage'
import { SessionGate } from '@/features/auth/SessionGate'
import { ProjectWizardPage } from '@/features/projects/ProjectWizardPage'
import { DeployDetailPage } from '@/features/deploys/DeployDetailPage'

/**
 * 路由表（源自文档 05 映射表）
 * 页面按批次实现：占位组件 → 批3 简单页 → 批4 复杂页 → 批5 收尾
 */
// Gate leaf business pages without bypassing the enclosing admin SessionGate.
function businessRoutes(routes: RouteObject[]): RouteObject[] {
  return routes.map(route => {
    if (route.children) return { ...route, children: businessRoutes(route.children) }
    if (route.path === '/login' || route.path === '*') return route
    return { ...route, element: <BusinessGate>{route.element}</BusinessGate> }
  })
}
export const router = createBrowserRouter(businessRoutes([
  { path: '/login', element: <LoginPage /> },
  {
    element: <SessionGate><AppShell /></SessionGate>,
    children: [
      { path: '/', element: <DashboardPage /> },

      // 交付
      { path: '/projects/new', element: <ProjectWizardPage /> },
      { path: '/builds', element: <BuildListPage /> },
      { path: '/builds/:id', element: <BuildDetailPage /> },
      { path: '/builds/:id/diagnosis', element: <DiagnosisPage /> },
      { path: '/deploys', element: <DeployListPage /> },
      { path: '/deploys/:id', element: <DeployDetailPage /> },

      // 质量与安全
      { path: '/reviews', element: <ReviewListPage /> },
      { path: '/reviews/new', element: <ReviewNewPage /> },
      { path: '/reviews/:id', element: <ReviewDetailPage /> },
      { path: '/scans', element: <ScansPage /> },
      { path: '/approvals', element: <ApprovalsPage /> },
      { path: '/knowledge', element: <KnowledgePage /> },

      // Administrator entry gates only. Business action authorization requires future server APIs.
      {
        element: <SessionGate adminOnly />,
        children: [
          { path: '/deploy-targets', element: <DeployTargetListPage /> },
          { path: '/deploy-targets/new', element: <DeployTargetFormPage /> },
          { path: '/deploy-targets/:id', element: <PlaceholderPage title="部署目标详情" batch="批3" /> },
          { path: '/defense', element: <DefensePage /> },
          { path: '/audit', element: <AuditPage /> },
          { path: '/audit/report', element: <PlaceholderPage title="昨夜托管报告" batch="批5" /> },
          { path: '/policies', element: <PoliciesPage /> },
          { path: '/secrets', element: <SecretsPage /> },
          { path: '/settings', element: <SettingsPage /> },
        ],
      },

      // 全局
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
]))
