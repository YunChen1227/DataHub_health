//go:build ignore

// Mock 商保电子凭证智能服务平台 (博思云易) upstream implementing the V2.0 envelope
// (appid/data(BASE64)/noise/sign(MD5 大写)/version) for hlt full-link testing.
// Run: go run scripts/mock_health.go
//
// Endpoints ({base}=/ciras-rest/ins-cl):
//   - {base}/100101001 个人授权备案: 验签失败 -> E0024；缺 name/idCard -> E0022；
//     phone=13800000000 -> authCode "AF-NOHIT-0001"（评测无命中）；否则 "AF-HIT-0001"。
//   - {base}/700101001 个人健康评测: 验签失败 -> E0024；authCode 前缀 AF-NOHIT ->
//     hitCount=0（查无/无风险）；否则 hitCount=3 + 疾病分类列表。
package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var key = env("HLT_UPSTREAM_KEY", "demo-health-key")

// sign mirrors PDF §2.3: MD5("appid=&data=&noise=&key=&version=") 大写 hex。
func sign(appid, data, noise, version string) string {
	s := "appid=" + appid + "&data=" + data + "&noise=" + noise + "&key=" + key + "&version=" + version
	sum := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

type envelope struct {
	AppID   string `json:"appid"`
	Data    string `json:"data"`
	Noise   string `json:"noise"`
	Version string `json:"version"`
	Sign    string `json:"sign"`
}

// respond wraps the data 节点 into the signed response envelope (响应恒为 MD5 签名)。
func respond(w http.ResponseWriter, appid, noise, version, result string, message any) {
	plain, _ := json.Marshal(map[string]any{"result": result, "message": message})
	dataB64 := base64.StdEncoding.EncodeToString(plain)
	resp := map[string]string{
		"data":  dataB64,
		"noise": noise,
		"sign":  sign(appid, dataB64, noise, version),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// decode parses the request envelope, verifies the signature and returns the
// decoded business params. ok=false 时已写出错误应答。
func decode(w http.ResponseWriter, r *http.Request) (envelope, map[string]string, bool) {
	raw, _ := io.ReadAll(r.Body)
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		respond(w, e.AppID, e.Noise, e.Version, "E0023", "参数转换异常")
		return e, nil, false
	}
	if sign(e.AppID, e.Data, e.Noise, e.Version) != e.Sign {
		respond(w, e.AppID, e.Noise, e.Version, "E0024", "安全码错误(签名失败)")
		return e, nil, false
	}
	plain, err := base64.StdEncoding.DecodeString(e.Data)
	if err != nil {
		respond(w, e.AppID, e.Noise, e.Version, "E0023", "参数转换异常")
		return e, nil, false
	}
	var params map[string]string
	if err := json.Unmarshal(plain, &params); err != nil {
		respond(w, e.AppID, e.Noise, e.Version, "E0023", "参数转换异常")
		return e, nil, false
	}
	return e, params, true
}

func main() {
	addr := env("MOCK_HEALTH_ADDR", ":9116")

	// 100101001 个人授权备案
	http.HandleFunc("/ciras-rest/ins-cl/100101001", func(w http.ResponseWriter, r *http.Request) {
		e, p, ok := decode(w, r)
		if !ok {
			log.Printf("auth <- bad envelope/sign")
			return
		}
		if p["name"] == "" || p["idCard"] == "" || p["busNo"] == "" {
			respond(w, e.AppID, e.Noise, e.Version, "E0022", "参数错误，请核对后重试")
			log.Printf("auth <- missing params: %v", p)
			return
		}
		authCode := "AF-HIT-0001"
		if p["phone"] == "13800000000" {
			authCode = "AF-NOHIT-0001"
		}
		respond(w, e.AppID, e.Noise, e.Version, "S0000", map[string]string{"authCode": authCode})
		log.Printf("auth <- phone=%s busNo=%s -> %s", p["phone"], p["busNo"], authCode)
	})

	// 700101001 个人健康评测
	http.HandleFunc("/ciras-rest/ins-cl/700101001", func(w http.ResponseWriter, r *http.Request) {
		e, p, ok := decode(w, r)
		if !ok {
			log.Printf("assess <- bad envelope/sign")
			return
		}
		if p["authCode"] == "" || p["idCard"] == "" {
			respond(w, e.AppID, e.Noise, e.Version, "E0022", "参数错误，请核对后重试")
			return
		}
		if strings.HasPrefix(p["authCode"], "AF-NOHIT") {
			respond(w, e.AppID, e.Noise, e.Version, "S0000",
				map[string]any{"hitCount": 0, "diseaseCategory": []string{}})
			log.Printf("assess <- authCode=%s -> hitCount=0", p["authCode"])
			return
		}
		respond(w, e.AppID, e.Noise, e.Version, "S0000", map[string]any{
			"hitCount":        3,
			"diseaseCategory": []string{"常规慢性病", "严重遗传性疾病", "恶性肿瘤"},
		})
		log.Printf("assess <- authCode=%s -> hitCount=3", p["authCode"])
	})

	fmt.Printf("mock 个人健康评测 upstream listening on %s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
