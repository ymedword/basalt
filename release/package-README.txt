Basalt {{VERSION}}（{{PLATFORM}} 完整包）
==========================================

1. 解压后进入本目录。
2. 首次启动：Windows 运行 `basalt.exe serve`，Linux 运行 `./basalt serve`。
   - 首次启动会在本目录生成 basalt.yaml 与 .basalt/ 工作区；按启动日志补齐
     database.dsn 与 callers.ingress 六项后重新启动，才会生成访问密码。
   - 打开 http://127.0.0.1:8080/console ，用访问密码登录。
   - 访问密码文件（实例私有目录，不在程序目录内）：
       Windows: %LOCALAPPDATA%\basalt\instances\<实例>\admin-token
       Linux:   ${XDG_DATA_HOME:-$HOME/.local/share}/basalt/instances/<实例>/admin-token
     实例 ID 见 .basalt/instance.json 的 instance_id。
3. 随包组件在 runtime/：Node、Python、DBHub、GitHub MCP、Fetch 与自研 docs-readonly。
   首次启动会把三条免凭据 MCP 条目写入 .basalt/mcp_servers.yaml；需要 GitHub 凭据的
   条目只在进程环境存在 GITHUB_PERSONAL_ACCESS_TOKEN 或 GITHUB_TOKEN 时写入。
   配置文件里只出现 ${VAR} 占位，不写凭据值。
4. 第三方组件版本与许可证见 LICENSES/THIRD-PARTY-NOTICES.md；产品许可见 LICENSE。
5. 8080 被占用时先释放端口；监听地址以生成的 basalt.yaml 为准。
6. 首次配置按固定顺序：模型连接 → Agent 绑定与启用 → 知识库（先建集合、摄取，
   再给 Agent 加只读绑定）→ 记忆确认（Flow 演示的 4 条建议须人工确认）→
   flows.enabled → 工作流模板发布 → 手动新建任务。详细清单见运维手册「首次配置清单」
   （docs/Operations.md §1.5）。
7. 托管凭据需要主密钥：首启因缺配置退出后重试会续做开箱 Agent，但不会自动生成
   主密钥；在控制台「高级：实例加密诊断」初始化，或停机执行 basalt secrets init。

数据位置：工作区 .basalt/ 落程序目录；主密钥与访问密码在用户数据目录，不在程序目录。
升级时替换程序与 runtime/，保留 .basalt/ 与用户数据目录。
