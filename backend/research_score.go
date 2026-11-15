package main

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// T54：研究价值评分。四维等权（各 0.25），总分 0–100，落库两位小数。
// 分数是全局口径（与列表 vendor 筛选无关），由 recomputeResearchScores 批量重算；
// 列表只按落库列排序/筛选，不在查询时动态计算。
const (
	researchScoreWeight    = 0.25
	researchUsageSmoothing = 5.0
	researchScoreBatchSize = 1000
)

const (
	researchAttackServiceCore  = 1.0  // service 且分类含 Databases/Caching/Message queues
	researchAttackServiceOther = 0.75 // 其它 service
	researchAttackWeb          = 0.4  // Web 组件
	researchAttackUnknown      = 0.3  // 无分类，无法判断攻击面

	researchSourceEnriched = 1.0 // repo 非空且富集成功
	researchSourceUnknown  = 0.5 // repo 为空或尚未富集
	researchSourceFailed   = 0.3 // repo 非空但富集失败

	researchAdvisoryUnknown = 0.5 // advisory_count 为 NULL
	researchAdvisoryNone    = 1.0 // 0 条公开披露，只用于排序，不代表安全
)

// researchAttackCategories：服务组件里攻击面最高的一档分类（SPEC 第九轮 T54 口径）。
var researchAttackCategories = []string{"Databases", "Caching", "Message queues"}

// researchScoreParts 是四维原始分（0–1），存 components.research_score_parts。
type researchScoreParts struct {
	Attack   float64 `json:"attack"`
	Source   float64 `json:"source"`
	Advisory float64 `json:"advisory"`
	Usage    float64 `json:"usage"`
}

func hasResearchAttackCategory(categories []string) bool {
	for _, c := range categories {
		c = strings.TrimSpace(c)
		for _, want := range researchAttackCategories {
			if strings.EqualFold(c, want) {
				return true
			}
		}
	}
	return false
}

// researchAttackScore：service 按分类分档；Web 组件按有无分类分档。
// T52 写入的 service 组件一定带分类，因此空分类只在 Web 组件上出现。
func researchAttackScore(source string, categories []string) float64 {
	if strings.EqualFold(strings.TrimSpace(source), "service") {
		if hasResearchAttackCategory(categories) {
			return researchAttackServiceCore
		}
		return researchAttackServiceOther
	}
	if len(categories) == 0 {
		return researchAttackUnknown
	}
	return researchAttackWeb
}

// researchSourceScore：能关联到开源 repo 且富集成功才给满分；未富集中性。
func researchSourceScore(repo, enrichError string, enrichedAt *time.Time) float64 {
	if strings.TrimSpace(repo) == "" || enrichedAt == nil {
		return researchSourceUnknown
	}
	if strings.TrimSpace(enrichError) != "" {
		return researchSourceFailed
	}
	return researchSourceEnriched
}

// researchAdvisoryScore：0 条最高，NULL 中性，n>0 按 1/(1+n) 递减；只用于排序，不代表安全。
func researchAdvisoryScore(advisories *int) float64 {
	if advisories == nil {
		return researchAdvisoryUnknown
	}
	if *advisories <= 0 {
		return researchAdvisoryNone
	}
	return 1 / (1 + float64(*advisories))
}

// researchUsageScore：n 为该组件在单一厂商内出现的最大去重资产数，n/(n+5) 归一化。
func researchUsageScore(n int64) float64 {
	if n <= 0 {
		return 0
	}
	return float64(n) / (float64(n) + researchUsageSmoothing)
}

// researchTotal：四维等权汇总为 0–100，保留两位小数。
func researchTotal(p researchScoreParts) float64 {
	sum := p.Attack + p.Source + p.Advisory + p.Usage
	return math.Round(sum*researchScoreWeight*100*100) / 100
}

type researchScoreDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// researchScoreRowsSQL：一次聚合出全部组件与「单一厂商内最大去重资产数」。
// usage 复用列表默认统计口径（排除泛解析/客户托管/非 alive），与 include_* 切换无关。
const researchScoreRowsSQL = `WITH usage AS (
	SELECT component_id, max(n)::bigint AS n FROM (
		SELECT s.component_id, s.vendor_id, count(*)::bigint AS n
		FROM (
			SELECT uc.component_id, r.vendor_id, u.asset_id
			` + scopedURLSQL + `
			UNION
			SELECT ac.component_id, r.vendor_id, a.id
			` + scopedAssetSQL + `
		) s
		GROUP BY s.component_id, s.vendor_id
	) t
	GROUP BY component_id
)
SELECT c.id, c.source, COALESCE(c.github_repo, ''), COALESCE(c.enrich_error, ''), c.enriched_at,
	c.advisory_count, COALESCE(c.categories, '{}'::text[]), COALESCE(u.n, 0)
FROM components c
LEFT JOIN usage u ON u.component_id = c.id
ORDER BY c.id`

// recomputeResearchScores 批量重算全部组件的研究价值分：一次聚合查询 + 分批 UPDATE，不逐组件查询。
// 幂等：同一数据重复执行得到同一分数（research_scored_at 会更新）。
func recomputeResearchScores(ctx context.Context, q researchScoreDB) (int, error) {
	rows, err := q.Query(ctx, researchScoreRowsSQL, false, false, false, (*int64)(nil))
	if err != nil {
		return 0, err
	}
	type item struct {
		id    int64
		score float64
		parts researchScoreParts
	}
	var batch []item
	total := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		payload := make([]map[string]any, len(batch))
		for i, it := range batch {
			payload[i] = map[string]any{"id": it.id, "score": it.score, "parts": it.parts}
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE components c
			SET research_score = x.score, research_score_parts = x.parts, research_scored_at = now()
			FROM jsonb_to_recordset($1::jsonb) AS x(id bigint, score numeric, parts jsonb)
			WHERE c.id = x.id`, string(raw)); err != nil {
			return err
		}
		total += len(batch)
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var id, n int64
		var source, repo, enrichErr string
		var enrichedAt *time.Time
		var advisories *int
		var categories []string
		if err := rows.Scan(&id, &source, &repo, &enrichErr, &enrichedAt, &advisories, &categories, &n); err != nil {
			rows.Close()
			return total, err
		}
		parts := researchScoreParts{
			Attack:   researchAttackScore(source, categories),
			Source:   researchSourceScore(repo, enrichErr, enrichedAt),
			Advisory: researchAdvisoryScore(advisories),
			Usage:    researchUsageScore(n),
		}
		batch = append(batch, item{id: id, score: researchTotal(parts), parts: parts})
		if len(batch) >= researchScoreBatchSize {
			if err := flush(); err != nil {
				rows.Close()
				return total, err
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return total, err
	}
	return total, flush()
}

// normalizeResearchScoreParts：pgx 的 RowToMap 把 JSONB 解成字符串，API 返回对象，取回后统一转换。
func normalizeResearchScoreParts(list []map[string]any) {
	for _, row := range list {
		v, ok := row["research_score_parts"]
		if !ok {
			continue
		}
		var raw string
		switch t := v.(type) {
		case string:
			raw = t
		case []byte:
			raw = string(t)
		default:
			continue
		}
		var parts map[string]any
		if err := json.Unmarshal([]byte(raw), &parts); err != nil {
			continue
		}
		row["research_score_parts"] = parts
	}
}
