package builtin

// 本文件是 review ② D-3 / C-53 的**素材侧**判据：五个内置 Demo 的 `AGENT.md` 正文
// 必须教模型"先读系统提示里的来源段再决定跑不跑完整流程"。只注入来源而不告诉模型
// 怎么用，等于把来源段变成一个没人读的标题——这正是 D-3 要防的空转。
// This file pins the prompt-side half of C-53: every bundled demo body must tell the
// model to read the origin block before deciding whether to run the full flow.

import (
	"path"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/examples"
)

// originGuidancePhrases 是正文必须出现的判据句：
//   - 让模型**先看**来源段（而不是把它当背景）；
//   - 即时请求那一支要明说"按消息意图、不自动跑完整流程"；
//   - 明说来源**不是**权限边界（防止模型把它当授权依据）。
//
// The phrases every demo body must carry.
var originGuidancePhrases = []string{
	"先看系统提示里的「## 本次运行来源」",
	"来自用户或业务接口的即时请求",
	"不是**权限边界",
}

// TestBuiltinAgents_RunOriginGuidanceInBody 守五个内置 Demo 的正文都带来源使用指引
// （C-53）：漏一个 Demo，那个 Demo 在手动会话里就可能照跑整套定时业务。
// 逆改点：删掉任一 Demo 正文里的指引段 → 对应子用例的 `Contains` 红。
// 验收点：C-53；review ② D-3。
func TestBuiltinAgents_RunOriginGuidanceInBody(t *testing.T) {
	agents := Agents()
	require.Len(t, agents, 5, "内置 Demo 清单是五个")

	for _, agent := range agents {
		t.Run(agent.Name, func(t *testing.T) {
			// 内嵌素材是安装器唯一读取的来源，判据也读同一份（不另读磁盘副本）。
			// The embedded assets are what the installer copies; assert on the same bytes.
			raw, err := examples.Assets.ReadFile(path.Join("daily", agent.Name, agentDefinitionFile))
			require.NoError(t, err, "内嵌素材里缺 daily/%s/%s", agent.Name, agentDefinitionFile)

			body := string(raw)
			for _, phrase := range originGuidancePhrases {
				require.Contains(t, body, phrase,
					"%s 的 AGENT.md 正文缺少来源使用指引", agent.Name)
			}
			require.Contains(t, body, "定时任务到期触发",
				"%s 的正文必须点名定时触发这一支", agent.Name)
		})
	}
}
