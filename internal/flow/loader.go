package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// Loader 从受控根目录加载普通文件；YAML 适配由装配层显式传入。
// Loader reads ordinary files within a controlled root with an explicit YAML adapter.
type Loader struct {
	Root      string
	Decode    func([]byte) (Definition, error)
	Validator *ContractValidator
}

// Load 加载一层流程目录，拒绝逃逸、链接、重解析点及大小写碰撞。
// Load rejects escape paths, links, reparse points and case-folded collisions.
func (l *Loader) Load(ctx context.Context, dir string) (def Definition, err error) {
	return l.load(ctx, dir, false)
}

// LoadStructure performs the same bounded, link-free two-file read as Load but
// reports local structural validity only. It grants no publication or Run rights.
func (l *Loader) LoadStructure(ctx context.Context, dir string) (Definition, error) {
	return l.load(ctx, dir, true)
}

func (l *Loader) load(ctx context.Context, dir string, structureOnly bool) (def Definition, err error) {
	if err := ctx.Err(); err != nil {
		return def, err
	}
	if l.Root == "" || l.Decode == nil || l.Validator == nil || !validID(dir) || filepath.IsAbs(dir) || strings.ContainsAny(dir, `/\`) {
		return def, errors.New("flow loader root, decoder or directory invalid")
	}
	info, err := os.Lstat(l.Root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return def, errors.New("flow root must be an ordinary directory")
	}
	base, err := os.OpenRoot(l.Root)
	if err != nil {
		return def, errors.New("flow root unavailable")
	}
	defer func() { err = errors.Join(err, base.Close()) }()
	opened, err := base.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return def, errors.New("flow root changed while opening")
	}
	if err := rejectCaseCollision(base); err != nil {
		return def, err
	}
	childInfo, err := base.Lstat(dir)
	if err != nil || !childInfo.IsDir() || childInfo.Mode()&os.ModeSymlink != 0 {
		return def, errors.New("flow directory must be ordinary")
	}
	child, err := base.OpenRoot(dir)
	if err != nil {
		return def, errors.New("flow directory unavailable")
	}
	defer func() { err = errors.Join(err, child.Close()) }()
	opened, err = child.Stat(".")
	if err != nil || !os.SameFile(childInfo, opened) {
		return def, errors.New("flow directory changed while opening")
	}
	if err := rejectCaseCollision(child); err != nil {
		return def, err
	}
	definitionBytes, err := readFlowFile(child, "FLOW.yaml")
	if err != nil {
		return def, err
	}
	def, err = l.Decode(definitionBytes)
	if err != nil {
		return def, errors.New("flow definition syntax invalid")
	}
	if def.Name != dir || def.TaskContract != "TASK.json" || !reflect.DeepEqual(def.Contract, TaskContract{}) {
		return def, errors.New("flow definition directory or contract reference invalid")
	}
	contractBytes, err := readFlowFile(child, "TASK.json")
	if err != nil {
		return def, err
	}
	def.Contract, err = DecodeTaskContractJSON(ctx, contractBytes)
	if err != nil {
		return def, err
	}
	validate := l.Validator.ValidateDefinition
	if structureOnly {
		validate = l.Validator.ValidateStructure
	}
	if err := validate(ctx, def); err != nil {
		return def, err
	}
	def.ContentHash, err = DefinitionHash(def)
	return def, err
}

// DecodeDefinitionJSON 只读取作者文件字段，不接受持久版本内部的内嵌 contract。
// DecodeDefinitionJSON reads author fields only, excluding persisted embedded contracts.
func DecodeDefinitionJSON(ctx context.Context, input json.RawMessage) (Definition, error) {
	value, err := core.DecodeFlowJSON(ctx, input)
	if err != nil {
		return Definition{}, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return Definition{}, errors.New("flow definition must be an object")
	}
	for key := range object {
		if key != "format_version" && key != "name" && key != "entry" && key != "nodes" && key != "limits" && key != "task_contract" {
			return Definition{}, errors.New("flow author definition field unsupported")
		}
	}
	var def Definition
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&def); err != nil {
		return Definition{}, errors.New("flow definition fields invalid")
	}
	return def, nil
}

// DecodeTaskContractJSON 从全新值严格读取唯一契约文件，不与定义文件合并。
// DecodeTaskContractJSON strictly reads a fresh contract without cross-file merging.
func DecodeTaskContractJSON(ctx context.Context, input json.RawMessage) (TaskContract, error) {
	if _, err := core.DecodeFlowJSON(ctx, input); err != nil {
		return TaskContract{}, err
	}
	var contract TaskContract
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		return TaskContract{}, errors.New("flow contract fields invalid")
	}
	return contract, nil
}

func rejectCaseCollision(root *os.Root) (err error) {
	file, err := root.Open(".")
	if err != nil {
		return errors.New("flow directory listing unavailable")
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	entries, err := file.ReadDir(10001)
	if err != nil && !errors.Is(err, io.EOF) {
		return errors.New("flow directory listing failed")
	}
	if len(entries) > 10000 {
		return errors.New("flow directory capacity exceeded")
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if seen[name] {
			return errors.New("flow case-folded name collision")
		}
		seen[name] = true
	}
	return nil
}

func readFlowFile(root *os.Root, name string) (data []byte, err error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("flow file must be ordinary and bounded")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.New("flow file unavailable")
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("flow file changed while opening")
	}
	data, err = io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("flow file read failed or exceeded bound")
	}
	after, err := root.Lstat(name)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(info, after) {
		return nil, errors.New("flow file changed while reading")
	}
	return data, nil
}
