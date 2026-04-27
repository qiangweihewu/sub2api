package service

import (
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/tidwall/sjson"
)

// vertexAnthropicVersion 是 Vertex AI 接收 Anthropic 模型时必须在 body 中携带的版本标识。
// 参考: https://cloud.google.com/vertex-ai/generative-ai/docs/partner-models/use-claude
const vertexAnthropicVersion = "vertex-2023-10-16"

// vertexProjectID 从账号凭据读取 GCP 项目 ID。空值由调用方处理。
func vertexProjectID(account *Account) string {
	if account == nil {
		return ""
	}
	return account.GetCredential("gcp_project_id")
}

// vertexRegion 从账号凭据读取 GCP region。返回空值时由调用方报错；不设默认 region —— Vertex
// 不同模型可用 region 各异，静默默认会带来"配错 region 才发现"的 404 噪音。
func vertexRegion(account *Account) string {
	if account == nil {
		return ""
	}
	return account.GetCredential("gcp_region")
}

// vertexServiceAccountJSON 从账号凭据读取 GCP Service Account JSON 原文。
func vertexServiceAccountJSON(account *Account) string {
	if account == nil {
		return ""
	}
	return account.GetCredential("gcp_service_account_json")
}

// looksLikeVertexModelID 判断字符串是否像已经解析好的 Vertex 模型 ID（含 '@日期' 形态）。
func looksLikeVertexModelID(modelID string) bool {
	return strings.Contains(modelID, "@")
}

// ResolveVertexModelID 把请求的 Claude 模型名解析为 Vertex 上发布的模型 ID。
// 解析顺序：
//  1. 账号 model_mapping（自定义优先）
//  2. domain.DefaultVertexModelMapping 默认表
//  3. 已经是 Vertex 形态（含 '@'）则直接放行
//
// 解析失败返回 ("", false)。
func ResolveVertexModelID(account *Account, requestedModel string) (string, bool) {
	if account == nil {
		return "", false
	}

	mapped := account.GetMappedModel(requestedModel)

	if looksLikeVertexModelID(mapped) {
		return mapped, true
	}

	if v, exists := domain.DefaultVertexModelMapping[mapped]; exists {
		return v, true
	}

	return "", false
}

// BuildVertexURL 构建 Vertex AI rawPredict / streamRawPredict 的 URL。
//
// 区域版（含 region 子域）：
//
//	https://{region}-aiplatform.googleapis.com/v1/projects/{project}/locations/{region}/publishers/anthropic/models/{model}:rawPredict
//
// global 特殊情况（不带 region 子域）：
//
//	https://aiplatform.googleapis.com/v1/projects/{project}/locations/global/publishers/anthropic/models/{model}:rawPredict
//
// stream=true 时端点变为 :streamRawPredict。
//
// 注意：Vertex 模型 ID 中的 '@' 在 URL path 中是合法字符，无需转义。
func BuildVertexURL(projectID, region, modelID string, stream bool) string {
	method := "rawPredict"
	if stream {
		method = "streamRawPredict"
	}

	host := fmt.Sprintf("%s-aiplatform.googleapis.com", region)
	if region == "global" {
		host = "aiplatform.googleapis.com"
	}

	return fmt.Sprintf(
		"https://%s/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s",
		host, projectID, region, modelID, method,
	)
}

// PrepareVertexRequestBody 将 Anthropic Messages 请求体调整为 Vertex AI 接收格式：
//  1. 注入 anthropic_version = "vertex-2023-10-16"（必须）
//  2. 移除 model 字段（model 通过 URL 路径指定）
//  3. 移除 stream 字段（流式由 endpoint :streamRawPredict 区分）
//
// 其他字段（messages/system/tools/max_tokens/temperature/thinking 等）原样保留。
// anthropic-beta 头若需要，由调用方在 HTTP header 上透传给 Vertex。
func PrepareVertexRequestBody(body []byte) ([]byte, error) {
	out, err := sjson.SetBytes(body, "anthropic_version", vertexAnthropicVersion)
	if err != nil {
		return nil, fmt.Errorf("inject anthropic_version: %w", err)
	}

	out, err = sjson.DeleteBytes(out, "model")
	if err != nil {
		return nil, fmt.Errorf("remove model field: %w", err)
	}

	out, err = sjson.DeleteBytes(out, "stream")
	if err != nil {
		return nil, fmt.Errorf("remove stream field: %w", err)
	}

	return out, nil
}
