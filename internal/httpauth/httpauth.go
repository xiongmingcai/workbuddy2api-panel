// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// VerifyRequest 校验请求是否携带正确的密钥，接受两种传递方式：
//   - Authorization: Bearer <key>（OpenAI 风格）
//   - x-api-key: <key>（Anthropic / Claude Code 风格）
//
// 与 VerifyBearer 同口径：key 为空恒放行；摘要 + 常量时间比较，两种方式都做
// 相同形状的比较，不因缺头/方案差异泄露信息。
func VerifyRequest(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	if VerifyBearer(r, key) {
		return true
	}
	tok := strings.TrimSpace(r.Header.Get("x-api-key"))
	if tok == "" {
		// 缺头：仍做一次摘要比较，保持与 Bearer 路径一致的耗时形状。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
