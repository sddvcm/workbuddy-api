// router.go 模型名路由：把客户端发来的裸模型名解析为 "channel/model"。
//
// 三接口共用一套语法：
//   - 带 channel/ 前缀（如 workbuddy/glm-5.2）→ 前缀即渠道，原样透传（兼容旧客户端）
//   - 无前缀 → 依次尝试：model_map 精确匹配 → model_map 通配（key 以 * 结尾）→ default_channel
//   - 全部未命中 → 报错并列出已知渠道，避免静默走错渠道
package gateway

import (
	"fmt"
	"sort"
	"strings"

	"workbuddy-api/internal/provider"
)

// Router 模型名解析规则。零值 Router 仅接受带前缀的模型名。
type Router struct {
	Default  string            // 兜底渠道，如 "workbuddy"
	Map      map[string]string // 裸名 → "channel/model"
	Channels []string          // 已知渠道（校验前缀 + 报错提示）
}

// Resolve 解析模型名，返回 "channel/model"。返回的 model 值可能带前缀，调用方勿再重复拼接。
//
// **倍率后缀在查表之前剥离**（见 provider.StripRateSuffix）：/v1/models 下发的名字
// 带 ` (0.06x)`，用户粘回来若不剥，`Map["glm (0.06x)"]` 查不到 → 静默落到兜底渠道，
// 把本该走 qodercn 的请求发到 workbuddy。这是「静默走错渠道」型故障，最难查。
//
// 剥离后重新把后缀贴回返回值，调用方（server.runtimeForModel）仍会再剥一次，
// 幂等，无需关心。
func (rt *Router) Resolve(model string) (string, error) {
	m := strings.TrimSpace(model)
	if m == "" {
		return "", fmt.Errorf("model is required")
	}

	// 剥倍率后缀（没有则原样返回）。后缀只会出现在最后一段，
	// 故在拆 channel 前缀之前整体剥除即可。
	suffix := ""
	if bare, had := provider.StripRateSuffix(m); had {
		m = bare
		suffix = provider.RateSuffixFormat(mustRate(model), true)
	}

	// 已带前缀：前缀必须已知（渠道名，或上游命名空间如 cline-free）
	if i := strings.Index(m, "/"); i > 0 {
		prefix, rest := m[:i], m[i+1:]
		if rest == "" {
			return "", fmt.Errorf("model %q has empty name after channel prefix", model)
		}
		if rt.isChannel(prefix) || isNamespace(prefix) {
			return m + suffix, nil
		}
		return "", fmt.Errorf("unknown channel %q in model %q; known channels: %s",
			prefix, model, strings.Join(rt.Channels, ", "))
	}

	// model_map 精确匹配（用剥离后缀的名字查表）
	if v := strings.TrimSpace(rt.Map[m]); v != "" {
		target, err := rt.resolveMapped(m, v)
		return target + suffix, err
	}
	// model_map 通配匹配（key 以 * 结尾，取最长匹配 key 以提升确定性）
	if wildcard := rt.longestWildcardKey(m); wildcard != "" {
		target, err := rt.resolveMapped(m, strings.TrimSpace(rt.Map[wildcard]))
		return target + suffix, err
	}

	// 兜底渠道：前缀取该渠道对外 ID 前缀（上游命名空间渠道用命名空间，如 cline-free，
	// 否则裸名会被拼成 cline/<name> —— cline 上游只认 cline-free/<name>，见 R59）。
	if rt.Default != "" {
		return provider.ModelIDPrefix(provider.Kind(rt.Default)) + "/" + m + suffix, nil
	}
	return "", fmt.Errorf("model %q needs an explicit channel prefix (e.g. %s/%s); "+
		"or set compat.default_channel / compat.model_map in config.json",
		model, firstOr(rt.Channels, "workbuddy"), m)
}

// mustRate 从已确认带后缀的名字里取倍率。StripRateSuffix 与 RateOfModel 的判定
// 条件完全一致，故 ok 必为 true；取不到时按 0 处理（宁可后缀写 0.00 也不能丢后缀）。
func mustRate(model string) float64 {
	if r, ok := provider.RateOfModel(model); ok {
		return r
	}
	return 0
}

// resolveMapped 校验映射目标：必须带已知渠道前缀或上游命名空间前缀，否则视为配置错误。
func (rt *Router) resolveMapped(orig, target string) (string, error) {
	i := strings.Index(target, "/")
	if i <= 0 || strings.TrimSpace(target[i+1:]) == "" {
		return "", fmt.Errorf("compat.model_map[%q] = %q 必须是 \"channel/model\" 形式", orig, target)
	}
	if prefix := target[:i]; !rt.isChannel(prefix) && !isNamespace(prefix) {
		return "", fmt.Errorf("compat.model_map[%q] = %q 的渠道 %q 未知；已知渠道: %s",
			orig, target, prefix, strings.Join(rt.Channels, ", "))
	}
	return target, nil
}

// isNamespace 报告 prefix 是否是某个渠道的上游 ID 命名空间（如 cline 的 cline-free）。
// 这类 ID 已经是完整的上游模型名，路由层原样放行，由内层 handler 解析渠道（见 R59）。
func isNamespace(prefix string) bool {
	_, ok := provider.ModelNamespaces[prefix]
	return ok
}

// longestWildcardKey 返回最长的匹配通配 key（无则空串）。
// 仅在 key 以 * 结尾时按前缀匹配；* 出现在中间视为普通字符。
func (rt *Router) longestWildcardKey(model string) string {
	best := ""
	for k := range rt.Map {
		if !strings.HasSuffix(k, "*") {
			continue
		}
		prefix := strings.TrimSuffix(k, "*")
		if strings.HasPrefix(model, prefix) && len(k) > len(best) {
			best = k
		}
	}
	return best
}

func (rt *Router) isChannel(name string) bool {
	if len(rt.Channels) == 0 { // 未注入渠道清单时不做前缀校验，保持向后兼容
		return true
	}
	for _, c := range rt.Channels {
		if c == name {
			return true
		}
	}
	return false
}

// SortChannels 去重并排序渠道清单（构造 Router 前调用，保证报错信息稳定）。
func SortChannels(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func firstOr(list []string, fallback string) string {
	if len(list) > 0 {
		return list[0]
	}
	return fallback
}
