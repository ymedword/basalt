package cli

// 本文件是学习准入命令面（颗粒度文档 §3.1.10、T012）：
//
//	propose  —— 冻结技能候选（写命令：实例锁）
//	evaluate —— 七维确定性评估（写命令：实例锁 + 策略快照）
//	approve / reject —— 决策文件批准 / 拒绝（写命令：实例锁）
//	cancel   —— 取消未发布候选（写命令：实例锁）
//	publish  —— 受控发布（写命令：实例锁 + 策略快照）
//	list     —— 候选列表（只读：不取锁、不校验身份）
//	show     —— 候选详情（只读：不取锁、不校验身份）
//
// 三条纪律：
//   - **身份先于开库**：写命令的操作主体只来自 `Deps.CLIWriteActor`（装配层的
//     `cliWriteActor`），命令面没有 `--approver` 之类的旁路；`cli:unknown` 在取锁与
//     打开数据库之前被拒绝——不打开数据库、不写审计；
//   - **策略快照在锁内读**：`evaluate`（以及 T013 起的 approve / publish）在实例锁内读
//     `<WorkDir>/basalt.yaml` 的 sandbox / shell / execution 与 isolation 段；YAML 缺失
//     或非法在打开数据库之前失败，数据库三张策略表由适配器补齐，缺行即失败、不取空默认；
//   - **正文出口只有两个**：`show --files` 与 `show --diff`（仅本机 CLI）。人类输出不打印
//     候选正文、来源正文、决策理由全文或任何凭证。
//
// The admission command surface. Write commands take the instance lock and reject an
// untrusted actor before any database is opened; reads never lock. File bodies leave the
// process only through show --files / show --diff, and nothing here ever runs a candidate
// script, calls a model or touches the network.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/core"
)

// newAdmissionCommand 组装 `admission` 命令组（propose / list / show / evaluate /
// approve / reject / cancel / publish / withdraw）。
// newAdmissionCommand groups every admission subcommand.
func newAdmissionCommand(deps Deps) *cobra.Command {
	group := &cobra.Command{
		Use:   "admission",
		Short: "技能学习准入：候选冻结、确定性评估、本机批准与受控发布",
		Long: "把人工提议的技能候选变成可冻结、可复算、可人审、可撤回的本机准入通道。\n" +
			"写命令（propose / evaluate / approve / reject / cancel / publish / withdraw）\n" +
			"需要停止服务（实例锁）；list / show 只读，可在服务运行时执行。\n" +
			"准入不执行候选脚本、不调用模型、不发起外部网络请求。\n" +
			"The local admission channel: write commands need the instance lock, reads do not,\n" +
			"and no candidate script, model or network request is ever executed here.",
		Args: cobra.NoArgs,
	}
	group.AddCommand(
		newAdmissionProposeCommand(deps),
		newAdmissionListCommand(deps),
		newAdmissionShowCommand(deps),
		newAdmissionEvaluateCommand(deps),
		newAdmissionApproveCommand(deps),
		newAdmissionRejectCommand(deps),
		newAdmissionCancelCommand(deps),
		newAdmissionPublishCommand(deps),
		newAdmissionWithdrawCommand(deps),
	)
	return group
}

// ─────────────────────────── 公共外壳 ───────────────────────────

// requireAdmissionPort 取准入端口；缺失即报缺件——命令面**不得**自己直连数据库补出
// 第二条读写路径。
// requireAdmissionPort reports a wiring gap instead of opening a second database path.
func requireAdmissionPort(deps Deps, command string) (AdmissionPort, error) {
	if deps.Admission == nil {
		e := fmt.Errorf("学习准入端口未注入——%s 需要装配层注入的 AdmissionPort（internal/app 提供）", command)
		deps.Logger.Error(command + " 未通过：准入端口未注入")
		return nil, e
	}
	return deps.Admission, nil
}

// withAdmissionWrite 是写命令的统一外壳，次序固定：
//
//	端口就绪 → 操作主体（cliWriteActor）→ 可信身份 → 配置 / 工作区存在 → 实例锁 → fn。
//
// 身份与配置检查都先于取锁与开库：不可信主体不碰实例锁、不打开数据库、不写审计。
// The fixed order of every write command: port, identity, configuration, workspace,
// lock, then the action. An untrusted actor never reaches the lock or the database.
func withAdmissionWrite(deps Deps, command string, fn func(actor core.Actor, port AdmissionPort) error) (err error) {
	port, err := requireAdmissionPort(deps, command)
	if err != nil {
		return err
	}
	actor, err := admissionWriteActor(deps, command)
	if err != nil {
		return err
	}
	if _, err := requireDatabaseDSN(deps, command); err != nil {
		return err
	}
	root := filepath.Join(deps.WorkDir, workspaceDirName)
	if _, err := os.Stat(root); err != nil {
		e := fmt.Errorf("工作区不存在（%s）——先运行 basalt init", root)
		deps.Logger.Error(command+" 未通过：工作区不存在", "root", root, "error", err)
		return e
	}
	if deps.AcquireWorkspaceLock == nil {
		e := fmt.Errorf("工作区锁端口未注入，拒绝执行 %s", command)
		deps.Logger.Error(command + " 未通过：工作区锁端口未注入")
		return e
	}
	release, lockErr := deps.AcquireWorkspaceLock(root)
	if lockErr != nil {
		deps.Logger.Error(command+" 未通过：实例锁被占用", "root", root, "error", lockErr)
		return lockErr
	}
	if release == nil {
		e := fmt.Errorf("工作区锁端口未返回释放句柄，拒绝执行 %s", command)
		deps.Logger.Error(command + " 未通过：锁端口未返回释放句柄")
		return e
	}
	defer func() {
		if relErr := release(); relErr != nil {
			deps.Logger.Error(command+" 释放实例锁失败", "root", root, "error", relErr)
			err = errors.Join(err, relErr)
		}
	}()
	return fn(actor, port)
}

// admissionWriteActor 取本次写动作的操作主体并做第一道身份校验：主体必须是
// `cli:<非空>` 且不是 `cli:unknown`，Method 与 Source 都是 `cli`。
// `AdmissionAdmin.checkTrustedActor` 与数据库 CHECK 是另外两道（纵深防御）。
// admissionWriteActor is the first of three identity layers; the orchestrator and the
// database CHECK constraints are the other two.
func admissionWriteActor(deps Deps, command string) (core.Actor, error) {
	if deps.CLIWriteActor == nil {
		e := fmt.Errorf("CLI 操作主体端口未注入——%s 需要装配层的 cliWriteActor", command)
		deps.Logger.Error(command + " 未通过：操作主体端口未注入")
		return core.Actor{}, e
	}
	actor := deps.CLIWriteActor()
	suffix, ok := strings.CutPrefix(actor.Subject, core.ActorSubjectCLIPrefix)
	if !ok || suffix == "" || suffix == "unknown" ||
		actor.Method != core.AuthMethodCLI || actor.Source != core.AuthMethodCLI {
		deps.Logger.Error(command+" 未通过：操作主体不是可信本机 CLI", "subject", actor.Subject)
		return core.Actor{}, core.ErrAdmissionUntrustedActor
	}
	return actor, nil
}

// admissionPolicyConfig 读取 `<WorkDir>/basalt.yaml` 的 sandbox / shell / execution 与
// isolation 段，投影成策略快照输入（数据库三张表由适配器在同一把实例锁内补齐）。
//
// `ProgramDirNames` 留空：生产装配的 `tool.PolicyOptions` 从不填它（安装清单未落地），
// 这里与运行期同口径，policy_hash 才比对得上真正生效的策略。
// 域名白名单**不**从 YAML 读——那只是首次种子，读它会让哈希漏掉管理台之后的修改。
// The YAML supplies the runtime safety surface; the domain allowlist is read from the
// database rows, never from the YAML seed.
func admissionPolicyConfig(deps Deps, command string) (core.AdmissionPolicyConfig, error) {
	path := filepath.Join(deps.WorkDir, configFileName)
	cfg, err := LoadConfig(path, os.LookupEnv)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		e := fmt.Errorf("未发现配置（%s）——%s 需要 sandbox / shell / execution 段", path, command)
		deps.Logger.Error(command+" 未通过：未发现配置", "path", path, "error", e)
		return core.AdmissionPolicyConfig{}, e
	case err != nil:
		// 原错误上抛：装载层已点名键路径与变量名，且不含凭证值（US-1 的口径照用）。
		deps.Logger.Error(command+" 未通过：配置无效", "path", path, "error", err)
		return core.AdmissionPolicyConfig{}, err
	}
	return core.AdmissionPolicyConfig{
		CrossAgentRead:    append([]string(nil), cfg.Sandbox.CrossAgentRead...),
		AllowedCommands:   append([]string(nil), cfg.Shell.AllowedCommands...),
		Interpreters:      append([]string(nil), cfg.Shell.Interpreters...),
		ProgramDirNames:   nil,
		AllowRunCode:      cfg.Execution.AllowRunCode,
		IsolationBackend:  cfg.Isolation.Backend,
		IsolationTools:    admissionCopyTools(cfg.Isolation.Tools),
		IsolationProfiles: admissionIsolationProfiles(cfg.Isolation.Profiles),
		ExecutionProfile:  cfg.Execution.Profile,
	}, nil
}

// admissionIsolationProfiles 把隔离档位投影成策略输入：名字与全部执行边界
// （运行时、镜像、CPU、内存、PID、临时盘、输出上限、超时）。评估 / 批准后修改
// 同名档位会让策略哈希漂移，旧批准发布因此被拒绝；排序由 BuildAdmissionPolicy
// 统一完成，这里不依赖 map 迭代顺序。
// admissionIsolationProfiles projects every isolation profile into the policy input;
// ordering is normalised later by BuildAdmissionPolicy.
func admissionIsolationProfiles(profiles map[string]IsolationProfileConfig) []core.AdmissionIsolationProfile {
	if len(profiles) == 0 {
		return nil
	}
	out := make([]core.AdmissionIsolationProfile, 0, len(profiles))
	for name, profile := range profiles {
		out = append(out, core.AdmissionIsolationProfile{
			Name:           name,
			Runtime:        profile.Runtime,
			Image:          profile.Image,
			CPUs:           profile.CPUs,
			MemoryMB:       profile.MemoryMB,
			PIDs:           profile.PIDs,
			TmpfsMB:        profile.TmpfsMB,
			OutputTmpfsMB:  profile.OutputTmpfsMB,
			MaxOutputBytes: profile.MaxOutputBytes,
			TimeoutSeconds: profile.TimeoutSeconds,
		})
	}
	return out
}

// admissionCopyTools 复制隔离 tools 映射：策略快照不得与配置对象共享可变 map。
// admissionCopyTools keeps the snapshot independent from the loaded configuration.
func admissionCopyTools(tools map[string]string) map[string]string {
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]string, len(tools))
	for name, target := range tools {
		out[name] = target
	}
	return out
}

// writeAdmissionJSON 输出机器可读结果（两空格缩进 + 结尾换行）。
// writeAdmissionJSON emits the machine-readable result with a trailing newline.
func writeAdmissionJSON(cmd *cobra.Command, payload any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

// admissionTime 把时间渲染成 UTC RFC3339；零值渲染为空串（"尚未发生"）。
// admissionTime renders UTC RFC3339 and an empty string for the zero instant.
func admissionTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ─────────────────────────── propose ───────────────────────────

// newAdmissionProposeCommand 组装 `admission propose`：冻结候选目录并写候选行。
// newAdmissionProposeCommand freezes a candidate directory and its database row.
func newAdmissionProposeCommand(deps Deps) *cobra.Command {
	var name, fromDir, sourceFile string
	var asJSON bool
	command := &cobra.Command{
		Use:   "propose --name <skill> --from-dir <dir> --source-file <json>",
		Short: "冻结一个技能候选（不可变目录 + 内容 / 来源哈希）",
		Long: "把 --from-dir 目录里的技能文件与 --source-file 的来源声明冻结为不可变候选。\n" +
			"来源声明是严格 JSON（未知键拒绝、≤64 KiB），凭证扫描命中即拒绝。\n" +
			"写命令需要停止服务（实例锁）。\n" +
			"Freeze a skill candidate from a directory plus a strict-JSON source declaration;\n" +
			"the service must be stopped for the whole command.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(name) == "" {
				return errors.New("propose 需要 --name 指定技能名")
			}
			if strings.TrimSpace(fromDir) == "" {
				return errors.New("propose 需要 --from-dir 指定技能目录")
			}
			if strings.TrimSpace(sourceFile) == "" {
				return errors.New("propose 需要 --source-file 指定来源声明 JSON")
			}
			return withAdmissionWrite(deps, "admission propose", func(actor core.Actor, port AdmissionPort) error {
				files, err := port.ImportFiles(fromDir)
				if err != nil {
					return err
				}
				sourceJSON, err := port.ReadSourceFile(sourceFile)
				if err != nil {
					return err
				}
				candidate, err := port.Propose(cmd.Context(), actor, core.AdmissionProposeRequest{
					SkillName:  name,
					Files:      files,
					SourceJSON: sourceJSON,
				})
				if err != nil {
					return err
				}
				if asJSON {
					return writeAdmissionJSON(cmd, admissionCandidateJSONView(candidate))
				}
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "候选已冻结：%s\n技能：%s\n状态：%s（修订 %d）\n内容哈希：%s\n来源哈希：%s\n提出者：%s\n下一步：basalt admission evaluate %s\n",
					candidate.CandidateID, candidate.SkillName, candidate.State, candidate.Revision,
					candidate.CandidateHash, candidate.SourceHash, candidate.ProposerSubject,
					candidate.CandidateID); err != nil {
					return err
				}
				return nil
			})
		},
	}
	command.Flags().StringVar(&name, "name", "", "技能名（与 AGENT.md 的 skills 声明同名）")
	command.Flags().StringVar(&fromDir, "from-dir", "", "技能源目录（不跟随软链接）")
	command.Flags().StringVar(&sourceFile, "source-file", "", "来源声明严格 JSON 文件（≤64 KiB）")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// ─────────────────────────── list ───────────────────────────

// newAdmissionListCommand 组装 `admission list`：只读，不取实例锁、不校验身份。
// newAdmissionListCommand lists candidates without locking or identity checks.
func newAdmissionListCommand(deps Deps) *cobra.Command {
	var state, skill string
	var limit int
	var asJSON bool
	command := &cobra.Command{
		Use:   "list [--state <state>] [--skill <name>] [--limit <n>]",
		Short: "列出准入候选（只读）",
		Long: "按状态 / 技能名过滤候选；状态取 proposed / evaluated / approved / rejected /\n" +
			"canceled / published / withdrawn，或派生态 expired（已批准但批准已过期）。\n" +
			"只读命令，可在服务运行时执行；不发写操作、不取实例锁。\n" +
			"Read-only listing; it takes no lock, writes no audit row and may run while the\n" +
			"service is up.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			port, err := requireAdmissionPort(deps, "admission list")
			if err != nil {
				return err
			}
			if _, err := requireDatabaseDSN(deps, "admission list"); err != nil {
				return err
			}
			candidates, err := port.List(cmd.Context(), core.AdmissionFilter{
				State: core.AdmissionState(strings.TrimSpace(state)),
				Skill: strings.TrimSpace(skill),
				Limit: limit,
			})
			if err != nil {
				return err
			}
			if asJSON {
				views := make([]admissionCandidateJSON, 0, len(candidates))
				for _, candidate := range candidates {
					views = append(views, admissionCandidateJSONView(candidate))
				}
				return writeAdmissionJSON(cmd, struct {
					Candidates []admissionCandidateJSON `json:"candidates"`
					Count      int                      `json:"count"`
				}{Candidates: views, Count: len(views)})
			}
			out := cmd.OutOrStdout()
			if len(candidates) == 0 {
				_, err := fmt.Fprintln(out, "（没有匹配的候选）")
				return err
			}
			if _, err := fmt.Fprintf(out, "%-28s %-11s %-24s %-8s %s\n",
				"CANDIDATE", "STATE", "SKILL", "RISK", "UPDATED"); err != nil {
				return err
			}
			for _, candidate := range candidates {
				if _, err := fmt.Fprintf(out, "%-28s %-11s %-24s %-8s %s\n",
					candidate.CandidateID, candidate.State, candidate.SkillName,
					candidate.RiskTier, admissionTime(candidate.UpdatedAt)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().StringVar(&state, "state", "", "按状态过滤（含派生态 expired）")
	command.Flags().StringVar(&skill, "skill", "", "按技能名过滤")
	command.Flags().IntVar(&limit, "limit", 50, "返回条数上限（1–200）")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// ─────────────────────────── show ───────────────────────────

// newAdmissionShowCommand 组装 `admission show`：只读详情，可附带文件正文与 diff。
// newAdmissionShowCommand renders one candidate's detail; files and the diff are opt-in
// and only ever printed by the local CLI.
func newAdmissionShowCommand(deps Deps) *cobra.Command {
	var filesFlag, diffFlag, templateFlag, asJSON bool
	command := &cobra.Command{
		Use:   "show <candidate-id> [--files] [--diff] [--decision-template]",
		Short: "查看候选详情（只读）",
		Long: "打印候选、来源声明摘要、评估维度、决策与发布状态。\n" +
			"--files 输出候选文件正文；--diff 输出与当前发布版本的有界 unified diff\n" +
			"（上限 1 MiB，超出截断并标注）；--decision-template 输出可填写的决策 JSON 模板。\n" +
			"只读命令，不取实例锁；人类输出不打印决策理由全文。\n" +
			"Read-only detail; the decision template is generated locally and nothing is written.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := requireAdmissionPort(deps, "admission show")
			if err != nil {
				return err
			}
			if _, err := requireDatabaseDSN(deps, "admission show"); err != nil {
				return err
			}
			detail, err := port.Get(cmd.Context(), args[0], core.AdmissionShowOptions{
				IncludeFiles: filesFlag,
				IncludeDiff:  diffFlag,
			})
			if err != nil {
				return err
			}
			if templateFlag {
				template, err := core.AdmissionDecisionTemplate(detail)
				if err != nil {
					return err
				}
				detail.DecisionTemplate = template
			}
			if asJSON {
				return writeAdmissionJSON(cmd, admissionDetailJSONView(detail))
			}
			return printAdmissionDetail(cmd, detail, templateFlag)
		},
	}
	command.Flags().BoolVar(&filesFlag, "files", false, "输出候选文件正文")
	command.Flags().BoolVar(&diffFlag, "diff", false, "输出与当前发布版本的有界 unified diff")
	command.Flags().BoolVar(&templateFlag, "decision-template", false, "输出决策 JSON 模板")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// printAdmissionDetail 渲染人类可读详情：只给摘要与结构化字段，不回显候选正文、
// 来源正文或决策理由全文（`--files` / `--diff` 是仅有的正文出口）。
// printAdmissionDetail renders the human summary; bodies leave only via --files/--diff.
func printAdmissionDetail(cmd *cobra.Command, detail core.AdmissionDetail, withTemplate bool) error {
	candidate := detail.Candidate
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "候选：%s\n技能：%s\n状态：%s（修订 %d）\n风险档：%s\n内容哈希：%s\n来源哈希：%s\n提出者：%s（%s / %s）\n创建：%s    更新：%s\n",
		candidate.CandidateID, candidate.SkillName, candidate.State, candidate.Revision,
		candidate.RiskTier, candidate.CandidateHash, candidate.SourceHash,
		candidate.ProposerSubject, candidate.ProposerAuthMethod, candidate.ProposerSource,
		admissionTime(candidate.CreatedAt), admissionTime(candidate.UpdatedAt)); err != nil {
		return err
	}
	if candidate.TerminalReason != "" {
		// rejected 的 terminal_reason 就是决策理由，按卡 §3.1.10 不得在人类输出回显全文；
		// canceled / withdrawn 的终态原因是操作者自己传入的 --reason，可以显示。
		// A rejected candidate's terminal reason IS the decision reason, which must not be
		// echoed in human output; cancel / withdraw reasons come from the operator's flag.
		reason := candidate.TerminalReason
		if candidate.State == core.AdmissionRejected {
			reason = "已记录（决策理由不在人类输出中回显全文）"
		}
		if _, err := fmt.Fprintf(out, "终态原因：%s\n", reason); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "来源：kind=%s ref=%s（证据 %d 条、反例 %d 条、局限 %d 条）\n",
		detail.Source.Kind, detail.Source.Ref, len(detail.Source.Evidence),
		len(detail.Source.Counterexamples), len(detail.Source.Limitations)); err != nil {
		return err
	}
	if evaluation := detail.Evaluation; evaluation != nil {
		if _, err := fmt.Fprintf(out, "评估：%s  聚合=%s  风险=%s  评分器=%s\n  策略哈希：%s\n  评估时间：%s\n",
			evaluation.EvaluationID, evaluation.Aggregate, evaluation.RiskTier,
			evaluation.GraderVersion, evaluation.PolicyHash,
			admissionTime(evaluation.EvaluatedAt)); err != nil {
			return err
		}
		for _, dimension := range evaluation.Dimensions {
			reason := dimension.Reason
			if reason != "" {
				reason = "  " + reason
			}
			if _, err := fmt.Fprintf(out, "  %-12s %-14s%s\n", dimension.Name, dimension.Status, reason); err != nil {
				return err
			}
		}
		if len(evaluation.Scripts) > 0 {
			if _, err := fmt.Fprintf(out, "  脚本（批准时须逐条静态审查）：%s\n",
				strings.Join(evaluation.Scripts, ", ")); err != nil {
				return err
			}
		}
	} else {
		if _, err := fmt.Fprintln(out, "评估：（尚未评估）"); err != nil {
			return err
		}
	}
	if decision := detail.Decision; decision != nil {
		// 决策理由全文不在人类输出里回显（可能含候选细节）；JSON 输出保留结构化字段。
		if _, err := fmt.Fprintf(out, "决策：%s  决定=%s  批准者=%s  有效期至=%s\n  理由：已记录（不在人类输出中回显全文）\n",
			decision.DecisionID, decision.Decision, decision.ApproverSubject,
			admissionTime(decision.ExpiresAt)); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(out, "决策：（尚未决策）"); err != nil {
			return err
		}
	}
	if publication := detail.Publication; publication != nil {
		if _, err := fmt.Fprintf(out, "发布：版本 %d（%s）  状态=%s  发布时间=%s\n",
			publication.VersionNo, publication.SkillName, publication.State,
			admissionTime(publication.PublishedAt)); err != nil {
			return err
		}
		if publication.State == core.AdmissionPublicationWithdrawn {
			if _, err := fmt.Fprintf(out, "  撤回：%s  原因=%s  恢复版本=%d\n",
				admissionTime(publication.WithdrawnAt), publication.WithdrawReason,
				publication.WithdrawVersionID); err != nil {
				return err
			}
		}
	} else {
		if _, err := fmt.Fprintln(out, "发布：（尚未发布）"); err != nil {
			return err
		}
	}
	if detail.Files != nil {
		if _, err := fmt.Fprintf(out, "文件（%d）：\n", len(detail.Files)); err != nil {
			return err
		}
		for _, file := range detail.Files {
			if _, err := fmt.Fprintf(out, "--- %s\n%s\n", file.Path, file.Content); err != nil {
				return err
			}
		}
	}
	if detail.Diff != "" {
		if _, err := fmt.Fprintf(out, "diff：\n%s", detail.Diff); err != nil {
			return err
		}
	}
	if withTemplate {
		if _, err := fmt.Fprintf(out, "决策模板：\n%s\n", detail.DecisionTemplate); err != nil {
			return err
		}
	}
	return nil
}

// ─────────────────────────── evaluate ───────────────────────────

// newAdmissionEvaluateCommand 组装 `admission evaluate`：在实例锁内读策略快照并跑
// 七维确定性评估（不执行脚本、不调用模型、不访问网络）。
// newAdmissionEvaluateCommand runs the fixed seven-dimension grader inside the lock.
func newAdmissionEvaluateCommand(deps Deps) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "evaluate <candidate-id>",
		Short: "对候选跑七维确定性评估（写命令）",
		Long: "从磁盘重读候选并复算内容 / 来源哈希，按当前策略快照跑 content / source /\n" +
			"permissions / safety / regression / effect / cost 七维判定。\n" +
			"effect 与 cost 在本 US 固定 not_evaluable——不可评估**不**等于通过。\n" +
			"写命令需要停止服务（实例锁）；不执行候选脚本、不调用模型、不访问网络。\n" +
			"The grader is deterministic and offline; not_evaluable is never a pass.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withAdmissionWrite(deps, "admission evaluate", func(actor core.Actor, port AdmissionPort) error {
				policy, err := admissionPolicyConfig(deps, "admission evaluate")
				if err != nil {
					return err
				}
				evaluation, err := port.Evaluate(cmd.Context(), actor, args[0], policy)
				if err != nil {
					return err
				}
				if asJSON {
					return writeAdmissionJSON(cmd, admissionEvaluationJSONView(evaluation))
				}
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "评估完成：%s\n候选：%s（修订 %d）\n聚合：%s    风险档：%s\n评分器：%s    策略哈希：%s\n",
					evaluation.EvaluationID, evaluation.CandidateID, evaluation.CandidateRevision,
					evaluation.Aggregate, evaluation.RiskTier, evaluation.GraderVersion,
					evaluation.PolicyHash); err != nil {
					return err
				}
				for _, dimension := range evaluation.Dimensions {
					reason := dimension.Reason
					if reason != "" {
						reason = "  " + reason
					}
					if _, err := fmt.Fprintf(out, "  %-12s %-14s%s\n", dimension.Name, dimension.Status, reason); err != nil {
						return err
					}
				}
				if len(evaluation.Scripts) > 0 {
					if _, err := fmt.Fprintf(out, "脚本（批准时须逐条静态审查）：%s\n",
						strings.Join(evaluation.Scripts, ", ")); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprintf(out, "下一步：basalt admission show %s --decision-template\n",
					evaluation.CandidateID); err != nil {
					return err
				}
				return nil
			})
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// ─────────────────────────── approve / reject ───────────────────────────

// newAdmissionApproveCommand 组装 `admission approve`：用严格 JSON 决策文件批准候选。
// 决策文件在端口内读取（≤64 KiB、普通文件、不跟随软链接）；批准者只来自 cliWriteActor，
// 命令面没有 `--approver` 之类旁路。策略快照在实例锁内读取——批准与评估必须绑定
// 同一策略哈希。
// newAdmissionApproveCommand approves with a strict-JSON decision file; the approver can
// only come from cliWriteActor, and the policy snapshot is taken inside the lock.
func newAdmissionApproveCommand(deps Deps) *cobra.Command {
	var decisionFile string
	var asJSON bool
	command := &cobra.Command{
		Use:   "approve <candidate-id> --decision-file <path>",
		Short: "用决策文件批准候选（写命令）",
		Long: "决策文件是严格 JSON（未知键拒绝、≤64 KiB、凭证扫描）：必须绑定候选修订 /\n" +
			"内容哈希、评估 ID / 哈希与基线；批准有效期上限 24 小时。高风险候选要求提出\n" +
			"者与批准者不是同一个本机身份。写命令需要停止服务（实例锁）。\n" +
			"Approve with a strict-JSON decision file bound to the candidate revision, hashes\n" +
			"and baseline; the approval lasts at most 24 hours.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(decisionFile) == "" {
				return errors.New("approve 需要 --decision-file 指定决策 JSON")
			}
			return withAdmissionWrite(deps, "admission approve", func(actor core.Actor, port AdmissionPort) error {
				policy, err := admissionPolicyConfig(deps, "admission approve")
				if err != nil {
					return err
				}
				decisionJSON, err := port.ReadDecisionFile(decisionFile)
				if err != nil {
					return err
				}
				decision, err := port.Approve(cmd.Context(), actor, args[0], decisionJSON, policy)
				if err != nil {
					return err
				}
				if asJSON {
					return writeAdmissionJSON(cmd, admissionDecisionJSONView(decision))
				}
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "已批准：%s\n候选：%s（修订 %d）\n批准者：%s\n有效期至：%s\n下一步：basalt admission publish %s\n",
					decision.DecisionID, decision.CandidateID, decision.CandidateRevision,
					decision.ApproverSubject, admissionTime(decision.ExpiresAt),
					decision.CandidateID); err != nil {
					return err
				}
				return nil
			})
		},
	}
	command.Flags().StringVar(&decisionFile, "decision-file", "", "决策严格 JSON 文件（≤64 KiB）")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// newAdmissionRejectCommand 组装 `admission reject`：拒绝走同一身份、哈希与绑定校验，
// 但不要求 approved_scope / baseline / expires_at / script_review。
// newAdmissionRejectCommand shares the identity and binding checks but takes no policy
// snapshot and no approval scope.
func newAdmissionRejectCommand(deps Deps) *cobra.Command {
	var decisionFile string
	var asJSON bool
	command := &cobra.Command{
		Use:   "reject <candidate-id> --decision-file <path>",
		Short: "用决策文件拒绝候选（写命令）",
		Long: "决策文件是严格 JSON：必须绑定候选修订 / 内容哈希与评估行；reason 必填、\n" +
			"≤2000 字符并扫描凭证。从 approved 发起即撤回批准，旧批准置 superseded。\n" +
			"写命令需要停止服务（实例锁）。\n" +
			"Reject binds the same hashes; rejecting an approved candidate withdraws it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(decisionFile) == "" {
				return errors.New("reject 需要 --decision-file 指定决策 JSON")
			}
			return withAdmissionWrite(deps, "admission reject", func(actor core.Actor, port AdmissionPort) error {
				decisionJSON, err := port.ReadDecisionFile(decisionFile)
				if err != nil {
					return err
				}
				decision, err := port.Reject(cmd.Context(), actor, args[0], decisionJSON)
				if err != nil {
					return err
				}
				if asJSON {
					return writeAdmissionJSON(cmd, admissionDecisionJSONView(decision))
				}
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "已拒绝：%s\n候选：%s（修订 %d）\n拒绝者：%s\n",
					decision.DecisionID, decision.CandidateID, decision.CandidateRevision,
					decision.ApproverSubject); err != nil {
					return err
				}
				return nil
			})
		},
	}
	command.Flags().StringVar(&decisionFile, "decision-file", "", "决策严格 JSON 文件（≤64 KiB）")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// ─────────────────────────── cancel ───────────────────────────

// newAdmissionCancelCommand 组装 `admission cancel`：取消未发布候选（终态，不复活）。
// newAdmissionCancelCommand cancels an unpublished candidate; the state is terminal.
func newAdmissionCancelCommand(deps Deps) *cobra.Command {
	var reason string
	var asJSON bool
	command := &cobra.Command{
		Use:   "cancel <candidate-id> --reason <text>",
		Short: "取消未发布候选（写命令）",
		Long: "取消是终态：同一技能要重新准入必须新建候选。理由必填、≤2000 字符并扫描凭证；\n" +
			"已发布 / 已撤回候选拒绝取消。写命令需要停止服务（实例锁）。\n" +
			"Cancel is terminal; published or withdrawn candidates refuse cancellation.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" {
				return errors.New("cancel 需要 --reason 说明取消原因")
			}
			return withAdmissionWrite(deps, "admission cancel", func(actor core.Actor, port AdmissionPort) error {
				candidate, err := port.Cancel(cmd.Context(), actor, args[0], reason)
				if err != nil {
					return err
				}
				if asJSON {
					return writeAdmissionJSON(cmd, admissionCandidateJSONView(candidate))
				}
				out := cmd.OutOrStdout()
				_, err = fmt.Fprintf(out, "已取消：%s\n技能：%s\n状态：%s（修订 %d）\n",
					candidate.CandidateID, candidate.SkillName, candidate.State, candidate.Revision)
				return err
			})
		},
	}
	command.Flags().StringVar(&reason, "reason", "", "取消原因（≤2000 字符）")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// ─────────────────────────── publish ───────────────────────────

// newAdmissionPublishCommand 组装 `admission publish`：复用既有技能发布事务生效，
// 策略快照在实例锁内读取；发布复核基线 / 策略 / 风险档三方一致。
// newAdmissionPublishCommand reuses the existing skill publication transaction; the
// policy snapshot is taken inside the lock and the baseline/policy/risk trio is
// re-checked before anything is written.
func newAdmissionPublishCommand(deps Deps) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "publish <candidate-id>",
		Short: "发布已批准候选（写命令）",
		Long: "发布重新读取并复算内容 / 来源哈希、基线与策略哈希、风险档，逐字相等才进入\n" +
			"单事务：版本行 + 发布指针 + 发布记录 + 候选状态 + 操作终态；任一失败整体回滚，\n" +
			"候选保持 approved。写命令需要停止服务（实例锁）。\n" +
			"Publish re-checks every binding, then writes version, pointer, publication,\n" +
			"candidate state and the operation terminal in one transaction.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withAdmissionWrite(deps, "admission publish", func(actor core.Actor, port AdmissionPort) error {
				policy, err := admissionPolicyConfig(deps, "admission publish")
				if err != nil {
					return err
				}
				publication, err := port.Publish(cmd.Context(), actor, args[0], policy)
				if err != nil {
					return err
				}
				if asJSON {
					return writeAdmissionJSON(cmd, admissionPublicationJSONView(publication))
				}
				out := cmd.OutOrStdout()
				_, err = fmt.Fprintf(out, "已发布：%s 版本 %d（%s）\n状态：%s\n发布时间：%s\n",
					publication.SkillName, publication.VersionNo, publication.SkillUID,
					publication.State, admissionTime(publication.PublishedAt))
				return err
			})
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// ─────────────────────────── withdraw ───────────────────────────

// newAdmissionWithdrawCommand 组装 `admission withdraw`：撤回已发布候选。指针仍指向
// 本次发布时把上一版内容作为**新版本**重新发布；被后续手工发布超越或技能已删除时
// 只记录撤回、不移动指针；首版撤回到空拒绝。写命令需要停止服务（实例锁）。
// newAdmissionWithdrawCommand withdraws a published candidate: the previous content is
// re-published as a new version when the pointer still names this publication.
func newAdmissionWithdrawCommand(deps Deps) *cobra.Command {
	var reason string
	var asJSON bool
	command := &cobra.Command{
		Use:   "withdraw <candidate-id> --reason <text>",
		Short: "撤回已发布候选（写命令）",
		Long: "撤回按当前发布指针分三类：仍指向本次发布 → 上一版内容作为新版本重新发布；\n" +
			"已被后续发布超越 / 技能已删除 → 只记录撤回，不移动指针；首版没有上一版 → 拒绝。\n" +
			"理由必填、≤2000 字符并扫描凭证；重复撤回幂等返回原发布记录。\n" +
			"Withdraw re-publishes the previous version when the pointer still names this\n" +
			"publication, records only otherwise, and refuses to withdraw the first version.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" {
				return errors.New("withdraw 需要 --reason 说明撤回原因")
			}
			return withAdmissionWrite(deps, "admission withdraw", func(actor core.Actor, port AdmissionPort) error {
				publication, err := port.Withdraw(cmd.Context(), actor, args[0], reason)
				if err != nil {
					return err
				}
				if asJSON {
					return writeAdmissionJSON(cmd, admissionPublicationJSONView(publication))
				}
				out := cmd.OutOrStdout()
				_, err = fmt.Fprintf(out, "已撤回：%s（撤回前版本 %d，%s）\n状态：%s\n撤回分类：%s\n撤回时刻：%s\n",
					publication.SkillName, publication.VersionNo, publication.SkillUID,
					publication.State, publication.WithdrawReason, admissionTime(publication.WithdrawnAt))
				return err
			})
		},
	}
	command.Flags().StringVar(&reason, "reason", "", "撤回原因（≤2000 字符）")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// ─────────────────────────── JSON 投影 ───────────────────────────
//
// 投影刻意只带摘要字段：候选行里的 manifest / source / baseline 原文、来源 notes 与
// 决策理由全文都不进 CLI 的机器输出，机器消费者要正文就走 show --files / --diff。

// admissionCandidateJSON 是候选的机器可读投影。
// admissionCandidateJSON is the machine-readable candidate projection.
type admissionCandidateJSON struct {
	CandidateID        string `json:"candidate_id"`
	Skill              string `json:"skill"`
	State              string `json:"state"`
	Revision           int64  `json:"revision"`
	CandidateHash      string `json:"candidate_hash"`
	SourceHash         string `json:"source_hash"`
	RiskTier           string `json:"risk_tier"`
	ProposerSubject    string `json:"proposer_subject"`
	ProposerAuthMethod string `json:"proposer_auth_method"`
	ProposerSource     string `json:"proposer_source"`
	TerminalReason     string `json:"terminal_reason,omitempty"`
	OperationID        string `json:"operation_id"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

// admissionCandidateJSONView 投影一条候选。
// admissionCandidateJSONView projects one candidate row.
func admissionCandidateJSONView(candidate core.AdmissionCandidate) admissionCandidateJSON {
	return admissionCandidateJSON{
		CandidateID:        candidate.CandidateID,
		Skill:              candidate.SkillName,
		State:              string(candidate.State),
		Revision:           candidate.Revision,
		CandidateHash:      candidate.CandidateHash,
		SourceHash:         candidate.SourceHash,
		RiskTier:           string(candidate.RiskTier),
		ProposerSubject:    candidate.ProposerSubject,
		ProposerAuthMethod: candidate.ProposerAuthMethod,
		ProposerSource:     candidate.ProposerSource,
		TerminalReason:     candidate.TerminalReason,
		OperationID:        candidate.OperationID,
		CreatedAt:          admissionTime(candidate.CreatedAt),
		UpdatedAt:          admissionTime(candidate.UpdatedAt),
	}
}

// admissionEvaluationJSON 是评估的机器可读投影（维度带 JSON 标签，直接复用）。
// admissionEvaluationJSON is the machine-readable evaluation projection.
type admissionEvaluationJSON struct {
	EvaluationID      string                    `json:"evaluation_id"`
	CandidateID       string                    `json:"candidate_id"`
	CandidateRevision int64                     `json:"candidate_revision"`
	CandidateHash     string                    `json:"candidate_hash"`
	EvaluationHash    string                    `json:"evaluation_hash"`
	GraderVersion     string                    `json:"grader_version"`
	PolicyHash        string                    `json:"policy_hash"`
	Dimensions        []core.AdmissionDimension `json:"dimensions"`
	Scripts           []string                  `json:"scripts"`
	Aggregate         string                    `json:"aggregate"`
	RiskTier          string                    `json:"risk_tier"`
	Referrers         []string                  `json:"referrers"`
	BaselineJSON      string                    `json:"baseline_json"`
	EvaluatedAt       string                    `json:"evaluated_at"`
	OperationID       string                    `json:"operation_id"`
}

// admissionEvaluationJSONView 投影一次评估。
// admissionEvaluationJSONView projects one evaluation row.
func admissionEvaluationJSONView(evaluation core.AdmissionEvaluation) admissionEvaluationJSON {
	return admissionEvaluationJSON{
		EvaluationID:      evaluation.EvaluationID,
		CandidateID:       evaluation.CandidateID,
		CandidateRevision: evaluation.CandidateRevision,
		CandidateHash:     evaluation.CandidateHash,
		EvaluationHash:    evaluation.EvaluationHash,
		GraderVersion:     evaluation.GraderVersion,
		PolicyHash:        evaluation.PolicyHash,
		Dimensions:        nonNilDimensions(evaluation.Dimensions),
		Scripts:           nonNilStrings(evaluation.Scripts),
		Aggregate:         string(evaluation.Aggregate),
		RiskTier:          string(evaluation.RiskTier),
		Referrers:         nonNilStrings(evaluation.Referrers),
		BaselineJSON:      evaluation.BaselineJSON,
		EvaluatedAt:       admissionTime(evaluation.EvaluatedAt),
		OperationID:       evaluation.OperationID,
	}
}

// admissionDecisionJSON 是决策的机器可读投影。
// admissionDecisionJSON is the machine-readable decision projection.
type admissionDecisionJSON struct {
	DecisionID        string `json:"decision_id"`
	Decision          string `json:"decision"`
	Reason            string `json:"reason"`
	ApproverSubject   string `json:"approver_subject"`
	ApproverMethod    string `json:"approver_auth_method"`
	ApproverSource    string `json:"approver_source"`
	RiskTier          string `json:"risk_tier"`
	CandidateRevision int64  `json:"candidate_revision"`
	CandidateHash     string `json:"candidate_hash"`
	EvaluationID      string `json:"evaluation_id"`
	EvaluationHash    string `json:"evaluation_hash"`
	ExpiresAt         string `json:"expires_at"`
	SupersededAt      string `json:"superseded_at,omitempty"`
	CreatedAt         string `json:"created_at"`
}

// admissionDecisionJSONView 投影一次决策。
// admissionDecisionJSONView projects one decision row.
func admissionDecisionJSONView(decision core.AdmissionDecision) admissionDecisionJSON {
	return admissionDecisionJSON{
		DecisionID:        decision.DecisionID,
		Decision:          string(decision.Decision),
		Reason:            decision.Reason,
		ApproverSubject:   decision.ApproverSubject,
		ApproverMethod:    decision.ApproverAuthMethod,
		ApproverSource:    decision.ApproverSource,
		RiskTier:          string(decision.RiskTier),
		CandidateRevision: decision.CandidateRevision,
		CandidateHash:     decision.CandidateHash,
		EvaluationID:      decision.EvaluationID,
		EvaluationHash:    decision.EvaluationHash,
		ExpiresAt:         admissionTime(decision.ExpiresAt),
		SupersededAt:      admissionTime(decision.SupersededAt),
		CreatedAt:         admissionTime(decision.CreatedAt),
	}
}

// admissionPublicationJSON 是发布记录的机器可读投影。
// admissionPublicationJSON is the machine-readable publication projection.
type admissionPublicationJSON struct {
	Skill             string `json:"skill"`
	ResourceUID       string `json:"resource_uid"`
	VersionID         int64  `json:"version_id"`
	VersionNo         int64  `json:"version_no"`
	ContentHash       string `json:"content_hash"`
	State             string `json:"state"`
	PublishedAt       string `json:"published_at"`
	WithdrawnAt       string `json:"withdrawn_at,omitempty"`
	WithdrawReason    string `json:"withdraw_reason,omitempty"`
	WithdrawVersionID int64  `json:"withdraw_version_id,omitempty"`
}

// admissionPublicationJSONView 投影一次发布。
// admissionPublicationJSONView projects one publication row.
func admissionPublicationJSONView(publication core.AdmissionPublication) admissionPublicationJSON {
	return admissionPublicationJSON{
		Skill:             publication.SkillName,
		ResourceUID:       string(publication.SkillUID),
		VersionID:         publication.VersionID,
		VersionNo:         publication.VersionNo,
		ContentHash:       publication.ContentHash,
		State:             string(publication.State),
		PublishedAt:       admissionTime(publication.PublishedAt),
		WithdrawnAt:       admissionTime(publication.WithdrawnAt),
		WithdrawReason:    publication.WithdrawReason,
		WithdrawVersionID: publication.WithdrawVersionID,
	}
}

// admissionFileJSON 是候选文件正文的唯一机器出口（仅在显式 --files 时出现）。
// admissionFileJSON carries a file body, emitted only on an explicit --files request.
type admissionFileJSON struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// admissionShowJSON 是 show 的机器可读结果。
// admissionShowJSON is the machine-readable show result.
type admissionShowJSON struct {
	Candidate        admissionCandidateJSON    `json:"candidate"`
	Source           core.AdmissionSource      `json:"source"`
	Evaluation       *admissionEvaluationJSON  `json:"evaluation"`
	Decision         *admissionDecisionJSON    `json:"decision"`
	Publication      *admissionPublicationJSON `json:"publication"`
	Files            []admissionFileJSON       `json:"files,omitempty"`
	Diff             string                    `json:"diff,omitempty"`
	DecisionTemplate string                    `json:"decision_template,omitempty"`
}

// admissionDetailJSONView 投影只读详情；缺评估 / 决策 / 发布时对应字段为 null。
// admissionDetailJSONView projects the detail; missing rows stay null.
func admissionDetailJSONView(detail core.AdmissionDetail) admissionShowJSON {
	view := admissionShowJSON{
		Candidate: admissionCandidateJSONView(detail.Candidate),
		Source:    detail.Source,
	}
	if detail.Evaluation != nil {
		evaluation := admissionEvaluationJSONView(*detail.Evaluation)
		view.Evaluation = &evaluation
	}
	if detail.Decision != nil {
		decision := admissionDecisionJSONView(*detail.Decision)
		view.Decision = &decision
	}
	if detail.Publication != nil {
		publication := admissionPublicationJSONView(*detail.Publication)
		view.Publication = &publication
	}
	if detail.Files != nil {
		view.Files = make([]admissionFileJSON, 0, len(detail.Files))
		for _, file := range detail.Files {
			view.Files = append(view.Files, admissionFileJSON{Path: file.Path, Content: file.Content})
		}
	}
	view.Diff = detail.Diff
	view.DecisionTemplate = detail.DecisionTemplate
	return view
}

// nonNilStrings 把 nil 切片归一为空切片，保证 JSON 输出是 [] 而非 null。
// nonNilStrings keeps the JSON shape stable as [] rather than null.
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// nonNilDimensions 把 nil 维度列表归一为空切片（契约上恒有七维，归一只是防御）。
// nonNilDimensions keeps the dimension list an array even in defensive paths.
func nonNilDimensions(values []core.AdmissionDimension) []core.AdmissionDimension {
	if values == nil {
		return []core.AdmissionDimension{}
	}
	return values
}
