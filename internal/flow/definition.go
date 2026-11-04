package flow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// Definition 是作者发布的版本化流程正文；不能在执行中由模型改图。
// Definition is the published author-controlled graph, never mutable by a model.
type Definition struct {
	FormatVersion int          `json:"format_version"`
	Name          string       `json:"name"`
	Entry         string       `json:"entry"`
	Nodes         []Node       `json:"nodes"`
	Limits        BudgetCap    `json:"limits"`
	TaskContract  string       `json:"task_contract"`
	Contract      TaskContract `json:"contract"`
	ContentHash   string       `json:"-"`
}

// DefinitionVersion 封装不可变发布内容和身份；名称不是引用身份。
// DefinitionVersion binds immutable publication content to a stable identity.
type DefinitionVersion struct {
	DefinitionUID string     `json:"definition_uid"`
	Version       int64      `json:"version"`
	ContentHash   string     `json:"content_hash"`
	Definition    Definition `json:"definition"`
	Retired       bool       `json:"retired"`
	PublishedAt   int64      `json:"published_at"`
}

// AgentRef 固定 UID、版本和快照哈希，名称仅帮助既有准入定位后再次核对。
// AgentRef pins identity and snapshot hashes; names are only lookup hints.
type AgentRef struct {
	UID          string `json:"uid"`
	Version      int64  `json:"version"`
	Name         string `json:"name"`
	ConfigHash   string `json:"config_hash"`
	ManifestHash string `json:"manifest_hash"`
}

// DefinitionRef 固定子流程版本，不使用当前指针隐式替换。
// DefinitionRef pins a subflow version rather than following its current pointer.
type DefinitionRef struct {
	UID     string `json:"uid"`
	Version int64  `json:"version"`
}

// InputMapping 只允许常量、合格任务参数或已验证上游输出；不运行表达式。
// InputMapping permits constants, accepted inputs and verified upstream outputs only.
type InputMapping struct {
	Target  string          `json:"target"`
	Source  string          `json:"source,omitempty"`
	Literal json.RawMessage `json:"literal,omitempty"`
}

// FailurePolicy 把只读重试和有限返工写入作者规则；unknown 永不重放。
// FailurePolicy bounds read-only retries and rework without replaying unknown actions.
type FailurePolicy struct {
	MaxAttempts  int    `json:"max_attempts"`
	ReworkTarget string `json:"rework_target,omitempty"`
	MaxReworks   int    `json:"max_reworks,omitempty"`
}

// Branch 只比较已校验输出或 C 自有状态，不读取任意正文或模型状态字符串。
// Branch compares validated outputs or trusted task state, never arbitrary text.
type Branch struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
	Then  string          `json:"then,omitempty"`
	Else  string          `json:"else,omitempty"`
}

// Node 是 agent、tool、branch、subflow 的封闭节点集合。
// Node is the closed sequential node union.
type Node struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	Agent        *AgentRef       `json:"agent,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Subflow      *DefinitionRef  `json:"subflow,omitempty"`
	Inputs       []InputMapping  `json:"inputs,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Next         string          `json:"next,omitempty"`
	Failure      FailurePolicy   `json:"failure"`
	Branch       *Branch         `json:"branch,omitempty"`
}

// SchemaCompiler 是本包消费的规则编译端口；第三方库不进入领域包。
// SchemaCompiler is the consumer-owned compilation port, keeping adapters outside flow.
type SchemaCompiler interface {
	Compile(ctx context.Context, schema json.RawMessage) (string, error)
	Validate(ctx context.Context, schemaHash string, input json.RawMessage) error
}

// DefinitionResolver 核对发布依赖；不会发起 Run 或业务工具调用。
// DefinitionResolver validates publication dependencies without running business work.
type DefinitionResolver interface {
	CheckAgent(ctx context.Context, ref AgentRef, tool string, write bool) error
	ReadDefinition(ctx context.Context, ref DefinitionRef) (DefinitionVersion, error)
}

// CalibrationResolver 核对可信评分器登记，不接受作者自报的任意校准字符串。
// CalibrationResolver checks trusted scorer registration instead of author assertions.
type CalibrationResolver interface {
	CheckCalibration(ctx context.Context, validatorID, calibrationID string) error
}

// ContractValidator 校验整条路线和每个必需规则，不信任模型的 verified 字符串。
// ContractValidator validates bounded routes and required author-controlled rules.
type ContractValidator struct {
	Schemas       SchemaCompiler
	Resolver      DefinitionResolver
	WriteGate     bool
	structureOnly bool
}

// ValidateStructure validates local syntax, schemas, unions and route bounds
// without claiming that published dependencies or execution permission exist.
// Only the offline CLI uses this path; publication always resolves dependencies.
func (v *ContractValidator) ValidateStructure(ctx context.Context, def Definition) error {
	if v == nil {
		return ErrDefinitionInvalid
	}
	local := *v
	local.structureOnly = true
	local.WriteGate = true
	local.Resolver = nil
	return local.ValidateDefinition(ctx, def)
}

// ValidateDefinition 拒绝不支持语义、无界路线与未满足的发布能力。
// ValidateDefinition rejects unsupported semantics and incomplete publication capabilities.
func (v *ContractValidator) ValidateDefinition(ctx context.Context, def Definition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.Schemas == nil || !v.structureOnly && v.Resolver == nil {
		return errors.New("flow definition validation unavailable")
	}
	if def.FormatVersion != 1 || !validID(def.Name) || def.TaskContract != "TASK.json" || len(def.Nodes) == 0 || len(def.Nodes) > 64 {
		return errors.New("flow definition format, name or node count invalid")
	}
	if err := validateBudget(def.Limits); err != nil {
		return err
	}
	if err := v.validateContract(ctx, def.Contract); err != nil {
		return err
	}
	if _, err := TightenBudget(def.Limits, &def.Contract.Budget); err != nil {
		return errors.New("flow task contract exceeds definition limits")
	}
	byID := make(map[string]Node, len(def.Nodes))
	for _, node := range def.Nodes {
		if !validID(node.ID) {
			return errors.New("flow node id invalid")
		}
		if _, found := byID[node.ID]; found {
			return errors.New("flow node id duplicated")
		}
		byID[node.ID] = node
	}
	for _, rule := range def.Contract.Acceptance {
		if rule.TargetKind != "claim" {
			continue
		}
		producer, found := byID[rule.TargetID]
		if !found || producer.Kind != "agent" && producer.Kind != "tool" {
			return errors.New("flow claim target must name an existing leaf producer")
		}
	}
	if err := validateUpstreamReferences(def, byID); err != nil {
		return err
	}
	if !v.structureOnly {
		if err := v.validateSubflowBounds(ctx, def); err != nil {
			return err
		}
	}
	if _, found := byID[def.Entry]; !found {
		return errors.New("flow entry node missing")
	}
	expanded := 0
	for _, node := range def.Nodes {
		if err := v.validateNode(ctx, def, node, byID); err != nil {
			return fmt.Errorf("flow node %s: %w", node.ID, err)
		}
		expanded += node.Failure.MaxAttempts * (node.Failure.MaxReworks + 1)
		if expanded > 256 {
			return errors.New("flow expanded route limit exceeded")
		}
	}
	colors := make(map[string]uint8, len(def.Nodes))
	var visit func(string) error
	visit = func(id string) error {
		if id == "" {
			return nil
		}
		if colors[id] == 1 {
			return errors.New("flow unbounded success cycle")
		}
		if colors[id] == 2 {
			return nil
		}
		colors[id] = 1
		node := byID[id]
		for _, next := range nodeSuccessors(node) {
			if err := visit(next); err != nil {
				return err
			}
		}
		colors[id] = 2
		return nil
	}
	for _, node := range def.Nodes {
		if err := visit(node.ID); err != nil {
			return err
		}
	}
	if _, _, err := DefinitionContent(def); err != nil {
		return errors.New("flow definition content limit exceeded")
	}
	return nil
}

var flowIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func validID(id string) bool { return flowIDPattern.MatchString(id) }

func validateBudget(cap BudgetCap) error {
	if cap.MaxTotalTokens < 1 || cap.MaxTotalTokens > 200000 || cap.MaxToolCalls < 1 || cap.MaxToolCalls > 200 || cap.MaxDurationSeconds < 1 || cap.MaxDurationSeconds > 1800 || cap.MaxDepth < 0 || cap.MaxDepth > 3 || cap.MaxChildren < 0 || cap.MaxChildren > 16 || cap.MaxAttemptsPerNode < 1 || cap.MaxAttemptsPerNode > 3 || cap.MaxRevisions < 0 || cap.MaxRevisions > 2 {
		return errors.New("flow budget missing or exceeds hard bounds")
	}
	return nil
}

func (v *ContractValidator) validateContract(ctx context.Context, contract TaskContract) error {
	if contract.ContractVersion < 1 || strings.TrimSpace(contract.Goal) == "" || len(contract.StopConditions) == 0 || len(contract.ArtifactSpec) == 0 {
		return errors.New("flow contract required controls missing")
	}
	if err := validateBudget(contract.Budget); err != nil {
		return err
	}
	for _, schema := range []json.RawMessage{contract.InputSchema, contract.OutputSchema} {
		if _, err := v.Schemas.Compile(ctx, schema); err != nil {
			return errors.Join(ErrSchemaUnsupported, err)
		}
	}
	artifacts := make(map[string]ArtifactSpec, len(contract.ArtifactSpec))
	for _, artifact := range contract.ArtifactSpec {
		if !validID(artifact.ID) || artifact.Format != "json" && artifact.Format != "text" {
			return errors.New("flow artifact specification invalid")
		}
		if _, found := artifacts[artifact.ID]; found {
			return errors.New("flow artifact id duplicated")
		}
		artifacts[artifact.ID] = artifact
	}
	slots := make(map[string]RequiredSlot, len(contract.RequiredSlots))
	for _, slot := range contract.RequiredSlots {
		if !validID(slot.ID) || len(slot.Validators) == 0 || slot.MinSources < 1 || slot.MinSources > 1024 {
			return errors.New("flow required slot must have validators and source bounds")
		}
		if _, found := slots[slot.ID]; found {
			return errors.New("flow slot id duplicated")
		}
		slots[slot.ID] = slot
	}
	rules := make(map[string]AcceptanceRule, len(contract.Acceptance))
	for _, rule := range contract.Acceptance {
		if !validID(rule.ID) || rule.Version < 1 {
			return errors.New("flow acceptance rule identity invalid")
		}
		if _, found := rules[rule.ID]; found {
			return errors.New("flow acceptance rule id duplicated")
		}
		switch rule.TargetKind {
		case "slot":
			if _, found := slots[rule.TargetID]; !found {
				return errors.New("flow acceptance target slot missing")
			}
		case "artifact":
			if _, found := artifacts[rule.TargetID]; !found {
				return errors.New("flow acceptance target artifact missing")
			}
		case "claim":
			if !validID(rule.TargetID) {
				return errors.New("flow claim target identity invalid")
			}
		default:
			return errors.New("flow acceptance target invalid")
		}
		switch rule.Kind {
		case "deterministic":
			if _, err := v.Schemas.Compile(ctx, rule.Schema); err != nil {
				return errors.Join(ErrSchemaUnsupported, err)
			}
		case "evidence":
			if rule.MinSources < 1 || rule.MinSources > 1024 {
				return errors.New("flow evidence rule source bounds invalid")
			}
		case "judgment":
			if !rule.HumanRequired && (rule.ValidatorID == "" || rule.CalibrationID == "") {
				return errors.New("flow judgment requires calibrated validator or human record")
			}
			if !rule.HumanRequired && !v.structureOnly {
				checker, ok := v.Resolver.(CalibrationResolver)
				if !ok {
					return errors.New("flow trusted calibration registry unavailable")
				}
				if err := checker.CheckCalibration(ctx, rule.ValidatorID, rule.CalibrationID); err != nil {
					return err
				}
			}
		default:
			return errors.New("flow acceptance rule kind invalid")
		}
		rules[rule.ID] = rule
	}
	for _, slot := range slots {
		for _, id := range slot.Validators {
			rule, found := rules[id]
			if !found || rule.TargetKind != "slot" || rule.TargetID != slot.ID || !rule.Required {
				return errors.New("flow required slot lacks applicable required rule")
			}
			if rule.Kind == "evidence" && rule.MinSources < slot.MinSources {
				return errors.New("flow evidence rule weakens slot source requirement")
			}
		}
	}
	for _, artifact := range artifacts {
		if artifact.Required {
			found := false
			for _, rule := range rules {
				found = found || rule.Required && rule.TargetKind == "artifact" && rule.TargetID == artifact.ID
			}
			if !found {
				return errors.New("flow required artifact lacks applicable rule")
			}
		}
	}
	return nil
}

func (v *ContractValidator) validateNode(ctx context.Context, def Definition, node Node, nodes map[string]Node) error {
	if node.Failure.MaxAttempts < 1 || node.Failure.MaxAttempts > def.Limits.MaxAttemptsPerNode || node.Failure.MaxReworks < 0 || node.Failure.MaxReworks > def.Limits.MaxRevisions {
		return errors.New("flow node attempt bounds invalid")
	}
	if node.Failure.ReworkTarget != "" {
		if _, found := nodes[node.Failure.ReworkTarget]; !found || node.Failure.MaxReworks == 0 {
			return errors.New("flow rework target or bound invalid")
		}
	} else if node.Failure.MaxReworks != 0 {
		return errors.New("flow rework bound without target")
	}
	for _, next := range nodeSuccessors(node) {
		if next != "" {
			if _, found := nodes[next]; !found {
				return errors.New("flow next node missing")
			}
		}
	}
	if _, err := v.Schemas.Compile(ctx, node.OutputSchema); err != nil {
		return errors.Join(ErrSchemaUnsupported, err)
	}
	switch node.Kind {
	case "agent", "tool":
		if node.Agent == nil || node.Agent.UID == "" || node.Agent.Version < 1 || !validID(node.Agent.Name) || node.Agent.ConfigHash == "" || node.Agent.ManifestHash == "" || node.Subflow != nil || node.Branch != nil {
			return errors.New("flow agent identity or node union invalid")
		}
		if node.Kind == "agent" && node.Tool != "" {
			return errors.New("flow agent node cannot set a fixed tool")
		}
		if node.Kind == "tool" && (node.Tool == "" || !v.WriteGate) {
			return errors.New("flow tool gate unavailable")
		}
		if !v.structureOnly {
			if err := v.Resolver.CheckAgent(ctx, *node.Agent, node.Tool, node.Kind == "tool"); err != nil {
				return err
			}
		}
	case "branch":
		if node.Branch == nil || node.Agent != nil || node.Subflow != nil || node.Tool != "" || node.Next != "" {
			return errors.New("flow branch node union invalid")
		}
		if err := v.validateBranch(ctx, def, *node.Branch, nodes); err != nil {
			return err
		}
	case "subflow":
		if node.Subflow == nil || node.Subflow.UID == "" || node.Subflow.Version < 1 || node.Agent != nil || node.Branch != nil || node.Tool != "" {
			return errors.New("flow subflow node union invalid")
		}
		if !v.structureOnly {
			if _, err := v.Resolver.ReadDefinition(ctx, *node.Subflow); err != nil {
				return err
			}
		}
	default:
		return errors.New("flow node kind unsupported")
	}
	targets := make(map[string]bool)
	for _, mapping := range node.Inputs {
		if !validID(mapping.Target) || targets[mapping.Target] || (mapping.Source == "") == (len(mapping.Literal) == 0) {
			return errors.New("flow input mapping invalid")
		}
		targets[mapping.Target] = true
		if len(mapping.Literal) > 0 {
			if _, err := core.DecodeFlowJSON(ctx, mapping.Literal); err != nil {
				return err
			}
		} else if _, err := trustedFieldSchema(ctx, def, mapping.Source, nodes, true); err != nil {
			return err
		}
	}
	return nil
}

func nodeSuccessors(node Node) []string {
	if node.Branch != nil {
		return []string{node.Branch.Then, node.Branch.Else}
	}
	return []string{node.Next}
}

func (v *ContractValidator) validateBranch(ctx context.Context, def Definition, branch Branch, nodes map[string]Node) error {
	if !slices.Contains([]string{"eq", "ne", "lt", "lte", "gt", "gte"}, branch.Op) {
		return errors.New("flow branch operator unsupported")
	}
	schema, err := trustedFieldSchema(ctx, def, branch.Field, nodes, false)
	if err != nil {
		return err
	}
	value, err := core.DecodeFlowJSON(ctx, schema)
	if err != nil {
		return err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("flow branch field schema invalid")
	}
	kind, _ := object["type"].(string)
	_, hasEnum := object["enum"]
	if kind != "boolean" && kind != "integer" && kind != "number" && !(kind == "string" && hasEnum) {
		return errors.New("flow branch requires boolean, enum or numeric field")
	}
	if branch.Op != "eq" && branch.Op != "ne" && kind != "integer" && kind != "number" {
		return errors.New("flow branch ordering requires numeric field")
	}
	hash, err := v.Schemas.Compile(ctx, schema)
	if err != nil {
		return err
	}
	return v.Schemas.Validate(ctx, hash, branch.Value)
}

func trustedFieldSchema(ctx context.Context, def Definition, field string, nodes map[string]Node, allowInput bool) (json.RawMessage, error) {
	parts := strings.Split(field, ".")
	if len(parts) == 3 && parts[0] == "slots" && parts[2] == "state" {
		for _, slot := range def.Contract.RequiredSlots {
			if slot.ID == parts[1] {
				return json.RawMessage(`{"type":"string","enum":["missing","found","conflict","unverifiable","verified"]}`), nil
			}
		}
	}
	if len(parts) == 2 && parts[0] == "budget" && (parts[1] == "remaining_tokens" || parts[1] == "remaining_tool_calls") {
		return json.RawMessage(`{"type":"integer","minimum":0}`), nil
	}
	if len(parts) == 2 && parts[0] == "attempts" {
		if _, found := nodes[parts[1]]; found {
			return json.RawMessage(`{"type":"integer","minimum":0}`), nil
		}
	}
	var schema json.RawMessage
	var path []string
	if len(parts) >= 2 && parts[0] == "input" && allowInput {
		schema, path = def.Contract.InputSchema, parts[1:]
	} else if len(parts) >= 3 && parts[0] == "outputs" {
		if node, found := nodes[parts[1]]; found {
			schema, path = node.OutputSchema, parts[2:]
		}
	}
	if len(schema) == 0 {
		return nil, errors.New("flow field is not declared trusted state")
	}
	value, err := core.DecodeFlowJSON(ctx, schema)
	if err != nil {
		return nil, err
	}
	for _, part := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("flow field schema invalid")
		}
		properties, ok := object["properties"].(map[string]any)
		if !ok {
			return nil, errors.New("flow field path missing")
		}
		value, ok = properties[part]
		if !ok {
			return nil, errors.New("flow field path undeclared")
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("flow field schema encoding failed")
	}
	return encoded, nil
}

// DefinitionHash 同时绑定定义与契约规范内容，不绑定可变发布指针。
// DefinitionHash binds canonical definition and contract content, not publication pointers.
func DefinitionHash(def Definition) (string, error) {
	definition, contract, err := DefinitionContent(def)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append(append(definition, '\n'), contract...))
	return hex.EncodeToString(digest[:]), nil
}

// DefinitionContent 分别返回定义与契约的规范字节，各自遵守1MiB边界。
// DefinitionContent returns separately bounded canonical definition and contract bytes.
func DefinitionContent(def Definition) ([]byte, []byte, error) {
	data, err := json.Marshal(def)
	if err != nil {
		return nil, nil, errors.New("flow definition encoding failed")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, nil, err
	}
	contract := fields["contract"]
	delete(fields, "contract")
	data, err = json.Marshal(fields)
	if err != nil {
		return nil, nil, err
	}
	definition, err := CanonicalJSON(context.Background(), data)
	if err != nil {
		return nil, nil, err
	}
	canonicalContract, err := CanonicalJSON(context.Background(), contract)
	return definition, canonicalContract, err
}

func (v *ContractValidator) validateSubflowBounds(ctx context.Context, def Definition) error {
	children, expanded := 0, 0
	seen := make(map[DefinitionRef]bool)
	var visit func(Definition, int, int) error
	visit = func(current Definition, depth, multiplier int) error {
		for _, node := range current.Nodes {
			if node.Failure.MaxAttempts < 1 || node.Failure.MaxAttempts > 3 || node.Failure.MaxReworks < 0 || node.Failure.MaxReworks > 2 {
				return errors.New("flow nested attempt bounds invalid")
			}
			unit := node.Failure.MaxAttempts * (node.Failure.MaxReworks + 1)
			if multiplier > 256/unit {
				return errors.New("flow expanded subflow bound exceeded")
			}
			weight := multiplier * unit
			expanded += weight
			if expanded > 256 {
				return errors.New("flow expanded subflow bound exceeded")
			}
			if node.Subflow == nil {
				continue
			}
			children += weight
			if depth >= def.Limits.MaxDepth || children > def.Limits.MaxChildren || seen[*node.Subflow] {
				return errors.New("flow subflow depth, cumulative count or recursion invalid")
			}
			seen[*node.Subflow] = true
			version, err := v.Resolver.ReadDefinition(ctx, *node.Subflow)
			if err != nil {
				return err
			}
			if version.DefinitionUID != node.Subflow.UID || version.Version != node.Subflow.Version {
				return errors.New("flow subflow version mismatch")
			}
			if err := visit(version.Definition, depth+1, weight); err != nil {
				return err
			}
			delete(seen, *node.Subflow)
		}
		return nil
	}
	return visit(def, 0, 1)
}

func validateUpstreamReferences(def Definition, nodes map[string]Node) error {
	// 支配集合使引用在每条可达控制路径上都是已执行前驱；运行期仍重查实际完成事实。
	// Dominators ensure references are preceding on every path; runtime facts are rechecked.
	predecessors := make(map[string][]string)
	reachable := make(map[string]bool)
	queue := []string{def.Entry}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if reachable[id] {
			continue
		}
		node, found := nodes[id]
		if !found {
			return errors.New("flow route references missing node")
		}
		reachable[id] = true
		next := nodeSuccessors(node)
		if node.Failure.ReworkTarget != "" {
			next = append(next, node.Failure.ReworkTarget)
		}
		for _, target := range next {
			if target == "" {
				continue
			}
			predecessors[target] = append(predecessors[target], id)
			queue = append(queue, target)
		}
	}
	dominators := make(map[string]map[string]bool)
	for id := range reachable {
		dominators[id] = map[string]bool{id: true}
		if id != def.Entry {
			for other := range reachable {
				dominators[id][other] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for id := range reachable {
			if id == def.Entry {
				continue
			}
			next := map[string]bool{id: true}
			for candidate := range reachable {
				all := len(predecessors[id]) > 0
				for _, previous := range predecessors[id] {
					all = all && dominators[previous][candidate]
				}
				if all {
					next[candidate] = true
				}
			}
			if len(next) != len(dominators[id]) {
				changed = true
			}
			dominators[id] = next
		}
	}
	for _, node := range def.Nodes {
		var fields []string
		for _, mapping := range node.Inputs {
			fields = append(fields, mapping.Source)
		}
		if node.Branch != nil {
			fields = append(fields, node.Branch.Field)
		}
		for _, field := range fields {
			parts := strings.Split(field, ".")
			if len(parts) > 1 && parts[0] == "outputs" && (parts[1] == node.ID || !dominators[node.ID][parts[1]]) {
				return errors.New("flow output reference is not a guaranteed upstream node")
			}
		}
		if reachable[node.ID] && slices.Contains(nodeSuccessors(node), "") {
			for _, rule := range def.Contract.Acceptance {
				if rule.Required && rule.TargetKind == "claim" && !dominators[node.ID][rule.TargetID] {
					return errors.New("flow required claim producer is not on every successful route")
				}
			}
		}
	}
	return nil
}
