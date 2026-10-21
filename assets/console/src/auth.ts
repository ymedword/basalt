// 认证状态：token 与 local_trust 标记只存在于页面内存，**不得**写浏览器存储、
// cookie 或 URL，也不进日志（约束 40）。视图通过 subscribe 感知登录态变化。
// Auth state: the token and the local-trust marker live in memory only; nothing is
// persisted, reflected into the URL, or written to a log.

/** 两种认证模式：token 发 Bearer 头，local_trust 只置本地信任标记。 */
// The two console auth modes: bearer token vs. the loopback local-trust marker.
export type AuthMode = 'token' | 'local_trust'

/** 登录态只读快照，供视图渲染；不含 token 本体。 */
// Read-only login snapshot; deliberately carries no token material.
export interface AuthSnapshot {
  readonly mode: AuthMode
  readonly authenticated: boolean
}

type AuthListener = (snapshot: AuthSnapshot) => void

/**
 * ConsoleAuth 只保存内存凭据并产出请求头；清除（401 / 退出）会同时丢弃 token 与
 * 本地信任标记，回到未认证态。
 * ConsoleAuth holds in-memory credentials and produces request headers; clear()
 * drops both the token and the local-trust marker.
 */
export class ConsoleAuth {
  #token: string | null = null
  #localTrust = false
  readonly #listeners = new Set<AuthListener>()

  get mode(): AuthMode {
    return this.#localTrust ? 'local_trust' : 'token'
  }

  get authenticated(): boolean {
    return this.#localTrust || this.#token !== null
  }

  /** 当前登录态快照（每次调用新建对象，视图不得依赖同一引用）。 */
  snapshot(): AuthSnapshot {
    return { mode: this.mode, authenticated: this.authenticated }
  }

  /** 设置 token 模式凭据；空白 token 视为调用方错误，直接拒绝（不进入未认证态）。 */
  // A blank token is a caller error: reject it instead of silently authenticating.
  setToken(token: string): void {
    const trimmed = token.trim()
    if (trimmed === '') {
      throw new Error('token 不能为空')
    }
    this.#token = trimmed
    this.#localTrust = false
    this.#emit()
  }

  /** 置 local_trust 标记；该模式没有 token。 */
  setLocalTrust(): void {
    this.#token = null
    this.#localTrust = true
    this.#emit()
  }

  /**
   * 清除内存凭据（401 / 403 / 退出）。两种模式都回到未认证态；
   * 模式不保留——下次登录由用户在登录页重新选择。
   * Clears everything; the login page picks the mode again from scratch.
   */
  clear(): void {
    this.#token = null
    this.#localTrust = false
    this.#emit()
  }

  /**
   * 生成请求头：token 模式 `Authorization: Bearer <token>`；
   * local_trust 模式 `X-Basalt-Local-Trust: 1`；未认证时为空对象。
   * Produces the auth headers for the current mode, or nothing when unauthenticated.
   */
  headers(): Record<string, string> {
    if (this.#localTrust) {
      return { 'X-Basalt-Local-Trust': '1' }
    }
    if (this.#token !== null) {
      return { Authorization: `Bearer ${this.#token}` }
    }
    return {}
  }

  /** 订阅登录态变化；返回退订函数。 */
  subscribe(listener: AuthListener): () => void {
    this.#listeners.add(listener)
    return () => {
      this.#listeners.delete(listener)
    }
  }

  #emit(): void {
    const snapshot = this.snapshot()
    for (const listener of this.#listeners) {
      listener(snapshot)
    }
  }
}
