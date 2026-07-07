package main

import (
	"fmt"
	"os"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"gopkg.in/yaml.v3"
)

// upstreamConfig holds a single version's upstream endpoint + 我方在该上游侧的
// 凭证。kind 决定使用哪种上游客户端：health(商保电子凭证智能服务平台-个人健康
// 评测, hlt)。
type upstreamConfig struct {
	kind    string // health
	baseURL string
	// health (商保电子凭证智能服务平台) 凭证 + 备案固定要素
	appID            string // 平台分配的应用帐号 appid
	key              string // appid 对应的签名密钥 key
	apiVersion       string // 默认 "2.0"（BASE64+MD5；3.0 国密未实现）
	claimCompanyCode string // 保险公司代码（统一社会信用代码，备案必填）
	claimCompanyName string // 保险公司名称（备案必填）
	busType          string // 业务类型 1:理赔 2:核保 3:其他，默认 "2"
	authFileURL      string // 授权文件下载地址（备案要求授权文件/地址二选一）
	authFileType     string // 授权文件类型 png/pdf/docx 等
	authPath         string // 授权备案路径，默认 /100101001
	assessPath       string // 健康评测路径，默认 /700101001
	assessTypes      string // 评测内容，默认 "1" 风险疾病分类
}

// dbConfig is a single version's PostgreSQL connection (独立数据库)。
type dbConfig struct {
	host     string
	port     int
	name     string
	user     string
	password string
	sslmode  string
	maxConns int
}

// redisConfig is a single version's Redis logical DB (独立计数器)。
type redisConfig struct {
	addr     string
	username string
	password string
	db       int
	poolSize int
}

// versionConfig is the full per-version dependency config (独立上游 + 独立库 +
// 独立 Redis)。三版本对外接口完全一致，仅靠路由名区分。
type versionConfig struct {
	upstream upstreamConfig
	db       dbConfig
	redis    redisConfig
}

// dsn builds a libpq key/value DSN (safe for passwords with special chars).
func (d dbConfig) dsn() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=10 pool_max_conns=%d",
		d.host, d.port, d.user, d.password, d.name, d.sslmode, d.maxConns,
	)
}

// config holds runtime knobs. Sensitive values (上游/admin 凭证) live in a YAML
// config file (config.yaml, .gitignore'd), never hardcoded. Path defaults to
// ./config.yaml and is overridable via CONFIG_FILE.
type config struct {
	addr string

	upstreamTimeout time.Duration
	requeryInterval time.Duration
	demoAppSecret   string
	demoSeed        bool // 是否在 postgres 启动时注入 demo license（默认 false；0004 迁移已从生产清除 demo，勿在生产开启）

	// admin console (DESIGN §16). 后台登录/JWT 走统一控制面 (首个路由)。
	adminUser      string
	adminPass      string
	adminJWTSecret string
	adminTokenTTL  time.Duration
	spaDir         string

	// 存储后端选择 (DESIGN §11): memory | postgres。
	storageDriver string
	migrationsDir string

	// 每版本独立配置 (hlt)。
	versions map[string]versionConfig
}

// duration parses Go-style duration strings (e.g. "4s", "5m", "8h") from YAML.
type duration time.Duration

func (d *duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = duration(parsed)
	return nil
}

// fileUpstream mirrors a version's upstream YAML block.
type fileUpstream struct {
	Kind    string `yaml:"kind"`
	BaseURL string `yaml:"baseURL"`
	// health (商保电子凭证智能服务平台) 专用
	AppID            string `yaml:"appId"`
	Key              string `yaml:"key"`
	APIVersion       string `yaml:"apiVersion"`
	ClaimCompanyCode string `yaml:"claimCompanyCode"`
	ClaimCompanyName string `yaml:"claimCompanyName"`
	BusType          string `yaml:"busType"`
	AuthFileURL      string `yaml:"authFileUrl"`
	AuthFileType     string `yaml:"authFileType"`
	AuthPath         string `yaml:"authPath"`
	AssessPath       string `yaml:"assessPath"`
	AssessTypes      string `yaml:"assessTypes"`
}

// fileDatabase mirrors a version's database YAML block.
type fileDatabase struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"sslmode"`
	MaxConns int    `yaml:"maxConns"`
}

// fileRedis mirrors a version's redis YAML block.
type fileRedis struct {
	Addr     string `yaml:"addr"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	PoolSize int    `yaml:"poolSize"`
}

// fileVersion mirrors one entry under versions: in config.yaml.
type fileVersion struct {
	Upstream fileUpstream `yaml:"upstream"`
	Database fileDatabase `yaml:"database"`
	Redis    fileRedis    `yaml:"redis"`
}

// fileConfig mirrors the YAML structure of config.yaml.
type fileConfig struct {
	Addr     string `yaml:"addr"`
	Upstream struct {
		Timeout duration `yaml:"timeout"`
	} `yaml:"upstream"`
	Billing struct {
		RequeryInterval duration `yaml:"requeryInterval"`
	} `yaml:"billing"`
	Admin struct {
		BootstrapUser string   `yaml:"bootstrapUser"`
		BootstrapPass string   `yaml:"bootstrapPass"`
		JWTSecret     string   `yaml:"jwtSecret"`
		TokenTTL      duration `yaml:"tokenTTL"`
		SPADir        string   `yaml:"spaDir"`
	} `yaml:"admin"`
	Demo struct {
		AppSecret string `yaml:"appSecret"`
		Seed      *bool  `yaml:"seed"` // 默认 false；开发/演示 postgres 可设 true（e2e 由建库脚本 SEED_DEMO=1 播种）
	} `yaml:"demo"`
	Storage struct {
		Driver        string `yaml:"driver"`
		MigrationsDir string `yaml:"migrationsDir"`
	} `yaml:"storage"`
	Versions map[string]fileVersion `yaml:"versions"`
}

// loadConfig reads the YAML config file and applies non-sensitive structural
// defaults. It fails fast if an explicitly requested file is missing/invalid.
func loadConfig() (config, error) {
	path := os.Getenv("CONFIG_FILE")
	explicit := path != ""
	if path == "" {
		path = "config.yaml"
	}

	var fc fileConfig
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(raw, &fc); err != nil {
			return config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	case explicit:
		return config{}, fmt.Errorf("read config %s: %w", path, err)
	default:
		fmt.Fprintf(os.Stderr, "warning: %s not found; using non-sensitive defaults, secrets empty\n", path)
	}

	cfg := config{
		addr:            def(fc.Addr, ":8080"),
		upstreamTimeout: durOr(fc.Upstream.Timeout, 4*time.Second),
		requeryInterval: durOr(fc.Billing.RequeryInterval, 10*time.Second),
		demoAppSecret:   def(fc.Demo.AppSecret, "demo-app-secret"),
		demoSeed:        demoSeedOr(fc.Demo.Seed, false),

		adminUser:      def(fc.Admin.BootstrapUser, "admin"),
		adminPass:      fc.Admin.BootstrapPass,
		adminJWTSecret: fc.Admin.JWTSecret,
		adminTokenTTL:  durOr(fc.Admin.TokenTTL, 8*time.Hour),
		spaDir:         def(fc.Admin.SPADir, "web/admin/dist"),

		storageDriver: def(fc.Storage.Driver, "memory"),
		migrationsDir: def(fc.Storage.MigrationsDir, "migrations"),

		versions: make(map[string]versionConfig, len(model.Versions)),
	}

	for _, v := range model.Versions {
		fv, ok := fc.Versions[v]
		if !ok {
			// version 未在配置中给出：memory 模式仍可启用 (无需 DB/上游凭证)。
			continue
		}
		cfg.versions[v] = versionConfig{
			upstream: upstreamConfig{
				kind:    def(fv.Upstream.Kind, defaultKind(v)),
				baseURL: fv.Upstream.BaseURL,
				appID:   fv.Upstream.AppID,
				key:     fv.Upstream.Key,
				// 其余空值由 health client 自行默认 (apiVersion/busType/路径等)
				apiVersion:       fv.Upstream.APIVersion,
				claimCompanyCode: fv.Upstream.ClaimCompanyCode,
				claimCompanyName: fv.Upstream.ClaimCompanyName,
				busType:          fv.Upstream.BusType,
				authFileURL:      fv.Upstream.AuthFileURL,
				authFileType:     fv.Upstream.AuthFileType,
				authPath:         fv.Upstream.AuthPath,
				assessPath:       fv.Upstream.AssessPath,
				assessTypes:      fv.Upstream.AssessTypes,
			},
			db: dbConfig{
				host:     fv.Database.Host,
				port:     intOr(fv.Database.Port, 5432),
				name:     fv.Database.Name,
				user:     fv.Database.User,
				password: fv.Database.Password,
				sslmode:  def(fv.Database.SSLMode, "disable"),
				maxConns: intOr(fv.Database.MaxConns, 10),
			},
			redis: redisConfig{
				addr:     fv.Redis.Addr,
				username: fv.Redis.Username,
				password: fv.Redis.Password,
				db:       fv.Redis.DB,
				poolSize: intOr(fv.Redis.PoolSize, 10),
			},
		}
	}
	return cfg, nil
}

// defaultKind picks the upstream client family by version: hlt→health.
func defaultKind(version string) string {
	_ = version
	return "health"
}

func def(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func demoSeedOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func intOr(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func durOr(d duration, fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}
