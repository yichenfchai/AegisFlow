import { createBrowserRouter, Navigate } from 'react-router'
import { AppShell } from './layout/AppShell'
import { UnimplementedPage } from './UnimplementedPage'
import { BuildListPage } from '@/features/builds/BuildListPage'
import { BuildDetailPage } from '@/features/builds/BuildDetailPage'
import { SettingsPage } from '@/features/settings/SettingsPages'
import { LoginPage } from '@/features/auth/LoginPage'
import { SessionGate } from '@/features/auth/SessionGate'
import { ProjectWizardPage } from '@/features/projects/ProjectWizardPage'
import { ProjectsPage } from '@/features/projects/ProjectsPage'
import { ProjectEditPage } from '@/features/projects/ProjectEditPage'
import { ProjectMembersPage } from '@/features/projects/ProjectMembersPage'
import { AccountSecurityPage } from '@/features/account/AccountSecurityPage'
import { DeploymentListPage } from '@/features/deployments/DeploymentListPage'
import { DeploymentNewPage } from '@/features/deployments/DeploymentNewPage'
import { DeploymentDetailPage } from '@/features/deployments/DeploymentDetailPage'
import { ApprovalListPage } from '@/features/approvals/ApprovalListPage'
import { ReviewListPage } from '@/features/reviews/ReviewListPage'
import { ReviewDetailPage } from '@/features/reviews/ReviewDetailPage'
import { DeploymentTargetsPage } from '@/features/deployments/DeploymentTargetsPage'
import { DeploymentTargetWizardPage } from '@/features/deployments/DeploymentTargetWizardPage'
import { DeploymentTargetDetailPage } from '@/features/deployments/DeploymentTargetDetailPage'

/**
 * 真实页面（项目、构建、部署、审批、审查、账号、用户与审计）不进 Gate；非主线页面
 * 恒定显示 UnimplementedPage，不提供演示数据。
 */
export const router = createBrowserRouter([
  { path: '/login', element: <LoginPage /> },
  {
    element: <SessionGate><AppShell /></SessionGate>,
    children: [
      // 真实项目注册表：列表、创建、编辑与成员
      { path: '/', element: <ProjectsPage /> },
      { path: '/projects/new', element: <ProjectWizardPage /> },
      { path: '/projects/:id/edit', element: <ProjectEditPage /> },
      { path: '/projects/:id/members', element: <ProjectMembersPage /> },

      // 真实构建闭环：列表、详情、取消；AI 诊断尚未接入
      { path: '/builds', element: <BuildListPage /> },
      { path: '/builds/:id', element: <BuildDetailPage /> },
      { path: '/builds/:id/diagnosis', element: <UnimplementedPage title="AI 诊断" /> },

      // 真实账号能力（所有角色）
      { path: '/account', element: <AccountSecurityPage /> },

      // 部署记录：按项目可见性过滤，成员/只读可查看；创建与回滚由服务端按项目写权限再判
      { path: '/deploys', element: <DeploymentListPage /> },
      { path: '/deploys/new', element: <DeploymentNewPage /> },
      { path: '/deploys/:id', element: <DeploymentDetailPage /> },

      // 审查中心：真实接口，全局列表按项目可见性过滤，决策权限由服务端强制
      { path: '/reviews', element: <ReviewListPage /> },
      { path: '/reviews/:id', element: <ReviewDetailPage /> },

      // 非主线业务：恒定未实现
      { path: '/scans', element: <UnimplementedPage title="安全扫描" /> },
      { path: '/knowledge', element: <UnimplementedPage title="知识库" /> },

      // 管理员入口：真实部署目标、审批、用户与审计；其余为未实现占位
      {
        element: <SessionGate adminOnly />,
        children: [
          { path: '/deploy-targets', element: <DeploymentTargetsPage /> },
          { path: '/deploy-targets/new', element: <DeploymentTargetWizardPage /> },
          { path: '/deploy-targets/:id', element: <DeploymentTargetDetailPage /> },
          { path: '/approvals', element: <ApprovalListPage /> },
          { path: '/defense', element: <UnimplementedPage title="攻击防御" /> },
          { path: '/audit', element: <UnimplementedPage title="自动化审计" /> },
          { path: '/audit/report', element: <UnimplementedPage title="昨夜托管报告" /> },
          { path: '/policies', element: <UnimplementedPage title="自动化策略" /> },
          { path: '/secrets', element: <UnimplementedPage title="密钥管理" /> },
          { path: '/settings', element: <SettingsPage /> },
        ],
      },

      // 全局
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
])
