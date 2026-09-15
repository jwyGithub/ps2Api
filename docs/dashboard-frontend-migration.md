# Dashboard 前端迁移方案

## 目标与边界

将 internal/dashboard/static 中的静态 HTML、Fragment 和单体 JavaScript 迁移为可持续扩展的 Vue 单页应用。

迁移要求：

- 保留现有业务功能和 /api/*、/v1/* 接口行为。
- 后端仅调整 SPA 页面加载、静态资源服务和 History 路由回退。
- 登录页与控制台共用一个 index.html，不使用 Vite 多入口。
- 完全移除 Chart.js，不引入其他图表库替代。
- 保持 Go 单二进制发布，Node.js 只参与构建。
- 清除 HTML Fragment、内联事件、全局 window 方法和运行时 CDN 依赖。

## 当前情况

当前前端包含独立 index.html、login.html、多个 fragments/*.html，以及约 1600 行 dashboard.js。状态、接口请求、DOM 渲染和事件处理集中在同一个脚本中。

后端已有完整登录能力：

- POST /api/login：登录并写入签名 Session Cookie。
- POST /api/logout：退出并清理 Session Cookie。
- ADMIN_PASSWORD 设置后开启登录，ADMIN_USERNAME 默认 admin。
- Session 校验和管理接口鉴权继续由后端负责。

## 技术栈

- Vue 3 + TypeScript
- Vite
- Vue Router，HTML5 History 模式
- Pinia
- Tailwind CSS
- SCSS
- shadcn-vue / Reka UI
- GSAP
- lucide-vue-next

不使用：Chart.js、其他图表库、Vite 多入口、Hash 路由、前端 CDN 依赖。

## 路由设计

整个前端只有一个入口 index.html。建议路由：

- /login
- /overview
- /stats
- /request-logs
- /sql
- /waf
- /accounts
- /routing
- /proxies
- /vision
- /api-keys
- /settings

登录页使用独立页面布局，其余页面使用 DashboardLayout。

后端需要让非 API 的前端路由回退到 index.html，支持直接访问和刷新。/api/*、/v1/*、/health 及真实静态资源不能进入 SPA fallback。

## 后端允许改动

1. /login 和控制台页面统一返回 SPA 的 index.html。
2. 提供 Vite 构建后的 assets/* 静态资源。
3. 添加 Vue Router History fallback。
4. 删除 dashboard.js、login.html 和 Fragment 的专用加载逻辑。
5. 调整 go:embed 范围以嵌入嵌套和带哈希的构建产物。
6. Docker 增加前端构建阶段。

不修改登录、Session、数据库、账号池、路由、代理、缓存、WAF、模型调用以及业务接口逻辑。

## 目录结构

~~~text
internal/dashboard/
├── frontend/
│   ├── src/
│   │   ├── api/
│   │   ├── assets/
│   │   ├── components/
│   │   │   ├── ui/
│   │   │   ├── common/
│   │   │   └── motion/
│   │   ├── composables/
│   │   ├── layouts/
│   │   ├── pages/
│   │   ├── router/
│   │   ├── stores/
│   │   ├── styles/
│   │   ├── types/
│   │   ├── utils/
│   │   ├── App.vue
│   │   └── main.ts
│   ├── index.html
│   ├── package.json
│   └── vite.config.ts
└── static/                 # Vite 构建产物，由 Go embed 打包
~~~

static 只保存构建产物，不直接维护业务源码。

## Pinia 设计

按领域拆分 Store：

- auth：登录、退出和 401 处理。
- app：主题、侧边栏和全局 UI 状态。
- accounts：账号、额度、筛选和账号操作。
- stats：统计指标和分析数据。
- requestLogs：日志、分页和详情。
- settings：配置读取与保存。
- waf：分析、探针任务和轮询。

只有跨组件或需要缓存的状态进入 Pinia；表单输入、弹窗开关等局部状态留在组件内。

## API 与鉴权

- 建立统一请求客户端，集中解析后端错误。
- 同源请求携带 Session Cookie。
- 收到 401 时清理前端状态并跳转 /login。
- 登录后返回原目标页或 /overview。
- 不在 localStorage 中保存密码、Session 或管理凭据。
- 请求和响应类型集中维护。

后端当前没有当前用户信息接口。迁移阶段不新增业务 API，通过受保护接口是否返回 401 判断会话有效性。

## 组件和样式

shadcn-vue 用于 Button、Input、Select、Switch、Dialog、Sheet、Dropdown、Tooltip、Table、Tabs、Badge、Skeleton、Pagination、Toast 等基础组件。

Tailwind CSS 负责布局、响应式、间距、尺寸和常规状态。SCSS 负责设计令牌、毛玻璃、复杂渐变、发光边框、噪点、扫描线、SVG 和动画辅助样式。

颜色、圆角、阴影、模糊程度和动画时长必须使用统一设计令牌，避免页面中散落硬编码值。

## 视觉与动画

整体采用深色科技控制台风格：深蓝黑背景，青蓝和紫色强调色，适度毛玻璃、低透明度边框、等宽数值和本地 SVG 数据流元素。

GSAP 仅用于登录与首屏入场、路由切换、Drawer/Dialog、数值变化和 SVG 路径绘制。避免在高频轮询、表格刷新和普通按钮上堆叠动画。

所有动画支持 prefers-reduced-motion，组件卸载时清理 timeline、ticker 和监听器。图标统一使用 lucide-vue-next。

## Chart.js 清理

迁移中删除 Chart.js CDN、依赖、Canvas、实例管理以及图表专用状态和格式化代码。

统计信息改用指标卡片、数据表格、趋势文本、CSS 进度条和状态分布列表。SVG 只用于装饰或简单状态表达，不自行实现复杂图表。

## 构建和交付

开发阶段由 Vite 提供开发服务器，并代理 /api、/v1 和 /health 到本地 Go 服务。

生产构建输出：

~~~text
internal/dashboard/static/
├── index.html
└── assets/
    ├── index-[hash].js
    └── index-[hash].css
~~~

Docker 使用前端构建、Go 构建和运行镜像三个阶段，最终运行镜像不包含 Node.js。

## 实施顺序

1. 创建 Vue 脚手架并安装基础依赖。
2. 配置 Tailwind、SCSS、路由、Pinia、shadcn-vue 和构建输出。
3. 实现 API 客户端、鉴权 Store、登录页和路由守卫。
4. 实现 DashboardLayout、导航与基础组件。
5. 迁移概览和统计页面，同时删除 Chart.js 展示。
6. 迁移账号、API Key、设置等操作页面。
7. 迁移请求日志、SQL、WAF 等复杂页面。
8. 修改 Go 静态资源加载和 History fallback。
9. 修改 Docker 构建流程。
10. 完成功能验收后删除全部旧前端实现。

## 验收标准

- 现有控制台功能可用且业务 API 行为不变。
- 登录、会话失效跳转和退出正常。
- 任意前端路由可直接访问和刷新。
- 不再存在 Chart.js 运行依赖和旧图表实现。
- 不再使用 Fragment、全局 window 方法或内联事件。
- npm build、类型检查及 go test ./... 通过。
- 前端资源成功嵌入 Go 二进制，运行镜像不包含 Node.js。
