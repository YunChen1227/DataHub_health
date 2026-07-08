//go:build ignore

// 02_hlt_query: hlt 版本 POST /v1/openapi/zlx/querySrmxHLT（x1 信封格式；
// 内部对接商保电子凭证智能服务平台-个人健康评测 mock，两步：授权备案->健康评测）。
// 全场景：成功(命中疾病分类，富对象 JSON range)/查无(无命中)/鉴权与参数错误。
//
// Run: go run test/cases/02_hlt_query.go
package main

import (
	"encoding/json"
	"strings"

	"github.com/datahub/relay/test/harness"
)

const version = "hlt"

func base() map[string]string {
	return map[string]string{"mobile": "13809091009", "idCard": "330129199109094312", "name": "张三"}
}

func main() {
	rec := harness.NewRecorder("02_hlt_query", "hlt 主接口全场景 (个人健康评测)")
	defer rec.Finish()

	r := harness.Query(version, harness.AppKeyFor(version), harness.Secret, base(), nil)
	rec.Check("成功查得", "errorCode=0 & body.code=001 & range 含 hitCount>0",
		r.ErrorCode == "0" && r.BodyCode == "001" && parseRangeHit(r.Range), r.Raw)

	nf := base()
	nf["mobile"] = "13800000000"
	r = harness.Query(version, harness.AppKeyFor(version), harness.Secret, nf, nil)
	rec.Check("查无结果(无命中)", "errorCode=0 & body.code=999", r.ErrorCode == "0" && r.BodyCode == "999", r.Raw)

	r = harness.Query(version, harness.AppKeyFor(version), harness.Secret, base(), map[string]any{"sign": "deadbeef"})
	rec.Check("错误签名", "errorCode=505002 且无 body", r.ErrorCode == "505002" && r.BodyCode == "", r.Raw)

	r = harness.Query(version, "nonexistent-appkey", harness.Secret, base(), nil)
	rec.Check("未知 appKey", "errorCode=505004", r.ErrorCode == "505004", r.Raw)

	r = harness.Query(version, "", harness.Secret, base(), map[string]any{"appKey": ""})
	rec.Check("缺失 appKey", "errorCode=505001", r.ErrorCode == "505001", r.Raw)

	badm := base()
	badm["mobile"] = "139xx"
	r = harness.Query(version, harness.AppKeyFor(version), harness.Secret, badm, nil)
	rec.Check("手机号非法", "errorCode=505062", r.ErrorCode == "505062", r.Raw)

	badi := base()
	badi["idCard"] = "12345"
	r = harness.Query(version, harness.AppKeyFor(version), harness.Secret, badi, nil)
	rec.Check("身份证非法", "errorCode=505062", r.ErrorCode == "505062", r.Raw)

	// 上游授权备案要求 name 必传：网关前置拦截，不调用上游（与对外手册口径一致）。
	noName := base()
	delete(noName, "name")
	r = harness.Query(version, harness.AppKeyFor(version), harness.Secret, noName, nil)
	rec.Check("缺 name 拦截", "errorCode=505062", r.ErrorCode == "505062", r.Raw)

	r = harness.Query(version, harness.AppKeyFor(version), harness.Secret, base(), nil)
	rec.Check("二次成功查得", "errorCode=0 & body.code=001 & range 含 diseaseCategory",
		r.ErrorCode == "0" && r.BodyCode == "001" && strings.Contains(r.Range, "diseaseCategory"), r.Raw)
}

// parseRangeHit 校验 result.range 是评测富对象 JSON 且 hitCount > 0。
func parseRangeHit(raw string) bool {
	if raw == "" {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return false
	}
	hit, ok := m["hitCount"].(float64)
	return ok && hit > 0
}
