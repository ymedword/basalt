// 模板、每日 Demo 与 MCP 目录的站点数据：与 examples/ 和内置目录一一对应，双语字段。
// Site data for templates, daily demos and the MCP catalog: one entry per examples/
// directory and per bundled catalog recipe, with both language fields.
//
// 单一事实源是仓库里的 examples/README.md 与 internal/catalog/data/mcp_catalog.yaml；
// 本文件只做展示投影，id 必须与两处逐字一致（website/tests/content.test.mjs 判）。
// The source of truth stays in the repository; this file is a display projection and
// content.test.mjs checks that every id matches the source artifacts.

export const DEMOS = [
  {
    id: 'github-radar',
    path: 'examples/daily/github-radar/',
    titleEn: 'GitHub radar',
    titleZh: 'GitHub 雷达',
    bodyEn: 'Star deltas and new projects for a topic, de-duplicated against the previous run.',
    bodyZh: '按主题整理星标变化与新项目，并与上一次运行的结果去重。',
    needsEn: 'GitHub MCP server and a personal access token',
    needsZh: 'GitHub MCP server 与个人访问令牌',
    evidenceEn: 'Fixture replay: de-duplication, star deltas, first run without a baseline',
    evidenceZh: '夹具复跑：去重、星差、首跑无基线'
  },
  {
    id: 'repo-digest',
    path: 'examples/daily/repo-digest/',
    titleEn: 'Repository digest',
    titleZh: '仓库周报',
    bodyEn: 'Pull request, issue and release counts with follow-up items, delivered by its schedule.',
    bodyZh: '统计 PR、Issue 与发布数量，列出待跟进项，由定时任务投递。',
    needsEn: 'GitHub MCP server and a personal access token',
    needsZh: 'GitHub MCP server 与个人访问令牌',
    evidenceEn: 'Fixture replay: counts, unconfirmed fields, cursor advance',
    evidenceZh: '夹具复跑：计数、待确认字段、游标推进'
  },
  {
    id: 'biz-daily',
    path: 'examples/daily/biz-daily/',
    titleEn: 'Business daily',
    titleZh: '业务日报',
    bodyEn: 'Business metrics from a read-only SQLite sample, with data-quality counts.',
    bodyZh: '从只读 SQLite 样例库统计业务指标，并给出数据质量分类计数。',
    needsEn: 'DBHub over SQLite',
    needsZh: '基于 SQLite 的 DBHub',
    evidenceEn: 'Fixture replay: missing, duplicate and invalid rows counted separately',
    evidenceZh: '夹具复跑：缺失、重复、非法值逐类计数'
  },
  {
    id: 'docs-diff',
    path: 'examples/daily/docs-diff/',
    titleEn: 'Document diff',
    titleZh: '文档差异',
    bodyEn: 'Added, modified and deleted documents with a content hash each.',
    bodyZh: '列出新增、修改与删除的文档，并给出每份文档的内容哈希。',
    needsEn: 'Bundled read-only MCP server',
    needsZh: '仓库自带的只读 MCP server',
    evidenceEn: 'Fixture replay: three change sets, content hashes, unchanged run still reports',
    evidenceZh: '夹具复跑：三类变更集合、内容哈希、无变化仍出报告'
  },
  {
    id: 'industry-brief',
    path: 'examples/daily/industry-brief/',
    titleEn: 'Industry brief',
    titleZh: '行业简报',
    bodyEn: 'Briefing with a source table of URL, title, date and access time.',
    bodyZh: '生成行业简报，并附 URL、标题、日期与访问时间组成的来源表。',
    needsEn: 'Brave Search MCP and Fetch MCP',
    needsZh: 'Brave Search MCP 与 Fetch MCP',
    evidenceEn: 'Fixture replay: unfetched URLs stay leads only; both servers invoked',
    evidenceZh: '夹具复跑：未抓正文的 URL 只作线索；两个 server 都被调用'
  }
]

export const TEMPLATES = [
  {
    id: 'weather-brief',
    path: 'examples/templates/weather-brief/',
    titleEn: 'Weather brief',
    titleZh: '天气简报',
    domainEn: 'Personal admin',
    domainZh: '个人事务',
    tier: 'A',
    needsEn: 'Built-in HTTP, no credentials',
    needsZh: '内置 HTTP，无需凭据'
  },
  {
    id: 'arxiv-tracker',
    path: 'examples/templates/arxiv-tracker/',
    titleEn: 'arXiv tracker',
    titleZh: 'arXiv 追踪',
    domainEn: 'Research',
    domainZh: '研究',
    tier: 'A',
    needsEn: 'Built-in HTTP, no credentials',
    needsZh: '内置 HTTP，无需凭据'
  },
  {
    id: 'policy-watch',
    path: 'examples/templates/policy-watch/',
    titleEn: 'Policy watch',
    titleZh: '政策监测',
    domainEn: 'Policy monitoring',
    domainZh: '政策监测',
    tier: 'A',
    needsEn: 'Built-in HTTP to named official sources',
    needsZh: '内置 HTTP，访问指定的官方来源'
  },
  {
    id: 'ticket-insights',
    path: 'examples/templates/ticket-insights/',
    titleEn: 'Ticket insights',
    titleZh: '工单洞察',
    domainEn: 'Support and product',
    domainZh: '支持与产品',
    tier: 'A',
    needsEn: 'DBHub over the sample SQLite database',
    needsZh: '基于样例 SQLite 库的 DBHub'
  },
  {
    id: 'onboarding-buddy',
    path: 'examples/templates/onboarding-buddy/',
    titleEn: 'Onboarding buddy',
    titleZh: '入职助手',
    domainEn: 'HR onboarding',
    domainZh: '人事入职',
    tier: 'A',
    needsEn: 'Bundled read-only MCP server',
    needsZh: '仓库自带的只读 MCP server'
  },
  {
    id: 'dependency-watch',
    path: 'examples/templates/dependency-watch/',
    titleEn: 'Dependency watch',
    titleZh: '依赖监测',
    domainEn: 'Security maintenance',
    domainZh: '安全维护',
    tier: 'A',
    needsEn: 'Built-in HTTP to OSV',
    needsZh: '内置 HTTP，访问 OSV'
  },
  {
    id: 'web-watch',
    path: 'examples/templates/web-watch/',
    titleEn: 'Web watch',
    titleZh: '网页监测',
    domainEn: 'Operations testing',
    domainZh: '运维探测',
    tier: 'A*',
    needsEn: 'Built-in HTTP plus the Playwright browser',
    needsZh: '内置 HTTP，另需 Playwright 浏览器'
  },
  {
    id: 'funding-watch',
    path: 'examples/templates/funding-watch/',
    titleEn: 'Funding watch',
    titleZh: '融资监测',
    domainEn: 'Startup research',
    domainZh: '创业研究',
    tier: 'B',
    needsEn: 'Brave Search and Fetch; needs a Brave API key',
    needsZh: 'Brave Search 与 Fetch；需要 Brave API key'
  }
]

export const CATALOG = [
  {
    id: 'brave-search',
    nameEn: 'Brave Search',
    nameZh: 'Brave Search',
    summaryEn: 'Web and news search for the retrieval step of a daily radar.',
    summaryZh: '检索网页与新闻，适合每日信息雷达的检索段。',
    needsEn: 'BRAVE_API_KEY, Node.js and the server package entry',
    needsZh: 'BRAVE_API_KEY、Node.js 与服务端包入口',
    limitEn: 'The package entry is not exercised here, and the server makes its own outbound calls.',
    limitZh: '包入口尚未实测，且该 server 自行出网，不受 Basalt 白名单约束。'
  },
  {
    id: 'dbhub',
    nameEn: 'DBHub',
    nameZh: 'DBHub',
    summaryEn: 'Read-only SQLite queries for daily reports and stock-taking.',
    summaryZh: '只读 SQLite 查询，适合每日报表与盘点。',
    needsEn: 'Node.js, the DBHub package entry and an absolute sample database path',
    needsZh: 'Node.js、DBHub 包入口与样例库的绝对路径',
    limitEn: 'The sample covers SQLite only; the package entry is not exercised here.',
    limitZh: '样例只覆盖 SQLite；包入口尚未实测。'
  },
  {
    id: 'fetch',
    nameEn: 'Fetch',
    nameZh: 'Fetch',
    summaryEn: 'Fetches a page body and converts it to text, so search results carry content.',
    summaryZh: '抓取指定 URL 正文并转为文本，为检索结果补正文。',
    needsEn: 'Python 3.10+ and the mcp-server-fetch package',
    needsZh: 'Python 3.10+ 与 mcp-server-fetch 包',
    limitEn: 'The package version and tool names are not exercised here.',
    limitZh: '包版本与工具名尚未实测。'
  },
  {
    id: 'filesystem-readonly',
    nameEn: 'Filesystem (read-only)',
    nameZh: 'Filesystem（只读）',
    summaryEn: 'Read-only access to one controlled directory.',
    summaryZh: '只读访问一个受控目录。',
    needsEn: 'Node.js, the filesystem server entry and one allowed root',
    needsZh: 'Node.js、filesystem 服务端入口与一个允许的根目录',
    limitEn: 'The upstream server also exposes write tools; restrict its face to read-only before use.',
    limitZh: '上游 server 也带写工具；接入前必须把工具面收敛到只读。'
  },
  {
    id: 'github',
    nameEn: 'GitHub',
    nameZh: 'GitHub',
    summaryEn: 'Reads repositories, issues and file content for project radars.',
    summaryZh: '读取仓库、Issue 与文件内容，适合项目雷达。',
    needsEn: 'A read-only fine-grained PAT and github-mcp-server on PATH',
    needsZh: '只读细粒度 PAT，以及 PATH 上的 github-mcp-server',
    limitEn: 'The server also ships write tools; use the read-only tool face. Not exercised here.',
    limitZh: '该 server 也提供写工具，应使用只读工具面；尚未实测。'
  },
  {
    id: 'playwright',
    nameEn: 'Playwright',
    nameZh: 'Playwright',
    summaryEn: 'Drives a real browser for pages that need script rendering.',
    summaryZh: '用真实浏览器访问需要脚本渲染的页面。',
    needsEn: 'Node.js, the Playwright MCP entry and a local browser',
    needsZh: 'Node.js、Playwright MCP 入口与本机浏览器',
    limitEn: 'A browser is required; the package entry and tool names are not exercised here.',
    limitZh: '需要浏览器；包入口与工具名尚未实测。'
  }
]

export const TIERS = [
  {
    tier: 'A',
    bodyEn: 'Loads, its built-in tools resolve, its declared MCP tools are a subset of what the server lists, and it replays from fixtures without credentials.',
    bodyZh: '可装载、内置工具可解析、声明的 MCP 工具属于服务端实际清单的子集，且无需凭据即可用夹具复跑。'
  },
  {
    tier: 'A*',
    bodyEn: 'Same as A, but an optional dependency (the Playwright browser) must be present; it is never relabelled as B.',
    bodyZh: '与 A 相同，但需要可选依赖（Playwright 浏览器）；缺依赖时保持 A*，不降级为 B。'
  },
  {
    tier: 'B',
    bodyEn: 'A configuration recipe with dependencies, credentials and known limits. It loads, but it is not claimed to run.',
    bodyZh: '只提供配置配方：依赖、凭据与已知限制写清楚；可装载，但不声称可运行。'
  }
]

export const TABLE_LABELS = {
  en: {
    demo: 'Daily agent',
    produces: 'What it produces',
    needs: 'Needs',
    evidence: 'In-repo evidence',
    template: 'Template',
    domain: 'Domain',
    tier: 'Tier',
    catalog: 'Recipe',
    summary: 'What it is for',
    limit: 'Known limits',
    status: 'Recipe not exercised on this machine',
    index: 'Examples index'
  },
  zh: {
    demo: '每日 Demo',
    produces: '产出什么',
    needs: '需要什么',
    evidence: '仓库内证据',
    template: '模板',
    domain: '领域',
    tier: '档位',
    catalog: '配方',
    summary: '用途',
    limit: '已知限制',
    status: '该配方尚未在本机实测',
    index: '示例索引'
  }
}
