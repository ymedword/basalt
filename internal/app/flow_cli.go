package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/tool"
)

// flowCLIPort is deliberately stateless: neither operation opens a database,
// starts a runtime, writes a message or invokes a business tool.
type flowCLIPort struct{}

var _ cli.FlowClient = flowCLIPort{}

func (flowCLIPort) ValidateStructure(ctx context.Context, dir string) (json.RawMessage, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("流程目录无法解析")
	}
	loader := flow.Loader{Root: filepath.Dir(abs), Validator: &flow.ContractValidator{Schemas: tool.NewSchemaCompiler()}, Decode: func(data []byte) (flow.Definition, error) {
		raw, err := yaml.YAMLToJSON(data)
		if err != nil {
			return flow.Definition{}, errors.New("FLOW.yaml 语法无效")
		}
		return flow.DecodeDefinitionJSON(ctx, raw)
	}}
	definition, err := loader.LoadStructure(ctx, filepath.Base(abs))
	if err != nil {
		return nil, errors.New("本地流程结构校验失败；请检查两份文件、Schema、节点引用和有界路线")
	}
	return json.Marshal(map[string]any{"valid": true, "check": "structure_only", "name": definition.Name, "content_hash": definition.ContentHash, "message": "本地结构校验通过；发布依赖、工具能力与权限必须由运行实例另行核对", "definition": definition})
}

func (flowCLIPort) Request(ctx context.Context, req cli.FlowRequest) (data json.RawMessage, err error) {
	base, err := url.Parse(req.Server)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" && base.Path != "/" || base.Scheme != "http" && base.Scheme != "https" {
		return nil, errors.New("--server 必须是无凭据、查询或路径的 HTTP(S) 实例地址")
	}
	if base.Scheme == "http" {
		ip := net.ParseIP(base.Hostname())
		if base.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("远端实例须使用 HTTPS，以保护凭据")
		}
	}
	token, err := readFlowCLIFile(req.TokenFile, 4096, true)
	if err != nil {
		return nil, err
	}
	secret := strings.TrimSpace(string(token))
	if secret == "" || strings.ContainsAny(secret, "\x00\r\n\t ") || strings.Contains(secret, "${") {
		return nil, errors.New("凭据文件内容无效")
	}
	method, path := http.MethodGet, "/api/v1/flows"
	var body any
	switch req.Operation {
	case "list":
	case "start":
		if !flowCLIIdentifier(req.ID) || strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 256 || strings.ContainsAny(req.IdempotencyKey, "\r\n") {
			return nil, errors.New("流程 UID 或幂等标识无效")
		}
		input, err := readFlowCLIFile(req.InputFile, 1<<20, false)
		if err != nil {
			return nil, err
		}
		canonical, err := flow.CanonicalJSON(ctx, input)
		if err != nil {
			return nil, errors.New("输入必须是无重复字段、大小有界的合法 JSON")
		}
		method, path = http.MethodPost, "/api/v1/flow-tasks"
		body = map[string]any{"definition_uid": req.ID, "input": json.RawMessage(canonical)}
	case "status", "cancel", "resume":
		if !flowCLIIdentifier(req.ID) {
			return nil, errors.New("任务标识无效")
		}
		path = "/api/v1/flow-tasks/" + req.ID
		if req.Operation != "status" {
			if req.ETag == "" || len(req.ETag) > 512 || strings.ContainsAny(req.ETag, "\r\n") || strings.TrimSpace(req.OperationID) == "" || len(req.OperationID) > 256 || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 4096 {
				return nil, errors.New("控制操作需要有效完整 ETag、操作标识与理由")
			}
			method, path = http.MethodPost, path+"/"+req.Operation
			body = map[string]string{"operation_id": req.OperationID, "reason": req.Reason}
		}
	default:
		return nil, errors.New("流程客户端操作无效")
	}
	base.Path = path
	if method == http.MethodGet {
		if req.Limit < 1 || req.Limit > 100 || len(req.Cursor) > 512 {
			return nil, errors.New("分页参数无效")
		}
		values := url.Values{"limit": {strconv.Itoa(req.Limit)}}
		if req.Cursor != "" {
			values.Set("cursor", req.Cursor)
		}
		base.RawQuery = values.Encode()
	}
	var raw []byte
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, errors.New("无法编码流程请求")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, base.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("无法创建流程请求")
	}
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/json")
	if req.Operation == "start" {
		request.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}
	if req.ETag != "" {
		request.Header.Set("If-Match", req.ETag)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 65 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("流程实例请求未确认；请保留并复用原幂等键或操作标识查证")
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	raw, err = io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return nil, errors.New("实例响应读取失败或过大；请缩小分页")
	}
	var envelope struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Code == "" {
		return nil, errors.New("实例返回了无效响应")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || envelope.Code != "OK" {
		return nil, errors.New("流程请求被实例拒绝（HTTP " + strconv.Itoa(response.StatusCode) + "）；请检查权限、当前版本和任务状态")
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, errors.New("实例未返回流程结果")
	}
	return envelope.Data, nil
}

func flowCLIIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func readFlowCLIFile(path string, maximum int64, secret bool) (data []byte, err error) {
	abs, err := filepath.Abs(path)
	if path == "" || err != nil {
		return nil, errors.New("请指定有效的普通文件")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || !sameFilePath(resolved, abs) {
		return nil, errors.New("文件不可读，或路径经过符号链接／重解析点")
	}
	parent, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return nil, errors.New("文件目录不可读")
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	name := filepath.Base(abs)
	info, err := parent.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("文件必须是大小有界的普通文件")
	}
	if secret {
		if count, known := hardLinkCount(abs, info); known && count > 1 {
			return nil, errors.New("凭据文件不得有硬链接")
		}
		if err := checkKeyPermissions(filepath.Dir(abs), abs); err != nil {
			return nil, errors.New("凭据文件及目录权限过宽，请使用实例私有目录或限制为当前用户")
		}
	}
	file, err := parent.Open(name)
	if err != nil {
		return nil, errors.New("文件无法打开")
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("读取期间文件发生变化")
	}
	data, err = io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, errors.New("文件读取失败或超过大小限制")
	}
	after, err := parent.Lstat(name)
	if err != nil || !os.SameFile(info, after) || !after.Mode().IsRegular() {
		return nil, errors.New("读取期间文件发生变化")
	}
	return data, nil
}
