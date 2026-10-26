Basalt {{VERSION}}（{{PLATFORM}} 完整包）
==========================================

1. 解压后进入本目录。
2. 首次启动：Windows 运行 `basalt.exe serve`，Linux 运行 `./basalt serve`。
   - 首次启动会在本目录生成 basalt.yaml 与 .basalt/ 工作区，并自动生成访问密码。
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

数据位置：工作区 .basalt/ 落程序目录；主密钥与访问密码在用户数据目录，不在程序目录。
升级时替换程序与 runtime/，保留 .basalt/ 与用户数据目录。
