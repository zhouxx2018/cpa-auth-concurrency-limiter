package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes,omitempty"`
	Resources []managementResource `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method string     `json:"Method,omitempty"`
	Path   string     `json:"Path,omitempty"`
	Query  url.Values `json:"Query,omitempty"`
	Body   []byte     `json:"Body,omitempty"`
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(bytesTrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	if strings.HasSuffix(req.Path, "/release") {
		return handleRelease(req)
	}
	if req.Path != "" && !strings.HasSuffix(req.Path, "/status") && req.Path != "/status" {
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
			Body:       []byte(`{"error":"未找到"}`),
		})
	}
	now := time.Now()
	globalLimiter.maybeRefreshAuths(now)
	status := globalLimiter.status(now)
	if wantsJSONStatus(req) {
		body, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			return nil, err
		}
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
			Body:       body,
		})
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"content-type": []string{"text/html; charset=utf-8"}},
		Body:       []byte(renderStatusHTML(status)),
	})
}

func handleRelease(req managementRequest) ([]byte, error) {
	if !strings.EqualFold(req.Method, http.MethodPost) {
		return okEnvelope(jsonManagementResponse(http.StatusMethodNotAllowed, map[string]any{
			"error": "请求方法不允许",
		}))
	}
	releaseReq, err := releaseRequestFromManagement(req)
	if err != nil {
		return okEnvelope(jsonManagementResponse(http.StatusBadRequest, map[string]any{
			"error": err.Error(),
		}))
	}
	if !releaseReq.All && strings.TrimSpace(releaseReq.SlotID) == "" && strings.TrimSpace(releaseReq.AuthID) == "" && strings.TrimSpace(releaseReq.AuthIndex) == "" && strings.TrimSpace(releaseReq.FileKey) == "" {
		return okEnvelope(jsonManagementResponse(http.StatusBadRequest, map[string]any{
			"error": "必须提供 all、slot_id、auth_id、auth_index 或 file_key 之一",
		}))
	}
	resp := globalLimiter.releaseManual(releaseReq, time.Now())
	return okEnvelope(jsonManagementResponse(http.StatusOK, resp))
}

func releaseRequestFromManagement(req managementRequest) (releaseRequest, error) {
	var out releaseRequest
	if len(bytesTrimSpace(req.Body)) > 0 {
		if err := json.Unmarshal(req.Body, &out); err != nil {
			return releaseRequest{}, fmt.Errorf("decode release request body: %w", err)
		}
	}
	if req.Query == nil {
		return out, nil
	}
	if raw := strings.TrimSpace(req.Query.Get("all")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return releaseRequest{}, fmt.Errorf("all 必须是布尔值")
		}
		out.All = parsed
	}
	if raw := strings.TrimSpace(req.Query.Get("slot_id")); raw != "" {
		out.SlotID = raw
	}
	if raw := strings.TrimSpace(req.Query.Get("auth_id")); raw != "" {
		out.AuthID = raw
	}
	if raw := strings.TrimSpace(req.Query.Get("auth_index")); raw != "" {
		out.AuthIndex = raw
	}
	if raw := strings.TrimSpace(req.Query.Get("file_key")); raw != "" {
		out.FileKey = raw
	}
	return out, nil
}

func jsonManagementResponse(statusCode int, payload any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"error":"failed to encode response"}`)
		statusCode = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

func wantsJSONStatus(req managementRequest) bool {
	if req.Query == nil {
		return false
	}
	format := strings.ToLower(strings.TrimSpace(req.Query.Get("format")))
	raw := strings.ToLower(strings.TrimSpace(req.Query.Get("raw")))
	return format == "json" || raw == "1" || raw == "true"
}

func renderStatusHTML(status statusResponse) string {
	cfg := status.Config
	limitText := "不限制"
	if cfg.DefaultLimit > 0 {
		limitText = strconv.Itoa(cfg.DefaultLimit)
	}
	activeText := strconv.Itoa(status.ActiveSlotCount)
	authCacheText := strconv.Itoa(status.HostAuthCacheSize)

	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>认证文件并发限制器</title>`)
	b.WriteString(`<style>
:root{color-scheme:light dark;--bg:#f7f8fb;--fg:#172033;--muted:#657186;--line:#d9dee8;--panel:#fff;--accent:#176f4d;--warn:#8a5a00}
@media (prefers-color-scheme:dark){:root{--bg:#11151d;--fg:#e8edf7;--muted:#9aa6bb;--line:#2c3444;--panel:#171d28;--accent:#62d197;--warn:#f0c36a}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.55 system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
main{max-width:1180px;margin:0 auto;padding:28px 22px 44px}h1{margin:0 0 6px;font-size:24px;line-height:1.25}h2{margin:28px 0 10px;font-size:17px}
.sub{color:var(--muted);margin:0 0 20px}.summary{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px;margin:18px 0 8px}
.metric{background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:12px 14px}.metric strong{display:block;font-size:22px;line-height:1.2}.metric span{color:var(--muted)}
.notice{border-left:4px solid var(--accent);background:var(--panel);padding:10px 12px;margin:18px 0;color:var(--fg)}
.warn{border-left-color:var(--warn)}table{width:100%;border-collapse:collapse;background:var(--panel);border:1px solid var(--line);border-radius:8px;overflow:hidden}
th,td{padding:10px 12px;border-bottom:1px solid var(--line);text-align:left;vertical-align:top}th{font-weight:650;color:var(--muted)}tr:last-child td{border-bottom:0}
code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px}.muted{color:var(--muted)}.actions{margin-top:18px}.actions a{color:var(--accent);text-decoration:none}
</style></head><body><main>`)
	b.WriteString(`<h1>认证文件并发限制器</h1>`)
	b.WriteString(`<p class="sub">按 CPA 认证文件限制最大并发请求数，并支持手动释放槽位。</p>`)
	b.WriteString(`<section class="summary">`)
	writeMetric(&b, "默认并发", limitText)
	writeMetric(&b, "活跃槽位", activeText)
	writeMetric(&b, "认证缓存", authCacheText)
	writeMetric(&b, "策略", cfg.Strategy)
	writeMetric(&b, "槽位 TTL", strconv.FormatInt(cfg.SlotTTLSeconds, 10)+" 秒")
	writeMetric(&b, "刷新间隔", strconv.FormatInt(cfg.AuthRefreshInterval, 10)+" 秒")
	b.WriteString(`</section>`)

	if cfg.DefaultLimit <= 0 && len(cfg.Limits) == 0 {
		b.WriteString(`<div class="notice warn">当前 <code>default_limit</code> 为 0，且没有配置 <code>limits</code>。除非认证 JSON 内写了 <code>cpa_max_concurrency</code> 或 <code>max_concurrency</code>，否则不会限制并发。</div>`)
	}
	if status.LastAuthRefreshErr != "" {
		b.WriteString(`<div class="notice warn">最近一次刷新认证文件失败：`)
		b.WriteString(html.EscapeString(status.LastAuthRefreshErr))
		b.WriteString(`</div>`)
	} else if status.HostAuthCacheSize == 0 {
		b.WriteString(`<div class="notice">认证缓存为空。通常需要有一次模型请求触发调度后，插件才会刷新认证文件列表。</div>`)
	}

	b.WriteString(`<h2>当前配置</h2><table><tbody>`)
	writeKVRow(&b, "default_limit", limitText)
	writeKVRow(&b, "strategy", cfg.Strategy)
	writeKVRow(&b, "deny_status", strconv.Itoa(cfg.DenyStatus))
	writeKVRow(&b, "read_auth_limits", strconv.FormatBool(cfg.ReadAuthLimits))
	writeKVRow(&b, "slot_ttl", strconv.FormatInt(cfg.SlotTTLSeconds, 10)+" 秒")
	writeKVRow(&b, "auth_refresh_interval", strconv.FormatInt(cfg.AuthRefreshInterval, 10)+" 秒")
	if status.LastAuthRefresh != "" {
		writeKVRow(&b, "最近刷新", status.LastAuthRefresh)
	}
	b.WriteString(`</tbody></table>`)

	b.WriteString(`<h2>活跃槽位</h2>`)
	if len(status.Buckets) == 0 {
		b.WriteString(`<p class="muted">当前没有占用中的槽位。</p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>认证文件</th><th>数量</th><th>槽位</th></tr></thead><tbody>`)
		for _, bucket := range status.Buckets {
			b.WriteString(`<tr><td><code>`)
			b.WriteString(html.EscapeString(firstNonEmpty(bucket.DisplayKey, bucket.FileKey)))
			b.WriteString(`</code></td><td>`)
			b.WriteString(strconv.Itoa(bucket.Count))
			b.WriteString(`</td><td>`)
			for index, slot := range bucket.Slots {
				if index > 0 {
					b.WriteString(`<br>`)
				}
				b.WriteString(`<code>`)
				b.WriteString(html.EscapeString(slot.ID))
				b.WriteString(`</code>`)
				if slot.AuthID != "" {
					b.WriteString(` <span class="muted">auth:</span> `)
					b.WriteString(html.EscapeString(slot.AuthID))
				}
				b.WriteString(` <span class="muted">剩余 `)
				b.WriteString(strconv.FormatInt(slot.ExpiresInSeconds, 10))
				b.WriteString(` 秒</span>`)
			}
			b.WriteString(`</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	}

	b.WriteString(`<h2>认证文件缓存</h2>`)
	if len(status.Auths) == 0 {
		b.WriteString(`<p class="muted">暂无缓存的认证文件。</p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>名称</th><th>Provider</th><th>Auth Index</th><th>当前并发 / 最大并发</th><th>限额来源</th></tr></thead><tbody>`)
		for _, auth := range status.Auths {
			limit := "不限制"
			if auth.EffectiveLimit > 0 {
				limit = strconv.Itoa(auth.EffectiveLimit)
			}
			b.WriteString(`<tr><td><code>`)
			b.WriteString(html.EscapeString(firstNonEmpty(auth.Name, auth.Path, auth.ID)))
			b.WriteString(`</code></td><td>`)
			b.WriteString(html.EscapeString(auth.Provider))
			b.WriteString(`</td><td><code>`)
			b.WriteString(html.EscapeString(auth.Index))
			b.WriteString(`</code></td><td>`)
			b.WriteString(strconv.Itoa(auth.CurrentSlots))
			b.WriteString(` / `)
			b.WriteString(html.EscapeString(limit))
			b.WriteString(`</td><td>`)
			b.WriteString(html.EscapeString(limitSourceLabel(auth.LimitSource)))
			b.WriteString(`</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	}

	b.WriteString(`<div class="actions"><a href="?format=json">查看原始 JSON</a></div>`)
	b.WriteString(`</main></body></html>`)
	return b.String()
}

func writeMetric(b *strings.Builder, label string, value string) {
	b.WriteString(`<div class="metric"><strong>`)
	b.WriteString(html.EscapeString(value))
	b.WriteString(`</strong><span>`)
	b.WriteString(html.EscapeString(label))
	b.WriteString(`</span></div>`)
}

func writeKVRow(b *strings.Builder, key string, value string) {
	b.WriteString(`<tr><th><code>`)
	b.WriteString(html.EscapeString(key))
	b.WriteString(`</code></th><td>`)
	b.WriteString(html.EscapeString(value))
	b.WriteString(`</td></tr>`)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return "-"
}

func limitSourceLabel(source string) string {
	switch strings.TrimSpace(source) {
	case "config":
		return "插件 limits 配置"
	case "auth_json":
		return "认证 JSON"
	case "default":
		return "默认配置"
	case "unlimited":
		return "不限制"
	default:
		if strings.TrimSpace(source) == "" {
			return "-"
		}
		return source
	}
}
