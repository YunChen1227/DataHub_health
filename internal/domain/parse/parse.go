// Package parse validates and normalises the client request into a normalized
// upstream request shape. hlt 上游（商保电子凭证平台）的个人授权备案要求
// mobile/idCard/name 三者必填——网关校验口径与上游要求一致，前置拦截而非
// 透传给上游报错（对外手册承诺"参数非法不调用上游、不计费"）。
package parse

import (
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/datahub/relay/internal/common/errs"
	"github.com/datahub/relay/internal/domain/model"
)

var (
	mobileRe = regexp.MustCompile(`^1\d{10}$`)
	idCardRe = regexp.MustCompile(`^\d{17}[\dX]$`)
)

// Parse runs参数校验; failures return busiCode 1007 数据请求异常 (我方拦截, 不调
// 上游/不计费). It generates an internal upstream reqid (≤20).
func Parse(cmd *model.QueryCommand) (*model.UpstreamRequest, error) {
	if cmd == nil {
		return nil, errs.New(errs.BusiDataRequestErr, "请求体为空")
	}
	name := strings.TrimSpace(cmd.Name) // hlt 上游授权备案必填
	mobile := strings.TrimSpace(cmd.Mobile)
	idCard := strings.ToUpper(strings.TrimSpace(cmd.IDCard))

	if !mobileRe.MatchString(mobile) {
		return nil, errs.New(errs.BusiDataRequestErr, "mobile 格式非法")
	}
	if !idCardRe.MatchString(idCard) {
		return nil, errs.New(errs.BusiDataRequestErr, "idCard 格式非法")
	}
	if name == "" {
		return nil, errs.New(errs.BusiDataRequestErr, "name 不能为空")
	}

	return &model.UpstreamRequest{
		IDCard: idCard,
		Name:   name,
		Mobile: mobile,
		Reqid:  NewReqid(),
	}, nil
}

// reqidSeq guarantees in-process uniqueness even when the wall clock does not
// advance between two rapid calls (Windows time.Now() can have coarse ~ms
// granularity, so consecutive UnixNano() values may be identical and cause
// reqid collisions → idempotency replay).
var reqidSeq atomic.Uint64

// NewReqid generates an internal upstream reqid（base36 时间戳 + 进程内自增序号，
// ≤20 位，满足各上游 reqid ≤20 的约束并保证同进程内绝不重复）。
func NewReqid() string {
	ts := strconv.FormatInt(time.Now().UnixNano(), 36) // ≤13 位
	seq := strconv.FormatUint(reqidSeq.Add(1)%46656, 36) // 1–3 位 (36^3)
	r := ts + seq
	if len(r) > 20 {
		r = r[:20]
	}
	return r
}
