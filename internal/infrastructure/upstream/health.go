package upstream

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/datahub/relay/internal/domain/model"
)

// 商保电子凭证智能服务平台（博思云易）接口规范 V3.0 —— hlt 路由上游。
// 平台按「接口编码」区分服务，请求路径为 {baseURL}/{接口编码}：
//   - 100101001 个人授权备案（拿 authCode）
//   - 700101001 个人健康评测（authCode + idCard → hitCount/diseaseCategory）
const (
	healthAuthPath   = "/100101001"
	healthAssessPath = "/700101001"
)

// HealthConfig holds the 平台 endpoint + 我方在平台侧的凭证与备案固定要素。
// 签名/编码采用 API 版本 2.0（data 走 BASE64，签名 MD5 大写）；3.0（SM2/SM4 国密）
// 未实现，联调需要时再补。
type HealthConfig struct {
	BaseURL          string // https://{host}:{port}/ciras-rest/ins-cl
	AppID            string // 平台分配的应用帐号 appid
	Key              string // appid 对应的签名密钥 key
	APIVersion       string // 默认 "2.0"（BASE64 + MD5）
	ClaimCompanyCode string // 保险公司代码（统一社会信用代码，备案必填）
	ClaimCompanyName string // 保险公司名称（备案必填）
	BusType          string // 业务类型 1:理赔 2:核保 3:其他，默认 "2"
	AuthFileURL      string // 授权文件下载地址（平台要求授权文件/地址二选一）
	AuthFileType     string // 授权文件类型 png/pdf/docx 等，默认 "pdf"
	AuthPath         string // 个人授权备案路径，默认 /100101001
	AssessPath       string // 个人健康评测路径，默认 /700101001
	AssessTypes      string // 评测内容，默认 "1"（风险疾病分类）
}

// HealthClient implements port.UpstreamPort for the 个人健康评测 provider。
// 单次 Query 对上游是两步调用：先「个人授权备案」取 authCode，再以 authCode 发起
// 「个人健康评测」。归一化：命中疾病分类 (hitCount>0) → "001" 查得（message 富对象
// JSON 经下游 result.range 透出）；hitCount=0 → "999" 查无（无风险）；平台返回
// EXXXX / 信封异常 → error（上游侧错误，不计费，走复查/对账兜底）。
type HealthClient struct {
	cfg  HealthConfig
	http *http.Client
}

// NewHealth builds a 个人健康评测 client (填充协议默认值)。
func NewHealth(cfg HealthConfig, httpClient *http.Client) *HealthClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = "2.0"
	}
	if cfg.BusType == "" {
		cfg.BusType = "2"
	}
	if cfg.AuthFileType == "" {
		cfg.AuthFileType = "pdf"
	}
	if cfg.AuthPath == "" {
		cfg.AuthPath = healthAuthPath
	}
	if cfg.AssessPath == "" {
		cfg.AssessPath = healthAssessPath
	}
	if cfg.AssessTypes == "" {
		cfg.AssessTypes = "1"
	}
	return &HealthClient{cfg: cfg, http: httpClient}
}

// healthEnvelope is the platform request/响应公共信封（PDF §2.2）。data 为业务参数
// JSON 的 BASE64；sign 为 MD5(appid=&data=&noise=&key=&version=) 大写 hex。
type healthEnvelope struct {
	AppID   string `json:"appid"`
	Data    string `json:"data"`
	Noise   string `json:"noise"`
	Version string `json:"version"`
	Sign    string `json:"sign"`
}

// Query performs 授权备案 + 健康评测 against the platform and normalizes the
// outcome to 001/999/error。busNo/noise 均由内部幂等流水号 reqid 派生——重试复用
// 相同 noise，平台以 noise 判重（PDF §2.1.2），天然幂等。
func (c *HealthClient) Query(ctx context.Context, req *model.UpstreamRequest) (*model.UpstreamResult, error) {
	// 第一步：个人授权备案 -> authCode。姓名/身份证为平台必填；缺失时交由平台报
	// E0022 参数错误（归一化为上游侧 error，不计费）。
	authData := map[string]string{
		"name":             req.Name,
		"idCard":           req.IDCard,
		"phone":            req.Mobile,
		"busNo":            req.Reqid,
		"claimCompanyCode": c.cfg.ClaimCompanyCode,
		"claimCompanyName": c.cfg.ClaimCompanyName,
		"busType":          c.cfg.BusType,
	}
	if c.cfg.AuthFileURL != "" {
		authData["authFileUrl"] = c.cfg.AuthFileURL
		authData["authFileType"] = c.cfg.AuthFileType
	}
	result, msg, err := c.call(ctx, c.cfg.AuthPath, req.Reqid+"-1", authData)
	if err != nil {
		return nil, fmt.Errorf("health 授权备案: %w", err)
	}
	if result != "S0000" {
		return nil, fmt.Errorf("health 授权备案失败 result=%s message=%s", result, string(msg))
	}
	var auth struct {
		AuthCode string `json:"authCode"`
	}
	if err := json.Unmarshal(msg, &auth); err != nil || auth.AuthCode == "" {
		return nil, fmt.Errorf("health 授权备案未返回 authCode: %s", string(msg))
	}

	// 第二步：个人健康评测（就诊日期区间不传，走平台默认近 2 年）。
	assessData := map[string]string{
		"authCode":    auth.AuthCode,
		"idCard":      req.IDCard,
		"assessTypes": c.cfg.AssessTypes,
	}
	result, msg, err = c.call(ctx, c.cfg.AssessPath, req.Reqid+"-2", assessData)
	if err != nil {
		return nil, fmt.Errorf("health 健康评测: %w", err)
	}
	if result != "S0000" {
		return nil, fmt.Errorf("health 健康评测失败 result=%s message=%s", result, string(msg))
	}
	var assess struct {
		HitCount        int      `json:"hitCount"`
		DiseaseCategory []string `json:"diseaseCategory"`
	}
	if err := json.Unmarshal(msg, &assess); err != nil {
		return nil, fmt.Errorf("health 评测结果解析失败: %w (message=%s)", err, string(msg))
	}
	if assess.HitCount <= 0 && len(assess.DiseaseCategory) == 0 {
		return &model.UpstreamResult{
			Code:  "999",
			Msg:   "未命中风险疾病分类",
			UID:   auth.AuthCode,
			Reqid: req.Reqid,
		}, nil
	}
	return &model.UpstreamResult{
		Code:  "001",
		Msg:   "成功",
		UID:   auth.AuthCode,
		Reqid: req.Reqid,
		Range: compactJSON(msg),
	}, nil
}

// Requery: 平台以 noise 幂等，真正的对账查询接口待联调。在此之前返回
// Reachable=false，记录保持 PENDING 由对账兜底（与既有上游一致）。
func (c *HealthClient) Requery(ctx context.Context, reqid string) (*model.RequeryResult, error) {
	_ = ctx
	_ = reqid
	return &model.RequeryResult{Reachable: false}, nil
}

// call POSTs one signed envelope to {baseURL}{path} and returns the decoded
// data 节点 (result 标识 + message 原文)。业务参数中的空值不参与提交。
func (c *HealthClient) call(ctx context.Context, path, noise string, data map[string]string) (string, json.RawMessage, error) {
	body := make(map[string]string, len(data))
	for k, v := range data {
		if v != "" {
			body[k] = v
		}
	}
	plain, err := json.Marshal(body)
	if err != nil {
		return "", nil, fmt.Errorf("marshal data: %w", err)
	}
	dataB64 := base64.StdEncoding.EncodeToString(plain)
	env := healthEnvelope{
		AppID:   c.cfg.AppID,
		Data:    dataB64,
		Noise:   noise,
		Version: c.cfg.APIVersion,
		Sign:    c.sign(dataB64, noise),
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return "", nil, fmt.Errorf("marshal envelope: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return "", nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	slog.Debug("health request", "url", c.cfg.BaseURL+path, "appid", c.cfg.AppID,
		"noise", noise, "version", c.cfg.APIVersion, "sign", env.Sign)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", nil, fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("read body: %w", err)
	}
	slog.Debug("health response", "status", resp.StatusCode, "raw", string(raw))

	var re struct {
		Data  string `json:"data"`
		Noise string `json:"noise"`
		Sign  string `json:"sign"`
	}
	if err := json.Unmarshal(raw, &re); err != nil {
		return "", nil, fmt.Errorf("decode envelope: %w (raw=%s)", err, string(raw))
	}
	if re.Data == "" {
		return "", nil, fmt.Errorf("响应缺少 data: %s", string(raw))
	}
	// 响应恒为 MD5 签名（PDF §2.1.2 不随版本变化）；验签为可选步骤，失败仅告警，
	// 以免平台侧签名口径偏差阻断联调（HTTPS 已保证信道完整性）。
	if want := c.sign(re.Data, re.Noise); !strings.EqualFold(want, re.Sign) {
		slog.Warn("health 响应验签不一致", "want", want, "got", re.Sign)
	}
	plainResp, err := base64.StdEncoding.DecodeString(re.Data)
	if err != nil {
		return "", nil, fmt.Errorf("decode data base64: %w", err)
	}
	var out struct {
		Result  string          `json:"result"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(plainResp, &out); err != nil {
		return "", nil, fmt.Errorf("decode data json: %w (data=%s)", err, string(plainResp))
	}
	return out.Result, out.Message, nil
}

// sign builds the V2.0 envelope signature: MD5("appid=…&data=…&noise=…&key=…
// &version=…") 大写 hex（PDF §2.3，key 插在 noise 与 version 之间，与官方 Java
// 示例一致；sign 本身不参与签名）。
func (c *HealthClient) sign(dataB64, noise string) string {
	s := "appid=" + c.cfg.AppID + "&data=" + dataB64 + "&noise=" + noise +
		"&key=" + c.cfg.Key + "&version=" + c.cfg.APIVersion
	sum := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// compactJSON returns a compact JSON string for the upstream result object so it
// can be透出 via下游 result.range。空/非法时返回空串。
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
