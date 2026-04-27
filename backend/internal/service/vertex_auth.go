package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// vertexOAuthScope 是 Vertex AI 调用所需的 OAuth2 scope。
const vertexOAuthScope = "https://www.googleapis.com/auth/cloud-platform"

// VertexTokenProvider 缓存每个 Vertex 账号的 oauth2.TokenSource，让底层
// 库（golang.org/x/oauth2）自动处理 access token 的刷新。
//
// 缓存键为 account.ID；如果账号的 SA JSON 被修改（管理员轮换凭据），
// 通过比对 JSON 原文实现缓存失效。TokenSource 内部由 oauth2.ReuseTokenSource
// 包装，因此并发调用 Token() 是线程安全的且只在 token 临近过期时才刷新。
type VertexTokenProvider struct {
	cache sync.Map // key: int64 (account.ID), value: *vertexCachedTokenSource
}

type vertexCachedTokenSource struct {
	saJSON string             // 原始 SA JSON 用于检测凭据轮换
	src    oauth2.TokenSource // 由 oauth2.ReuseTokenSource 包装，自动刷新
}

// NewVertexTokenProvider 创建一个空的 token provider，调用方应在 GatewayService
// 启动时构造一次并复用。
func NewVertexTokenProvider() *VertexTokenProvider {
	return &VertexTokenProvider{}
}

// GetAccessToken 取得给定账号的当前 Vertex access token（必要时刷新）。
// 返回的 token 适合直接拼到 "Authorization: Bearer ..." 头里。
func (p *VertexTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	src, err := p.tokenSourceFor(account)
	if err != nil {
		return "", err
	}
	tok, err := src.Token()
	if err != nil {
		return "", fmt.Errorf("vertex: fetch access token: %w", err)
	}
	return tok.AccessToken, nil
}

// tokenSourceFor 返回与账号绑定的 TokenSource，命中缓存或现场构造。
//   - 空 SA JSON → error
//   - SA JSON 解析失败 → error
//   - SA JSON 未变化 → 复用缓存
//   - SA JSON 已变化 → 失效缓存并重建
func (p *VertexTokenProvider) tokenSourceFor(account *Account) (oauth2.TokenSource, error) {
	if account == nil {
		return nil, errors.New("vertex: nil account")
	}

	saJSON := vertexServiceAccountJSON(account)
	if saJSON == "" {
		return nil, errors.New("vertex: gcp_service_account_json not found in credentials")
	}

	if cached, ok := p.cache.Load(account.ID); ok {
		c := cached.(*vertexCachedTokenSource)
		if c.saJSON == saJSON {
			return c.src, nil
		}
	}

	cfg, err := google.JWTConfigFromJSON([]byte(saJSON), vertexOAuthScope)
	if err != nil {
		return nil, fmt.Errorf("vertex: parse service account JSON: %w", err)
	}

	// ReuseTokenSource caches the token until ~1 minute before expiry, then
	// triggers a refresh via the underlying JWT-based source on next Token().
	// Use context.Background() — the token source outlives the request context.
	src := oauth2.ReuseTokenSource(nil, cfg.TokenSource(context.Background()))

	p.cache.Store(account.ID, &vertexCachedTokenSource{
		saJSON: saJSON,
		src:    src,
	})
	return src, nil
}

// attachVertexAuth 把 Bearer access token 写入 Authorization 头。
func attachVertexAuth(req *http.Request, accessToken string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
}
