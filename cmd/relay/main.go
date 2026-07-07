// Command relay is the entrypoint for the个人健康评测转接服务. It wires the
// hexagonal layers together and starts the HTTP server + background workers.
//
// 各路由 (hlt) 对外接口完全一致，仅靠路由名区分；存储按「域」装配（每路由独立
// 一套 DB+Redis+license）。跨域使用 license 一律鉴权失败。
// Dev defaults use in-memory adapters; production swaps in Redis+Lua + 独立 PG。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/datahub/relay/internal/api"
	"github.com/datahub/relay/internal/application"
	"github.com/datahub/relay/internal/domain/admin"
	"github.com/datahub/relay/internal/domain/auth"
	"github.com/datahub/relay/internal/domain/billing"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/port"
	"github.com/datahub/relay/internal/domain/quota"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
	"github.com/datahub/relay/internal/infrastructure/persistence/postgres"
	redisq "github.com/datahub/relay/internal/infrastructure/persistence/redis"
	"github.com/datahub/relay/internal/infrastructure/secret"
	"github.com/datahub/relay/internal/infrastructure/upstream"
	"github.com/datahub/relay/internal/job"
)

// domainStorage is one license 域的存储后端 (独立 DB+Redis)。同一域内的多条路由
// 复用这一套 repos，共享 license 表，但统计/台账/审计按各自 route 独立
// (见 model.RouteDomain；本服务各路由独立成域)。
type domainStorage struct {
	licenseRepo port.LicenseRepository
	ledgerRepo  port.LedgerRepository
	quotaRepo   port.QuotaRepository
	auditRepo   port.AuditRepository
	adminRepo   port.AdminUserRepository
	userRepo    port.UserAdminRepository
	secrets     port.SecretProvider
	cleanup     func()
}

// routeStack is one fully-wired route (独立 orchestrator + 后台服务 + 复查 worker)，
// 接到其所属域的存储 + 自己的上游客户端。
type routeStack struct {
	orch    *application.QueryOrchestrator
	admin   *admin.Service
	requery *job.RequeryWorker
}

// domainOwner returns the route whose db/redis config seeds a domain's storage
// (域内第一个出现的路由)。本服务域名即路由名，owner 即路由自身。
func domainOwner(domain string) string {
	for _, r := range model.Versions {
		if model.RouteDomain(r) == domain {
			return r
		}
	}
	return domain
}

// checkStorageIsolation fails fast when两个不同的域被配置成共用同一个 PostgreSQL
// 库或同一个 Redis 逻辑库——那会破坏「各域独立 license/记录」的隔离承诺。
func checkStorageIsolation(cfg config) error {
	dbSeen := make(map[string]string)    // host:port/name -> domain
	redisSeen := make(map[string]string) // addr/db -> domain
	for _, domain := range model.Domains {
		vc, ok := cfg.versions[domainOwner(domain)]
		if !ok {
			continue
		}
		if vc.db.name != "" {
			key := fmt.Sprintf("%s:%d/%s", vc.db.host, vc.db.port, vc.db.name)
			if prev, dup := dbSeen[key]; dup {
				return fmt.Errorf("域 %s 与 %s 配置了同一个数据库 %s；每个域必须使用独立数据库", prev, domain, key)
			}
			dbSeen[key] = domain
		}
		if vc.redis.addr != "" {
			key := fmt.Sprintf("%s/%d", vc.redis.addr, vc.redis.db)
			if prev, dup := redisSeen[key]; dup {
				return fmt.Errorf("域 %s 与 %s 配置了同一个 Redis 逻辑库 %s；每个域必须使用独立 Redis db", prev, domain, key)
			}
			redisSeen[key] = domain
		}
	}
	return nil
}

func main() {
	level := slog.LevelInfo
	if lv := os.Getenv("LOG_LEVEL"); strings.EqualFold(lv, "debug") {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("load config failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{Timeout: cfg.upstreamTimeout}

	// --- 存储隔离防呆校验后，按域开库，再逐路由装配 ---
	if err := checkStorageIsolation(cfg); err != nil {
		logger.Error("storage isolation check failed", "err", err)
		os.Exit(1)
	}

	domainStores := make(map[string]*domainStorage, len(model.Domains))
	cleanups := make([]func(), 0, len(model.Domains))
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()
	for _, domain := range model.Domains {
		ds, err := buildDomainStorage(ctx, cfg, domain, logger)
		if err != nil {
			logger.Error("build domain storage failed", "domain", domain, "err", err)
			os.Exit(1)
		}
		domainStores[domain] = ds
		if ds.cleanup != nil {
			cleanups = append(cleanups, ds.cleanup)
		}
		logger.Info("domain storage ready", "domain", domain, "driver", cfg.storageDriver,
			"owner", domainOwner(domain))
	}

	apiStacks := make(map[string]*api.VersionStack, len(model.Versions))
	adminByRoute := make(map[string]*admin.Service, len(model.Versions))
	for _, route := range model.Versions {
		ds := domainStores[model.RouteDomain(route)]
		st, err := buildRouteStack(cfg, route, ds, httpClient, logger)
		if err != nil {
			logger.Error("build route stack failed", "route", route, "err", err)
			os.Exit(1)
		}
		apiStacks[route] = &api.VersionStack{Orch: st.orch, Admin: st.admin}
		adminByRoute[route] = st.admin
		go st.requery.Run(ctx)
		logger.Info("route stack ready", "route", route, "domain", model.RouteDomain(route),
			"upstream", cfg.versions[route].upstream.kind)
	}

	// 控制面：后台统一登录 + JWT 校验走首个路由 (model.Versions[0]) 的 admin 服务。
	control := adminByRoute[model.Versions[0]]
	if control == nil {
		logger.Error("control-plane stack not built; cannot start admin console", "route", model.Versions[0])
		os.Exit(1)
	}
	if err := control.BootstrapAdmin(ctx, cfg.adminUser, cfg.adminPass); err != nil {
		logger.Error("bootstrap admin failed", "err", err)
	} else {
		logger.Info("admin console ready", "loginUser", cfg.adminUser, "spaDir", cfg.spaDir)
	}

	// --- HTTP server ---
	server := api.NewServer(apiStacks, control, cfg.spaDir)
	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("relay listening", "addr", cfg.addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// buildDomainStorage opens the storage backend for one license 域 (DB+Redis or
// memory)，使用该域 owner 路由的 db/redis 配置。同一域只建一次，供域内各路由复用。
// 生产 (postgres) 不播种 demo license；memory (开发) 按域播种各自独立的 demo 凭证
// (model.DemoAppKey)。
func buildDomainStorage(ctx context.Context, cfg config, domain string, logger *slog.Logger) (*domainStorage, error) {
	owner := domainOwner(domain)
	vc := cfg.versions[owner]

	switch cfg.storageDriver {
	case "postgres":
		if vc.db.name == "" {
			return nil, fmt.Errorf("domain %s (owner %s): database.name 未配置", domain, owner)
		}
		pg, err := postgres.New(ctx, vc.db.dsn())
		if err != nil {
			return nil, fmt.Errorf("postgres connect: %w", err)
		}
		if err := postgres.ApplyMigrations(ctx, pg.Pool(), cfg.migrationsDir); err != nil {
			pg.Close()
			return nil, fmt.Errorf("apply migrations: %w", err)
		}
		if cfg.demoSeed {
			if err := postgres.SeedDemo(ctx, pg, owner); err != nil {
				pg.Close()
				return nil, fmt.Errorf("seed demo: %w", err)
			}
		}
		rq, err := redisq.New(ctx, redisq.Options{
			Addr:     vc.redis.addr,
			Username: vc.redis.username,
			Password: vc.redis.password,
			DB:       vc.redis.db,
			PoolSize: vc.redis.poolSize,
		}, pg)
		if err != nil {
			pg.Close()
			return nil, fmt.Errorf("redis connect: %w", err)
		}
		return &domainStorage{
			licenseRepo: pg, ledgerRepo: pg, quotaRepo: rq, auditRepo: pg,
			adminRepo: pg, userRepo: pg, secrets: secret.NewStore(pg),
			cleanup: func() { rq.Close(); pg.Close() },
		}, nil
	default:
		store := memory.New()
		seedDemo(store, domain, cfg.demoAppSecret)
		return &domainStorage{
			licenseRepo: store, ledgerRepo: store, quotaRepo: store, auditRepo: store,
			adminRepo: store, userRepo: store, secrets: secret.NewStore(store),
			cleanup: func() {},
		}, nil
	}
}

// buildRouteStack wires the per-route dependencies (auth/quota/billing/orchestrator/
// admin/requery) on top of the route's 域存储 + 自己的上游客户端。
func buildRouteStack(cfg config, route string, ds *domainStorage, httpClient *http.Client, logger *slog.Logger) (*routeStack, error) {
	vc := cfg.versions[route]
	log := logger.With("route", route)

	upRouter, err := buildUpstream(route, vc.upstream, httpClient, log)
	if err != nil {
		return nil, err
	}

	verifier := auth.Md5Verifier{}
	authSvc := auth.New(ds.licenseRepo, ds.secrets, verifier)
	quotaSvc := quota.New(ds.quotaRepo, ds.ledgerRepo)
	billSvc := billing.New(billing.DefaultTable())
	adminSvc := admin.New(route, ds.adminRepo, ds.userRepo, ds.auditRepo, admin.Config{
		JWTSecret: cfg.adminJWTSecret,
		TokenTTL:  cfg.adminTokenTTL,
	})
	orch := application.NewQueryOrchestrator(route, authSvc, quotaSvc, billSvc, upRouter, ds.auditRepo, log)
	requery := job.NewRequeryWorker(ds.ledgerRepo, ds.licenseRepo, upRouter, billSvc, quotaSvc, cfg.requeryInterval, log)

	return &routeStack{orch: orch, admin: adminSvc, requery: requery}, nil
}

// buildUpstream constructs the version's upstream client behind a 1-provider Router.
func buildUpstream(version string, uc upstreamConfig, httpClient *http.Client, logger *slog.Logger) (*upstream.Router, error) {
	_ = logger
	switch uc.kind {
	case upstream.ProviderHealth, "":
		client := upstream.NewHealth(upstream.HealthConfig{
			BaseURL:          uc.baseURL,
			AppID:            uc.appID,
			Key:              uc.key,
			APIVersion:       uc.apiVersion,
			ClaimCompanyCode: uc.claimCompanyCode,
			ClaimCompanyName: uc.claimCompanyName,
			BusType:          uc.busType,
			AuthFileURL:      uc.authFileURL,
			AuthFileType:     uc.authFileType,
			AuthPath:         uc.authPath,
			AssessPath:       uc.assessPath,
			AssessTypes:      uc.assessTypes,
		}, httpClient)
		return upstream.NewRouter(upstream.ProviderHealth, map[string]port.UpstreamPort{
			upstream.ProviderHealth: client,
		})
	default:
		return nil, fmt.Errorf("version %s: unknown upstream kind %q", version, uc.kind)
	}
}

// seedDemo registers the 域's dev demo license in a memory store so the
// e2e/admin flows have a known client per 域。demo appKey 按域各不相同
// (model.DemoAppKey)，保证 demo 凭证无法跨域使用。
func seedDemo(store *memory.Store, domain, demoSecret string) {
	up := strings.ToUpper(domain)
	store.SeedLicense(&model.LicenseView{
		LicenseID:  "LIC-DEMO-" + up,
		AppKey:     model.DemoAppKey(domainOwner(domain)),
		ClientUUID: "demo-client-" + domain,
		Status:     "ACTIVE",
	}, demoSecret, "Demo 商户("+up+")", "13800001234")
}
