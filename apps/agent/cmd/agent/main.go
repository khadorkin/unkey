package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/unkeyed/unkey/apps/agent/pkg/analytics"
	analyticsMiddleware "github.com/unkeyed/unkey/apps/agent/pkg/analytics/middleware"
	"github.com/unkeyed/unkey/apps/agent/pkg/analytics/tinybird"
	"github.com/unkeyed/unkey/apps/agent/pkg/boot"
	"github.com/unkeyed/unkey/apps/agent/pkg/cache"
	cacheMiddleware "github.com/unkeyed/unkey/apps/agent/pkg/cache/middleware"
	"github.com/unkeyed/unkey/apps/agent/pkg/database"
	"github.com/unkeyed/unkey/apps/agent/pkg/env"
	"github.com/unkeyed/unkey/apps/agent/pkg/events"
	"github.com/unkeyed/unkey/apps/agent/pkg/logging"
	"github.com/unkeyed/unkey/apps/agent/pkg/ratelimit"

	"os"
	"os/signal"
	"syscall"

	"github.com/unkeyed/unkey/apps/agent/pkg/entities"
	"github.com/unkeyed/unkey/apps/agent/pkg/events/kafka"
	"github.com/unkeyed/unkey/apps/agent/pkg/server"
	"github.com/unkeyed/unkey/apps/agent/pkg/tracing"
	"github.com/unkeyed/unkey/apps/agent/pkg/version"
	"go.uber.org/zap"
)

type features struct {
	enableAxiom bool
	analytics   string
	eventBus    string
	prewarm     bool
}

var runtimeConfig features

func init() {
	AgentCmd.Flags().BoolVar(&runtimeConfig.enableAxiom, "enable-axiom", false, "Send logs and traces to axiom")
	AgentCmd.Flags().StringVar(&runtimeConfig.analytics, "analytics", "", "Send analytics to a backend, available: ['tinybird']")
	AgentCmd.Flags().StringVar(&runtimeConfig.eventBus, "event-bus", "", "Use a message bus for communication between nodes, available: ['kafka']")
	AgentCmd.Flags().BoolVar(&runtimeConfig.prewarm, "prewarm", false, "Load all keys from the db to memory on boot")
}

// AgentCmd represents the agent command
var AgentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the Unkey agent",
	Long:  ``,
	Run: func(cmd *cobra.Command, args []string) {

		logger := logging.New().With(
			zap.String("version", version.Version),
		)

		defer func() {
			// this is best effort and can error quite frequently
			_ = logger.Sync()
		}()
		logger.Info("Starting Unkey Agent", zap.Any("runtimeConfig", runtimeConfig))

		e := env.Env{
			ErrorHandler: func(err error) { logger.Fatal("unable to load environment variable", zap.Error(err)) },
		}
		region := e.String("FLY_REGION", "local")
		allocId := e.String("FLY_ALLOC_ID", "")
		logger = logger.With(zap.String("region", region))
		if allocId != "" {
			logger = logger.With(zap.String("allocId", allocId))
		}

		// Setup Axiom

		var tracer tracing.Tracer = tracing.NewNoop()
		{
			if runtimeConfig.enableAxiom {
				t, closeTracer, err := tracing.New(context.Background(), tracing.Config{
					Dataset:    "tracing",
					Service:    "agent",
					Version:    version.Version,
					AxiomOrgId: e.String("AXIOM_ORG_ID"),
					AxiomToken: e.String("AXIOM_TOKEN"),
				})
				if err != nil {
					logger.Fatal("unable to start tracer", zap.Error(err))
				}
				defer func() {
					err := closeTracer()
					if err != nil {
						logger.Fatal("unable to close tracer", zap.Error(err))
					}
				}()
				tracer = t
				logger.Info("Axiom tracing enabled")
			}
		}

		// Setup Analytics

		var a analytics.Analytics = analytics.NewNoop()
		{
			if runtimeConfig.analytics == "tinybird" {
				tb := tinybird.New(tinybird.Config{
					Token:  e.String("TINYBIRD_TOKEN"),
					Logger: logger,
				})
				defer tb.Close()
				a = tb
			}
			a = analyticsMiddleware.WithTracing(a, tracer)

		}

		// Setup Event Bus

		var eventBus events.EventBus = events.NewNoop()
		{
			if runtimeConfig.eventBus == "kafka" {
				k, err := kafka.New(kafka.Config{
					Logger:   logger,
					GroupId:  e.String("FLY_ALLOC_ID", "local"),
					Broker:   e.String("KAFKA_BROKER"),
					Username: e.String("KAFKA_USERNAME"),
					Password: e.String("KAFKA_PASSWORD"),
				})
				if err != nil {
					logger.Fatal("unable to start kafka", zap.Error(err))
				}

				k.Start()
				defer k.Close()
				eventBus = k
			}
		}
		var err error

		fastRatelimit := ratelimit.NewInMemory()
		var consistentRatelimit ratelimit.Ratelimiter
		redisUrl := e.String("REDIS_URL", "")
		if redisUrl != "" {
			consistentRatelimit, err = ratelimit.NewRedis(ratelimit.RedisConfig{
				RedisUrl: redisUrl,
			})
			if err != nil {
				logger.Fatal("unable to start redis ratelimiting", zap.Error(err))
			}
		}

		db, err := database.New(
			database.Config{
				Logger:      logger,
				PrimaryUs:   e.String("DATABASE_DSN"),
				ReplicaEu:   e.String("DATABASE_DSN_EU", ""),
				ReplicaAsia: e.String("DATABASE_DSN_ASIA", ""),
				FlyRegion:   region,
			},
			database.WithLogging(logger),
			database.WithTracing(tracer),
		)
		if err != nil {
			logger.Fatal("Failed to connect to database", zap.Error(err))
		}
		defer db.Close()

		keyCache := cache.New[entities.Key](cache.Config[entities.Key]{
			Fresh:   time.Minute * 15,
			Stale:   time.Minute * 60,
			MaxSize: 1024 * 1024,
			RefreshFromOrigin: func(ctx context.Context, keyHash string) (entities.Key, bool) {
				key, found, err := db.FindKeyByHash(ctx, keyHash)
				if err != nil {
					logger.Error("unable to refresh key by hash", zap.Error(err))
					return entities.Key{}, false
				}
				return key, found
			},
			Logger: logger.With(zap.String("cacheType", "key")),
		})
		keyCache = cacheMiddleware.WithTracing[entities.Key](keyCache, tracer)
		keyCache = cacheMiddleware.WithLogging[entities.Key](keyCache, logger.With(zap.String("cacheType", "key")))

		apiCache := cache.New[entities.Api](cache.Config[entities.Api]{
			Fresh:   time.Minute * 5,
			Stale:   time.Minute * 15,
			MaxSize: 1024 * 1024,
			RefreshFromOrigin: func(ctx context.Context, apiId string) (entities.Api, bool) {
				key, found, err := db.FindApi(ctx, apiId)
				if err != nil {
					logger.Error("unable to refresh api by id", zap.Error(err))
					return entities.Api{}, false
				}
				return key, found
			},
			Logger: logger.With(zap.String("cacheType", "api")),
		})
		apiCache = cacheMiddleware.WithTracing[entities.Api](apiCache, tracer)
		apiCache = cacheMiddleware.WithLogging[entities.Api](apiCache, logger.With(zap.String("cacheType", "api")))

		eventBus.OnKeyEvent(func(ctx context.Context, e events.KeyEvent) error {

			if e.Type == events.KeyDeleted {
				logger.Info("evicting from cache", zap.String("keyId", e.Key.Id), zap.String("keyHash", e.Key.Hash))
				keyCache.Remove(context.Background(), e.Key.Hash)
				return nil
			}

			logger.Info("precaching key", zap.String("keyId", e.Key.Id), zap.String("keyHash", e.Key.Hash))
			key, found, err := db.FindKeyById(ctx, e.Key.Id)
			if err != nil {
				return fmt.Errorf("unable to get key by id: %s: %w", e.Key.Id, err)
			}
			if !found {
				return nil
			}
			keyCache.Set(ctx, key.Hash, key)
			logger.Info("precaching api", zap.String("keyAuthId", key.KeyAuthId))
			api, found, err := db.FindApiByKeyAuthId(ctx, key.KeyAuthId)
			if err != nil {
				return fmt.Errorf("unable to find api by keyAuthId: %s: %w", key.KeyAuthId, err)
			}
			if !found {
				return nil
			}
			apiCache.Set(ctx, api.KeyAuthId, api)

			return nil
		})

		if runtimeConfig.prewarm {

			cacheWarmer := boot.NewCacheWarmer(boot.Config{
				KeyCache: keyCache,
				ApiCache: apiCache,
				DB:       db,
				Logger:   logger,
			})

			go func() {
				err := cacheWarmer.Run(context.Background())
				if err != nil {
					logger.Error("unable to warm cache", zap.Error(err))
				}
			}()
		}

		srv := server.New(server.Config{
			Logger:            logger,
			KeyCache:          keyCache,
			ApiCache:          apiCache,
			Database:          db,
			Ratelimit:         fastRatelimit,
			GlobalRatelimit:   consistentRatelimit,
			Tracer:            tracer,
			Analytics:         a,
			UnkeyAppAuthToken: e.String("UNKEY_APP_AUTH_TOKEN"),
			UnkeyWorkspaceId:  e.String("UNKEY_WORKSPACE_ID"),
			UnkeyApiId:        e.String("UNKEY_API_ID"),
			UnkeyKeyAuthId:    e.String("UNKEY_KEY_AUTH_ID"),
			Region:            region,
			EventBus:          eventBus,
			Version:           version.Version,
		})

		go func() {
			err = srv.Start(fmt.Sprintf("0.0.0.0:%s", e.String("PORT", "8080")))
			if err != nil {
				logger.Fatal("Failed to run service", zap.Error(err))
			}
		}()
		defer srv.Close()

		cShutdown := make(chan os.Signal, 1)
		signal.Notify(cShutdown, os.Interrupt, syscall.SIGTERM)

		// wait for signal
		sig := <-cShutdown
		logger.Info("Caught signal, shutting down", zap.Any("sig", sig))
	},
}
