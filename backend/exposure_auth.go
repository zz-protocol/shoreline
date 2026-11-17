package main

import (
	"context"
	"encoding/json"
	"strings"
)

// T62：资产暴露状态与鉴权状态。只用扫描内存里的 probeResult（状态码/标题/响应头/正文）判定，
// 不落正文、不写 IP；判定不出来的记 unknown，写库时不覆盖已有结论。
const (
	exposureLiveApp     = "live_app"
	exposureLoginOnly   = "login_only"
	exposureAPI         = "api"
	exposureAdmin       = "admin"
	exposurePlaceholder = "placeholder"
	exposureWAF         = "waf"
	exposureErrorPage   = "error_page"
	exposureStaticOnly  = "static_only"
	exposureUnknown     = "unknown"

	authAnonymous     = "anonymous"
	authLoginRequired = "login_required"
	authUnknown       = "unknown"
)

// wafStrongMarkers：常见 JS challenge / 人机校验页特征，命中即判 waf。
var wafStrongMarkers = []string{
	"cf-chl", "__cf_chl", "cf-challenge", "cf_challenge", "just a moment", "checking your browser",
	"attention required", "enable javascript and cookies to continue", "verify you are human",
	"please verify you are a human", "人机验证", "访问验证", "滑动验证", "安全验证",
}

// wafBlockedMarkers：401/403/429/503 上的拦截页特征（不含通用登录页表单）。
var wafBlockedMarkers = []string{"captcha", "challenge", "security check", "access denied", "验证码", "waf"}

var loginTitleKeywords = []string{
	"login", "log in", "logon", "sign in", "signin", "sign-in", "sso", "单点登录", "统一认证", "登录", "登入", "登陆",
}

var loginBodyKeywords = []string{
	"login", "logout", "sign in", "signin", "password", "username", "用户名", "密码", "登录", "登入",
}

var loginRedirectKeywords = []string{"login", "signin", "sign-in", "signon", "sso", "cas", "passport", "登录"}

var adminTitleKeywords = []string{
	"admin", "dashboard", "management console", "control panel", "management", "console", "管理后台", "后台管理", "管理系统", "控制台", "运维平台",
}

var adminBodyKeywords = []string{"管理后台", "后台管理", "系统管理", "控制台", "运维平台"}

var placeholderMarkers = []string{
	"welcome to nginx", "it works!", "apache2 ubuntu default page", "apache http server test page",
	"iis windows server", "iis7", "default web site page", "site not configured", "web site not found",
	"under construction", "coming soon", "domain is for sale", "this domain is parked", "buy this domain",
	"domain parking", "future home of something quite cool", "恭喜，站点创建成功", "站点创建成功",
	"网页建设中", "网站建设中", "域名出售", "域名停放", "站点未配置", "默认站点",
}

// classifyExposureAuth 判定资产暴露状态与鉴权状态（T62）。
func classifyExposureAuth(r *probeResult) (exposure, auth string) {
	if r == nil {
		return exposureUnknown, authUnknown
	}
	body := string(r.body)
	lowerBody := strings.ToLower(body)
	title := strings.ToLower(strings.Join(strings.Fields(r.Title), " "))

	if looksWAF(r, lowerBody) {
		return exposureWAF, authUnknown
	}
	if looksLoginPage(title, lowerBody) {
		return exposureLoginOnly, authLoginRequired
	}
	switch {
	case r.Status == 401:
		return exposureUnknown, authLoginRequired
	case r.Status >= 300 && r.Status < 400 && looksLoginURL(r.Redirect):
		return exposureUnknown, authLoginRequired
	case isErrorStatus(r.Status):
		return exposureErrorPage, authUnknown
	case r.Status != 200:
		return exposureUnknown, authUnknown
	}
	switch {
	case looksJSONBody(body):
		return exposureAPI, authAnonymous
	case looksPlaceholder(title, lowerBody):
		return exposurePlaceholder, authUnknown
	case looksAdminPage(title, lowerBody):
		return exposureAdmin, authUnknown
	case looksStaticOnly(lowerBody):
		return exposureStaticOnly, authUnknown
	case strings.TrimSpace(body) == "" && title == "":
		return exposureUnknown, authUnknown
	default:
		return exposureLiveApp, authUnknown
	}
}

func isErrorStatus(status int) bool {
	switch status {
	case 404, 500, 502, 503, 504:
		return true
	}
	return false
}

func looksWAF(r *probeResult, lowerBody string) bool {
	for _, m := range wafStrongMarkers {
		if strings.Contains(lowerBody, m) {
			return true
		}
	}
	server := strings.ToLower(r.Headers["Server"])
	if strings.Contains(server, "cloudflare") && (r.Status == 401 || r.Status == 403 || r.Status == 429 || r.Status == 503) {
		return true
	}
	if r.Status == 401 || r.Status == 403 || r.Status == 429 || r.Status == 503 {
		for _, m := range wafBlockedMarkers {
			if strings.Contains(lowerBody, m) {
				return true
			}
		}
	}
	return false
}

// hasPasswordInput：出现密码框即按登录页处理；只有登录字样没有输入框的页面不算。
func hasPasswordInput(lowerBody string) bool {
	for _, q := range []string{`type="password"`, `type='password'`, `type=password`, `type="passwd"`, `type='passwd'`} {
		if strings.Contains(lowerBody, q) {
			return true
		}
	}
	return false
}

func looksLoginPage(lowerTitle, lowerBody string) bool {
	if hasPasswordInput(lowerBody) {
		return true
	}
	for _, kw := range loginTitleKeywords {
		if strings.Contains(lowerTitle, kw) {
			return true
		}
	}
	// 正文登录词需与表单/输入框同时出现，避免普通页面里的 login 链接误判。
	if strings.Contains(lowerBody, "<form") || strings.Contains(lowerBody, "<input") {
		for _, kw := range loginBodyKeywords {
			if strings.Contains(lowerBody, kw) {
				return true
			}
		}
	}
	return false
}

func looksLoginURL(loc string) bool {
	l := strings.ToLower(loc)
	for _, kw := range loginRedirectKeywords {
		if strings.Contains(l, kw) {
			return true
		}
	}
	return false
}

func looksAdminPage(lowerTitle, lowerBody string) bool {
	for _, kw := range adminTitleKeywords {
		if strings.Contains(lowerTitle, kw) {
			return true
		}
	}
	for _, kw := range adminBodyKeywords {
		if strings.Contains(lowerBody, kw) {
			return true
		}
	}
	return false
}

func looksPlaceholder(lowerTitle, lowerBody string) bool {
	for _, m := range placeholderMarkers {
		if strings.Contains(lowerTitle, m) || strings.Contains(lowerBody, m) {
			return true
		}
	}
	return false
}

// looksJSONBody：正文是 JSON 对象/数组（业务 API）。
func looksJSONBody(body string) bool {
	s := strings.TrimSpace(body)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return false
	}
	if json.Valid([]byte(s)) {
		return true
	}
	head := s
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.Contains(head, "\":") || strings.Contains(head, "],[")
}

// looksStaticOnly：目录列表、二进制或纯文本等非 HTML/JSON 的静态文件响应。
func looksStaticOnly(lowerBody string) bool {
	if lowerBody == "" {
		return false
	}
	if strings.Contains(lowerBody, "index of /") && (strings.Contains(lowerBody, "directory listing") || strings.Contains(lowerBody, "parent directory")) {
		return true
	}
	if strings.IndexByte(lowerBody, 0) >= 0 {
		return true
	}
	for _, tag := range []string{"<html", "<!doctype html", "<head", "<body", "<title", "<meta", "<script"} {
		if strings.Contains(lowerBody, tag) {
			return false
		}
	}
	if looksJSONBody(lowerBody) {
		return false
	}
	return true
}

// updateAssetExposure：把本次 HTTP 探测判定的 exposure/auth 写回资产。unknown 表示本次判不出来，
// 保留已有值，避免一次 3xx/空响应把之前的结论冲掉。
func updateAssetExposure(ctx context.Context, q dbq, assetID int64, r *probeResult) error {
	if assetID <= 0 || r == nil {
		return nil
	}
	exposure, auth := classifyExposureAuth(r)
	_, err := q.Exec(ctx, `UPDATE assets SET
			exposure_state = CASE WHEN $2 <> 'unknown' THEN $2 ELSE exposure_state END,
			auth_state = CASE WHEN $3 <> 'unknown' THEN $3 ELSE auth_state END
		WHERE id = $1`, assetID, exposure, auth)
	return err
}
