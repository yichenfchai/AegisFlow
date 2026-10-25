export interface NavItem {
  label: string
  to: string
  icon: string
  badge?: number
}

export interface NavGroup {
  title: string
  items: NavItem[]
}

/**
 * 侧边栏导航。除项目/构建主线为 BusinessGate 演示外，其余业务页面当前为
 * 明确的未实现占位；账号安全与用户管理（管理员）为真实接口。
 */
export const NAV_GROUPS: NavGroup[] = [
  {
    title: '交付',
    items: [
      { label: '项目列表', to: '/', icon: 'folder_open' },
      { label: '构建流水线', to: '/builds', icon: 'alt_route' },
      { label: '部署记录', to: '/deploys', icon: 'rocket_launch' },
    ],
  },
  {
    title: '质量与安全',
    items: [
      { label: '审查中心', to: '/reviews', icon: 'fact_check' },
      { label: '安全扫描', to: '/scans', icon: 'shield' },
      { label: '审批中心', to: '/approvals', icon: 'how_to_reg' },
      { label: '知识库', to: '/knowledge', icon: 'menu_book' },
    ],
  },
  {
    title: '运行与治理',
    items: [
      { label: '部署目标', to: '/deploy-targets', icon: 'dns' },
      { label: '攻击防御', to: '/defense', icon: 'security' },
      { label: '自动化审计', to: '/audit', icon: 'policy' },
      { label: '自动化策略', to: '/policies', icon: 'rule' },
      { label: '密钥管理', to: '/secrets', icon: 'key' },
    ],
  },
]
