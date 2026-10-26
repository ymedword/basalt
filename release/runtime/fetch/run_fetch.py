"""Bundled launcher for the MCP fetch server (mcp-server-fetch 2026.10.24).

Adds the component-local `site-packages` next to this file to `sys.path` so the
bundled interpreter carries no host packages, then hands over to the package's
own entry point. Keep this file dependency-free: it must work with the exact
Python version shipped under `<program dir>/runtime/python`.
"""

import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
_SITE = os.path.join(_HERE, "site-packages")
sys.path.insert(0, _SITE)

# pywin32（mcp 在 Windows 上的依赖）靠自身 .pth 补路径与 DLL 目录，而 .pth 只对
# 解释器自带的 site-packages 生效；随包布局里这里按同一口径手工补一次。
# pywin32 relies on its .pth file, which only runs for the interpreter's own
# site-packages; replicate that setup here for the bundled layout.
if sys.platform == "win32":
    for _sub in ("win32", os.path.join("win32", "lib"), "pythonwin"):
        _path = os.path.join(_SITE, _sub)
        if os.path.isdir(_path):
            sys.path.append(_path)
    _dll_dir = os.path.join(_SITE, "pywin32_system32")
    if os.path.isdir(_dll_dir) and hasattr(os, "add_dll_directory"):
        os.add_dll_directory(_dll_dir)

from mcp_server_fetch import main  # noqa: E402  (path set up above)

if __name__ == "__main__":
    main()
