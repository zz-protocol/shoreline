package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/publicsuffix"
)

// T24 客户托管排除：云厂商分配给「客户」的默认域名（OSS/COS 桶、函数、网站托管、CDN 默认域等）
// 属于托管在云上的客户资产，不是厂商自身资产。命中的资产标 customer_hosted=true（不删），列表/播报默认隐藏。
//
// 匹配规则（只看资产子域名本身，不看 CNAME）：
//   - pattern 必须以 `*.` 开头，`*.` 匹配 ≥1 层子域；
//   - 其余标签里的 `*`（如 oss-*）只匹配本层字符，不跨点；不用正则；
//   - 每条规则至少比服务端点多一层客户子域，服务端点本身不命中（TestBuiltinRulesSkipEndpoints 守住）。

type hostedRule struct {
	Pattern string
	Note    string // 云产品名 + 官方文档
}

// builtinHostedRules 内置规则（架构 10-21 审核通过）。标「按检索摘要核对」的：官方文档页由 JS 渲染，按检索摘要核对格式。
// 首次启动/升级时按 pattern 补种；用户删除的内置规则留墓碑（deleted_at），不会被加回。
var builtinHostedRules = []hostedRule{
	// ---- AWS ----
	{"*.cloudfront.net", "AWS CloudFront 分配默认域名 https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/LinkFormat.html"},
	{"*.s3.amazonaws.com", "AWS S3 存储桶（旧式全局端点） https://docs.aws.amazon.com/AmazonS3/latest/userguide/VirtualHosting.html"},
	{"*.s3.*.amazonaws.com", "AWS S3 存储桶（地域端点） https://docs.aws.amazon.com/AmazonS3/latest/userguide/VirtualHosting.html"},
	{"*.s3-website-*.amazonaws.com", "AWS S3 静态网站（短横线地域） https://docs.aws.amazon.com/AmazonS3/latest/userguide/WebsiteEndpoints.html"},
	{"*.s3-website.*.amazonaws.com", "AWS S3 静态网站（点号地域） https://docs.aws.amazon.com/AmazonS3/latest/userguide/WebsiteEndpoints.html"},
	{"*.*.elb.amazonaws.com", "AWS ELB 负载均衡 DNS 名（us-east-1，<name>.<region>.elb.amazonaws.com） https://docs.aws.amazon.com/elasticloadbalancing/latest/classic/using-domain-names-with-elb.html"},
	{"*.elb.*.amazonaws.com", "AWS ELB 负载均衡 DNS 名（其它地域） https://docs.aws.amazon.com/elasticloadbalancing/latest/application/application-load-balancers.html"},
	{"*.compute-1.amazonaws.com", "AWS EC2 公网 DNS（us-east-1） https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/AMI-launch-index-examples.html"},
	{"*.*.compute.amazonaws.com", "AWS EC2 公网 DNS（其它地域，<ip>.<region>.compute.amazonaws.com） https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/using-instance-addressing.html"},
	{"*.execute-api.*.amazonaws.com", "AWS API Gateway 默认端点 https://docs.aws.amazon.com/apigateway/latest/developerguide/how-to-call-api.html"},
	{"*.lambda-url.*.on.aws", "AWS Lambda 函数 URL https://docs.aws.amazon.com/lambda/latest/dg/urls-invocation.html"},
	{"*.elasticbeanstalk.com", "AWS Elastic Beanstalk 环境域名 https://docs.aws.amazon.com/elasticbeanstalk/latest/dg/customdomains.html"},
	{"*.amplifyapp.com", "AWS Amplify Hosting 默认域名 https://docs.aws.amazon.com/amplify/latest/userguide/custom-domains.html"},
	{"*.awsapprunner.com", "AWS App Runner 服务默认域名 https://docs.aws.amazon.com/apprunner/latest/dg/manage-custom-domains.html"},
	// ---- Azure ----
	{"*.azurewebsites.net", "Azure App Service 默认域名 https://learn.microsoft.com/en-us/azure/app-service/app-service-web-tutorial-custom-domain"},
	{"*.cloudapp.azure.com", "Azure 公共 IP / VM DNS 名 https://learn.microsoft.com/en-us/azure/virtual-network/ip-services/public-ip-addresses"},
	{"*.blob.core.windows.net", "Azure Storage Blob 端点 https://learn.microsoft.com/en-us/azure/storage/common/storage-account-overview"},
	{"*.web.core.windows.net", "Azure Storage 静态网站端点 https://learn.microsoft.com/en-us/azure/storage/common/storage-account-overview"},
	{"*.file.core.windows.net", "Azure Storage File 端点 https://learn.microsoft.com/en-us/azure/storage/common/storage-account-overview"},
	{"*.queue.core.windows.net", "Azure Storage Queue 端点 https://learn.microsoft.com/en-us/azure/storage/common/storage-account-overview"},
	{"*.table.core.windows.net", "Azure Storage Table 端点 https://learn.microsoft.com/en-us/azure/storage/common/storage-account-overview"},
	{"*.dfs.core.windows.net", "Azure Data Lake Storage 端点 https://learn.microsoft.com/en-us/azure/storage/common/storage-account-overview"},
	{"*.azureedge.net", "Azure CDN 端点 https://learn.microsoft.com/en-us/azure/cdn/cdn-map-content-to-custom-domain"},
	{"*.azurefd.net", "Azure Front Door 端点 https://learn.microsoft.com/en-us/azure/frontdoor/front-door-custom-domain"},
	{"*.azurestaticapps.net", "Azure Static Web Apps 默认域名 https://learn.microsoft.com/en-us/azure/static-web-apps/custom-domain"},
	{"*.trafficmanager.net", "Azure Traffic Manager 配置文件域名 https://learn.microsoft.com/en-us/azure/traffic-manager/traffic-manager-point-internet-domain"},
	{"*.azure-api.net", "Azure API Management 网关 https://learn.microsoft.com/en-us/azure/api-management/configure-custom-domain"},
	{"*.azurecontainerapps.io", "Azure Container Apps 默认域名 https://learn.microsoft.com/en-us/azure/container-apps/connect-apps"},
	// ---- GCP / Firebase ----
	{"*.appspot.com", "Google App Engine 默认域名 https://cloud.google.com/appengine/docs/standard/how-requests-are-routed"},
	{"*.run.app", "Google Cloud Run 服务 URL https://cloud.google.com/run/docs/triggering/https-request"},
	{"*.cloudfunctions.net", "Google Cloud Functions（1st gen）URL https://cloud.google.com/functions/docs/calling/http"},
	{"*.storage.googleapis.com", "Google Cloud Storage 虚拟主机式桶 URL https://cloud.google.com/storage/docs/request-endpoints"},
	{"*.web.app", "Firebase Hosting 默认域名 https://firebase.google.com/docs/hosting/custom-domain"},
	{"*.firebaseapp.com", "Firebase Hosting 默认域名 https://firebase.google.com/docs/hosting/custom-domain"},
	// ---- 阿里云 ----
	{"*.oss-*.aliyuncs.com", "阿里云 OSS 存储桶域名 https://help.aliyun.com/zh/oss/user-guide/regions-and-endpoints"},
	{"*.fcapp.run", "阿里云函数计算默认域名（按检索摘要核对） https://help.aliyun.com/zh/functioncompute/fc-2-0/use-cases/use-fcapp-run-to-run-restful-applications"},
	{"*.*.fc.aliyuncs.com", "阿里云函数计算账号端点 <账号>.<地域>.fc.aliyuncs.com（按检索摘要核对） https://help.aliyun.com/zh/functioncompute/fc/404-error-occurs-when-i-use-a-browser-or-curl-tool-to-access-a-function-1"},
	{"*.kunlunsl.com", "阿里云 CDN 加速 CNAME 域名（按检索摘要核对） https://help.aliyun.com/zh/cdn/add-a-cname-record-for-a-domain-name"},
	{"*.alikunlun.com", "阿里云 CDN 加速 CNAME 域名（按检索摘要核对） https://help.aliyun.com/zh/cdn/add-a-cname-record-for-a-domain-name"},
	{"*.kunlunca.com", "阿里云 CDN 加速 CNAME 域名（按检索摘要核对） https://help.aliyun.com/zh/cdn/add-a-cname-record-for-a-domain-name"},
	// ---- 腾讯云 ----
	{"*.cos.*.myqcloud.com", "腾讯云 COS 存储桶默认域名 https://cloud.tencent.com/document/product/436/6224"},
	{"*.cos.accelerate.myqcloud.com", "腾讯云 COS 全球加速域名 https://cloud.tencent.com/document/product/436/6224"},
	{"*.cos-website.*.myqcloud.com", "腾讯云 COS 静态网站访问节点 https://cloud.tencent.com/document/product/436/32670"},
	{"*.file.myqcloud.com", "腾讯云 COS 默认 CDN 加速域名 https://cloud.tencent.com/document/product/436/65819"},
	{"*.cdn.dnsv1.com", "腾讯云 CDN 加速 CNAME 域名（按检索摘要核对） https://cloud.tencent.com/document/product/228/3121"},
	{"*.tencentscf.com", "腾讯云云函数 SCF 函数 URL（按检索摘要核对） https://cloud.tencent.com/document/product/583/96099"},
	{"*.apigw.tencentcs.com", "腾讯云 API 网关默认域名（按检索摘要核对） https://cloud.tencent.com/document/product/628/11810"},
	{"*.tencentapigw.com", "腾讯云 API 网关默认域名（按检索摘要核对） https://cloud.tencent.com/document/product/628/11810"},
	// ---- 华为云 ----
	{"*.obs.*.myhuaweicloud.com", "华为云 OBS 桶访问域名 https://support.huaweicloud.com/intl/en-us/productdesc-obs/obs_03_0152.html"},
	{"*.obs-website.*.myhuaweicloud.com", "华为云 OBS 静态网站托管域名 https://support.huaweicloud.com/intl/en-us/productdesc-obs/obs_03_0152.html"},
	{"*.cdnhwc1.com", "华为云 CDN 加速 CNAME 域名 https://support.huaweicloud.com/cdn_faq/cdn_faq_0111.html"},
	// ---- 火山引擎 / 百度智能云 / 七牛 ----
	{"*.tos-*.volces.com", "火山引擎 TOS 桶域名（按检索摘要核对） https://www.volcengine.com/docs/6349/107356"},
	{"*.*.bcebos.com", "百度智能云 BOS 桶域名 <bucket>.<region>.bcebos.com https://cloud.baidu.com/doc/BOS/s/akrqd2wcx"},
	{"*.qiniudns.com", "七牛云 CDN 加速 CNAME 域名（按检索摘要核对） https://developer.qiniu.com/fusion/4941/cname-configuration"},
	// ---- Cloudflare ----
	{"*.cdn.cloudflare.net", "Cloudflare 代理 CNAME 目标（CNAME 接入） https://developers.cloudflare.com/dns/zone-setups/partial-setup/setup/"},
	{"*.workers.dev", "Cloudflare Workers 默认域名 https://developers.cloudflare.com/workers/configuration/routing/workers-dev/"},
	{"*.pages.dev", "Cloudflare Pages 默认域名 https://developers.cloudflare.com/pages/configuration/custom-domains/"},
	{"*.r2.dev", "Cloudflare R2 公共桶开发域名 https://developers.cloudflare.com/r2/buckets/public-buckets/"},
	// ---- Akamai ----
	{"*.edgekey.net", "Akamai 边缘主机名（Enhanced TLS） https://techdocs.akamai.com/property-mgr/docs/edge-hostnames"},
	{"*.edgesuite.net", "Akamai 边缘主机名（Standard TLS） https://techdocs.akamai.com/property-mgr/docs/edge-hostnames"},
	{"*.akamaized.net", "Akamai 边缘主机名（共享证书） https://techdocs.akamai.com/property-mgr/docs/edge-hostnames"},
	// ---- Fastly ----
	{"*.global.fastly.net", "Fastly 服务 CNAME 目标 https://docs.fastly.com/en/guides/adding-cname-records"},
	// ---- 通用托管 ----
	{"*.github.io", "GitHub Pages 默认域名 https://docs.github.com/en/pages/configuring-a-custom-domain-for-your-github-pages-site/managing-a-custom-domain-for-your-github-pages-site"},
	{"*.herokuapp.com", "Heroku 应用默认域名 https://devcenter.heroku.com/articles/custom-domains"},
}

var hostedLabelRe = regexp.MustCompile(`^[a-z0-9*-]+$`)

// validHostedPattern：以 `*.` 开头，各标签只含 a-z0-9-*；最后一个含 * 的标签之后的固定后缀
// 必须是 eTLD+1 或更长（按 golang.org/x/net/publicsuffix 判断），防止 `*.*.com`、`*.co.uk` 把整个公共后缀排除。
func validHostedPattern(p string) bool {
	return hostedPatternErr(p) == ""
}

// hostedPatternErr 返回中文错误说明，合法返回空串。
func hostedPatternErr(p string) string {
	rest, ok := strings.CutPrefix(p, "*.")
	if !ok || len(p) > 253 {
		return "规则须以 *. 开头"
	}
	labels := strings.Split(rest, ".")
	last := -1
	for i, l := range labels {
		if !hostedLabelRe.MatchString(l) {
			return "标签只能包含 a-z、0-9、- 和 *"
		}
		if strings.Contains(l, "*") {
			last = i
		}
	}
	suffix := strings.Join(labels[last+1:], ".")
	if suffix == "" {
		return "规则末尾须是固定域名，不能以 * 结尾"
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(suffix); err != nil {
		return "通配符之后的固定后缀「" + suffix + "」是公共后缀，至少要到可注册域名（eTLD+1）一级，如 *.oss-*.aliyuncs.com"
	}
	return ""
}

// matchHosted：host 是否命中 pattern（pattern 已校验）。`*.` 匹配 ≥1 层，其余标签逐层匹配，标签内 * 不跨点。
func matchHosted(pattern, host string) bool {
	rest, ok := strings.CutPrefix(pattern, "*.")
	if !ok {
		return false
	}
	pl := strings.Split(rest, ".")
	hl := strings.Split(host, ".")
	if len(hl) <= len(pl) {
		return false
	}
	hl = hl[len(hl)-len(pl):]
	for i := range pl {
		// 标签只含 a-z0-9-*，path.Match 的 * 不跨 '/'，标签里没有 '/' 和 '.'，等价于本层通配
		if m, _ := path.Match(pl[i], hl[i]); !m {
			return false
		}
	}
	return true
}

func isCustomerHosted(host string, patterns []string) bool {
	for _, p := range patterns {
		if matchHosted(p, host) {
			return true
		}
	}
	return false
}

// loadHostedPatterns 读取启用且未删除的规则。
func loadHostedPatterns(ctx context.Context, d *pgxpool.Pool) ([]string, error) {
	rows, err := d.Query(ctx, `SELECT pattern FROM customer_hosted_rules WHERE enabled AND deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// seedHostedRules 补种内置规则：按 pattern 去重，已存在（含被删墓碑）的不动。返回新插入条数。
func seedHostedRules(ctx context.Context, d *pgxpool.Pool) (int, error) {
	n := 0
	for _, r := range builtinHostedRules {
		tag, err := d.Exec(ctx, `INSERT INTO customer_hosted_rules (pattern, note, builtin) VALUES ($1, $2, true)
			ON CONFLICT (pattern) DO NOTHING`, r.Pattern, r.Note)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// recalcHosted 按 id 分批重算全部资产的 customer_hosted，只更新有变化的行，进度写 customer_hosted_tasks。
func recalcHosted(ctx context.Context, d *pgxpool.Pool, taskID int64, batch int) error {
	if _, err := d.Exec(ctx, `UPDATE customer_hosted_tasks SET status = 'running', processed = 0, changed = 0, error = NULL WHERE id = $1`, taskID); err != nil {
		return err
	}
	var cursor int64
	for {
		pats, err := loadHostedPatterns(ctx, d) // 每批重读，过程中规则再变以最新为准
		if err != nil {
			return err
		}
		rows, err := d.Query(ctx, `SELECT id, subdomain FROM assets WHERE id > $1 ORDER BY id LIMIT $2`, cursor, batch)
		if err != nil {
			return err
		}
		var ids []int64
		var vals []bool
		for rows.Next() {
			var id int64
			var host string
			if err := rows.Scan(&id, &host); err != nil {
				return err
			}
			ids, vals = append(ids, id), append(vals, isCustomerHosted(host, pats))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			_, err := d.Exec(ctx, `UPDATE customer_hosted_tasks SET status = 'done', finished_at = now() WHERE id = $1`, taskID)
			return err
		}
		tag, err := d.Exec(ctx, `UPDATE assets SET customer_hosted = v.s FROM unnest($1::bigint[], $2::bool[]) v(id, s)
			WHERE assets.id = v.id AND assets.customer_hosted IS DISTINCT FROM v.s`, ids, vals)
		if err != nil {
			return err
		}
		if _, err := d.Exec(ctx, `UPDATE customer_hosted_tasks SET processed = processed + $2, changed = changed + $3 WHERE id = $1`,
			taskID, len(ids), tag.RowsAffected()); err != nil {
			return err
		}
		cursor = ids[len(ids)-1]
	}
}

// startHostedTask 建重算任务并在后台执行。
func startHostedTask(ctx, bg context.Context, d *pgxpool.Pool, batch int) (int64, error) {
	var id int64
	if err := d.QueryRow(ctx, `INSERT INTO customer_hosted_tasks DEFAULT VALUES RETURNING id`).Scan(&id); err != nil {
		return 0, err
	}
	go runHostedTask(bg, d, id, batch)
	return id, nil
}

func runHostedTask(bg context.Context, d *pgxpool.Pool, id int64, batch int) {
	if err := recalcHosted(bg, d, id, batch); err != nil {
		log.Printf("customer_hosted task %d: %v", id, err)
		d.Exec(bg, `UPDATE customer_hosted_tasks SET status = 'failed', error = $2, finished_at = now() WHERE id = $1`, id, err.Error())
	}
}

// initHosted 启动时：补种内置规则；续跑未完成的重算任务；有新补种的规则则重算一次。
func (a *api) initHosted(ctx context.Context) error {
	n, err := seedHostedRules(ctx, a.db)
	if err != nil {
		return err
	}
	rows, err := a.db.Query(ctx, `SELECT id FROM customer_hosted_tasks WHERE status IN ('pending', 'running') ORDER BY id`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		go runHostedTask(a.bg, a.db, id, a.cfg.ScopeBatch)
	}
	if n > 0 && len(ids) == 0 {
		_, err = startHostedTask(ctx, a.bg, a.db, a.cfg.ScopeBatch)
	}
	return err
}

func (a *api) hostedChanged(w http.ResponseWriter, r *http.Request, ruleID int64) {
	taskID, err := startHostedTask(r.Context(), a.bg, a.db, a.cfg.ScopeBatch)
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"rule_id": ruleID, "task_id": taskID})
}

// listHostedRules：未删除的规则（含停用的）。
func (a *api) listHostedRules(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT id, pattern, note, builtin, enabled, created_at FROM customer_hosted_rules WHERE deleted_at IS NULL ORDER BY id`)
}

// createHostedRule：新增自定义规则；与已删除的同名规则冲突时恢复该条（启用）。
func (a *api) createHostedRule(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Pattern string `json:"pattern"`
		Note    string `json:"note"`
	}
	if !decode(w, r, &b) {
		return
	}
	pat := strings.ToLower(strings.TrimSpace(b.Pattern))
	if msg := hostedPatternErr(pat); msg != "" {
		writeErr(w, 400, "invalid pattern: "+msg)
		return
	}
	var id int64
	err := a.db.QueryRow(r.Context(), `INSERT INTO customer_hosted_rules (pattern, note) VALUES ($1, $2)
		ON CONFLICT (pattern) DO UPDATE SET deleted_at = NULL, enabled = true, note = EXCLUDED.note
			WHERE customer_hosted_rules.deleted_at IS NOT NULL
		RETURNING id`, pat, b.Note).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 409, "pattern already exists")
		return
	}
	if err != nil {
		dbErr(w, err)
		return
	}
	a.hostedChanged(w, r, id)
}

// updateHostedRule：启用/停用。
func (a *api) updateHostedRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	var b struct {
		Enabled *bool `json:"enabled"`
	}
	if !ok || !decode(w, r, &b) {
		return
	}
	if b.Enabled == nil {
		writeErr(w, 400, "enabled required")
		return
	}
	tag, err := a.db.Exec(r.Context(), `UPDATE customer_hosted_rules SET enabled = $2 WHERE id = $1 AND deleted_at IS NULL`, id, *b.Enabled)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "not found")
		return
	}
	a.hostedChanged(w, r, id)
}

// deleteHostedRule：软删除（墓碑），内置规则升级时不会加回。
func (a *api) deleteHostedRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	tag, err := a.db.Exec(r.Context(), `UPDATE customer_hosted_rules SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "not found")
		return
	}
	a.hostedChanged(w, r, id)
}

func (a *api) getHostedTask(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r, "id"); ok {
		a.one(w, r, 200, `SELECT id, status, processed, changed, error, created_at, finished_at FROM customer_hosted_tasks WHERE id = $1`, id)
	}
}
