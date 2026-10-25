// MCP 目录与诊断的只读调用：真正的 HTTP 出口仍是 api.ts 的 ApiClient——凭据注入、
// 信封解包、401 与 Retry-After 只有那一处实现（约束 45：页面不直接调用 fetch，也
// 不新增第二套客户端）。本文件只声明路径与返回类型，不发请求、不读 DOM。
// Read-only catalog and diagnosis calls. Every request still goes through ApiClient so
// the single HTTP/auth exit stays in api.ts; this module only names paths and types.
import type { ApiClient, MCPCatalog, McpServerDiagnostics } from './api'

/**
 * 只读 GET 的便捷入口。两个端点都不写任何东西：目录是编译期快照，诊断不启动进程、
 * 不连接、不写操作行——因此这里没有任何条件写参数。
 * Two read-only GETs; neither writes anything, so no conditional headers are involved.
 */
export class CatalogApi {
  readonly #client: ApiClient

  constructor(client: ApiClient) {
    this.#client = client
  }

  /** 整份目录（静态集合，无分页、无筛选参数；筛选由页面本地做）。 */
  getMCPCatalog(): Promise<MCPCatalog> {
    return this.#client.request<MCPCatalog>('GET', '/catalog/mcp')
  }

  /** 某条已发布 MCP server 的诊断；未配置或已删除时服务端 404（不返回诊断对象）。 */
  getMCPDiagnostics(name: string): Promise<McpServerDiagnostics> {
    return this.#client.request<McpServerDiagnostics>(
      'GET',
      `/mcp-servers/${encodeURIComponent(name)}/diagnostics`,
    )
  }
}

/** 视图侧便捷构造：一个视图共享一个 ApiClient，不另建连接。 */
export function catalogApi(client: ApiClient): CatalogApi {
  return new CatalogApi(client)
}
