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
import { ProjectMembersPage } from '@/features/projects/ProjectMembersPage'
import { AccountSecurityPage } from '@/features/account/AccountSecurityPage'

/**
 * 真实页面（项目、构建、账号、用户与审计）不进 Gate；业务演示页已随 P3
 * 构建闭环下线。非主线页面恒定显示 UnimplementedPage。
 */
export const router = createBrowserRouter([
  { path: '/login', element: <LoginPage /> },
  {
    element: <SessionGate><AppShell /></SessionGate>,
    children: [
      // 真实项目注册表（P2）
      { path: '/', element: <ProjectsPage /> },
      { path: '/projects/new', element: <ProjectWizardPage /> },
      { path: '/projects/:id/members', element: <ProjectMembersPage /> },

      // 真实构建闭环（P3）：列表、详情、取消；AI 诊断尚未实现
      { path: '/builds', element: <BuildListPage /> },
      { path: '/builds/:id', element: <BuildDetailPage /> },
      { path: '/builds/:id/diagnosis', element: <UnimplementedPage title="AI 诊断" /> },

      // 真实账号能力（所有角色）
      { path: '/account', element: <AccountSecurityPage /> },

      // 非主线业务：恒定未实现
      { path: '/deploys', element: <UnimplementedPage title="部署记录" /> },
      { path: '/deploys/:id', element: <UnimplementedPage title="部署详情" /> },
      { path: '/reviews', element: <UnimplementedPage title="审查中心" /> },
      { path: '/reviews/new', element: <UnimplementedPage title="发起审查" /> },
      { path: '/reviews/:id', element: <UnimplementedPage title="审查详情" /> },
      { path: '/scans', element: <UnimplementedPage title="安全扫描" /> },
      { path: '/approvals', element: <UnimplementedPage title="审批中心" /> },
      { path: '/knowledge', element: <UnimplementedPage title="知识库" /> },

      // 管理员入口：真实用户与审计；其余为未实现占位
      {
        element: <SessionGate adminOnly />,
        children: [
          { path: '/deploy-targets', element: <UnimplementedPage title="部署目标" /> },
          { path: '/deploy-targets/new', element: <UnimplementedPage title="新建部署目标" /> },
          { path: '/deploy-targets/:id', element: <UnimplementedPage title="部署目标详情" /> },
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
