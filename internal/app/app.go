// Package app wires configuration, storage, and HTTP routes into a process.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/activity"
	"github.com/PhantomMatthew/nextcloud-go/internal/appconfig"
	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	caldav "github.com/PhantomMatthew/nextcloud-go/internal/calendar"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	carddav "github.com/PhantomMatthew/nextcloud-go/internal/contacts"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/login"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/notifications"
	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocm"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins"
	"github.com/PhantomMatthew/nextcloud-go/internal/preview"
	"github.com/PhantomMatthew/nextcloud-go/internal/session"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	s3store "github.com/PhantomMatthew/nextcloud-go/internal/storage/s3"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/version"
	"github.com/PhantomMatthew/nextcloud-go/internal/web"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// App is the wired server process.
type App struct {
	Cfg        *config.Config
	Logger     *slog.Logger
	DB         database.DB
	Cache      cache.Cache
	Users      users.Store
	Router     *httpx.Router
	PluginHost *plugins.Host

	pluginReg  *plugins.Registry
	reconciler *plugins.Reconciler
	recCancel  context.CancelFunc
	metrics    *observability.Registry
	tracing    *sdktrace.TracerProvider

	hasher        auth.PasswordHasher
	authStore     auth.Store
	loginStore    login.Store
	sessions      session.Store
	secret        string
	instanceID    string
	memCache      *cache.Memory
	redisCache    *cache.Redis
	fileMeta      files.Store
	davFS         webdav.FS
	uploadsFS     webdav.FS
	trashFS       *files.Trash
	versionsFS    *files.Versions
	publicFS      webdav.FS
	shares        *sharing.Service
	jobs          jobs.Runner
	jobsStore     *jobs.SQLStore
	calendarStore *caldav.SQLStore
	calendarFS    *caldav.DAV
	contactsStore *carddav.SQLStore
	contactsFS    *carddav.DAV
	notifStore    *notifications.SQLStore
	activityStore *activity.SQLStore
	ocmStore      *ocm.SQLStore
	lookup        *sharing.LookupClient
	principalFS   *caldav.PrincipalDAV
	davRootFS     *caldav.RootDAV
	previewGen    *preview.Generator
	staticUI      *web.StaticUI
}

// New opens dependencies and mounts routes.
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*App, error) {
	if cfg == nil {
		return nil, fmt.Errorf("app: nil config")
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &App{Cfg: cfg, Logger: logger}
	db, err := database.Open(ctx, database.Config{
		Driver:          database.Dialect(cfg.Database.Driver),
		DSN:             cfg.Database.DSN,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
	})
	if err != nil {
		return nil, err
	}
	a.DB = db
	std, ok := database.Unwrap(db)
	if !ok {
		_ = db.Close()
		return nil, fmt.Errorf("app: unwrap db")
	}
	if cfg.Database.AutoMigrate {
		if _, err := migrations.Up(ctx, std, db.Dialect(), logger); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	// The instance id feeds the tracing resource below (and the storage
	// prefixes further down), so it must exist before any store is built.
	a.instanceID = cfg.Instance.ID
	if a.instanceID == "" {
		a.instanceID = "oc" + randomHex(logger, 5, "NCGO_INSTANCE_ID / instance.id")
	}
	// An empty endpoint means no provider at all: the global default stays
	// the no-op provider and neither the middleware nor the plugin wrapper is
	// installed (ADR-0072), so disabled tracing is exactly zero overhead.
	if cfg.Observability.OTelEndpoint != "" {
		tp, err := observability.NewTracerProvider(ctx, observability.TracingConfig{
			Endpoint:       cfg.Observability.OTelEndpoint,
			SampleRatio:    cfg.Observability.OTelSampleRatio,
			ServiceVersion: version.String(),
			InstanceID:     a.instanceID,
		})
		if err != nil {
			if cerr := a.closeResources(ctx); cerr != nil {
				return nil, errors.Join(err, cerr)
			}
			return nil, err
		}
		a.tracing = tp
	}
	// One wrap at the source traces every store built below and the plugin
	// host's DB access alike (ADR-0075); with no provider the DB is untouched.
	if a.tracing != nil {
		db = database.WithTracing(db, a.tracing)
		a.DB = db
	}
	a.hasher = auth.NewArgon2id(auth.Argon2idParams{
		MemoryKB:    cfg.Auth.Argon2id.MemoryKB,
		Iterations:  cfg.Auth.Argon2id.Iterations,
		Parallelism: cfg.Auth.Argon2id.Parallelism,
	})
	a.Users = users.NewSQLStore(db)
	a.authStore = auth.NewSQLStore(db)
	a.loginStore = login.NewSQLStore(db)
	a.sessions = session.NewSQLStore(db)
	if err := users.EnsureBootstrapAdmin(ctx, a.Users, a.hasher, users.BootstrapAdmin{
		UID:         cfg.Auth.BootstrapAdmin.UID,
		Password:    cfg.Auth.BootstrapAdmin.Password,
		DisplayName: cfg.Auth.BootstrapAdmin.DisplayName,
	}, logger); err != nil {
		_ = db.Close()
		return nil, err
	}
	mem, err := cache.NewMemory(cache.MemoryConfig{
		MaxItems:     cfg.Cache.L1MaxItems,
		MaxCostBytes: int64(cfg.Cache.L1MaxCostMB) * 1024 * 1024,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	a.memCache = mem
	if cfg.Cache.RedisAddr != "" {
		r, err := cache.NewRedis(cache.RedisConfig{
			Addr:     cfg.Cache.RedisAddr,
			Password: cfg.Cache.RedisPassword,
			DB:       cfg.Cache.RedisDB,
		})
		if err != nil {
			mem.Close()
			_ = db.Close()
			return nil, err
		}
		a.redisCache = r
		a.Cache = cache.NewTiered(mem, r)
	} else {
		a.Cache = mem
	}
	st, err := openStorage(cfg)
	if err != nil {
		mem.Close()
		if a.redisCache != nil {
			_ = a.redisCache.Close()
		}
		_ = db.Close()
		return nil, err
	}
	meta := files.NewSQLStore(db)
	a.fileMeta = meta
	bus := events.NewBus(logger)
	dav := files.NewDAV(st, meta, a.Users)
	dav.Events = bus
	dav.Props = files.NewSQLPropertyStore(db)
	dav.Locks = files.NewSQLLockStore(db)
	dav.Shares = sharing.NewSQLShareStore(db)
	a.davFS = dav
	a.notifStore = notifications.NewSQLStore(db)
	a.shares = &sharing.Service{
		Store:  dav.Shares,
		Files:  dav,
		Users:  a.Users,
		Hasher: a.hasher,
		OCM:    ocm.NewClient(),
		Notifs: a.notifStore,
		Logger: logger,
	}
	a.ocmStore = ocm.NewSQLStore(db)
	a.lookup = &sharing.LookupClient{BaseURL: cfg.Sharing.LookupServer}
	dav.Incoming = files.MultiIncoming{a.shares, a.ocmStore}
	dav.Remote = a.shares.OCM
	a.publicFS = &files.PublicDAV{Files: dav, Resolve: a.shares.LookupValid}
	a.uploadsFS = files.NewUploads(st, files.NewSQLUploadStore(db), dav, a.Users)
	tr := files.NewTrash(st, files.NewSQLTrashStore(db), dav, a.Users)
	dav.Trash = tr
	a.trashFS = tr
	ver := files.NewVersions(st, files.NewSQLVersionStore(db), dav, a.Users)
	dav.Versions = ver
	a.versionsFS = ver
	jobsStore := jobs.NewSQLStore(db)
	a.jobsStore = jobsStore
	jr := jobs.NewRunner(jobsStore, time.Now, cfg.Jobs.Workers, cfg.Jobs.PollInterval)
	jr.Logger = logger
	if err := jr.Register(sharing.NewExpireJob(dav.Shares, dav.Clock, a.notifStore, logger)); err != nil {
		if cerr := a.closeResources(ctx); cerr != nil {
			return nil, errors.Join(err, cerr)
		}
		return nil, err
	}
	if err := jr.Register(files.NewExpireLocksJob(dav.Locks, dav.Clock)); err != nil {
		if cerr := a.closeResources(ctx); cerr != nil {
			return nil, errors.Join(err, cerr)
		}
		return nil, err
	}
	if cfg.Previews.Enabled {
		a.previewGen = preview.NewGenerator(dav, st, "appdata_"+a.instanceID+"/previews", cfg.Previews.MaxDimension, logger)
		// preview.gc is periodic, and Start seeds periodic jobs only for
		// names already registered — so this must precede jr.Start.
		if err := jr.Register(preview.NewGCJob(st, a.previewGen.CachePrefix, cfg.Previews.CacheMaxAge, time.Now, logger)); err != nil {
			if cerr := a.closeResources(ctx); cerr != nil {
				return nil, errors.Join(err, cerr)
			}
			return nil, err
		}
	}
	calStore := caldav.NewSQLStore(db)
	a.calendarStore = calStore
	a.calendarFS = &caldav.DAV{Store: calStore, Users: a.Users}
	cardStore := carddav.NewSQLStore(db)
	a.contactsStore = cardStore
	a.contactsFS = &carddav.DAV{Store: cardStore, Users: a.Users}
	a.activityStore = activity.NewSQLStore(db)
	a.principalFS = &caldav.PrincipalDAV{Users: a.Users}
	a.davRootFS = &caldav.RootDAV{Users: a.Users}

	if err := jr.Start(ctx); err != nil {
		if cerr := a.closeResources(ctx); cerr != nil {
			return nil, errors.Join(err, cerr)
		}
		return nil, err
	}
	a.jobs = jr
	a.secret = cfg.Instance.Secret
	if a.secret == "" {
		a.secret = randomHex(logger, 32, "NCGO_SECRET / instance.secret")
	}
	// Event-driven (not periodic): one jobs row per upload, with the
	// files.uploaded msgpack payload forwarded verbatim (ADR-0084).
	if a.previewGen != nil && cfg.Previews.PregenerateEnabled {
		if err := jr.Register(preview.NewPregenerateJob(a.previewGen, cfg.Previews.PregenerateSizes, logger)); err != nil {
			if cerr := a.closeResources(ctx); cerr != nil {
				return nil, errors.Join(err, cerr)
			}
			return nil, err
		}
		bus.Subscribe(func(ctx context.Context, ev events.Event) {
			if ev.Topic != files.EventFilesUploaded {
				return
			}
			if err := jr.Enqueue(ctx, jobs.JobPreviewPregenerate, ev.Payload, time.Now()); err != nil {
				logger.WarnContext(ctx, "preview pregeneration enqueue failed", slog.Any("error", err))
			}
		})
	}
	if cfg.Web.StaticRoot != "" {
		ui, err := web.NewStaticUI(cfg.Web.StaticRoot, logger)
		if err != nil {
			if cerr := a.closeResources(ctx); cerr != nil {
				return nil, errors.Join(err, cerr)
			}
			return nil, err
		}
		a.staticUI = ui
	}
	if cfg.Observability.MetricsEnabled {
		a.metrics = observability.NewRegistry()
	}
	if cfg.Plugin.Enabled {
		reg := plugins.NewRegistry(a.DB)
		a.pluginReg = reg
		ph, err := plugins.NewHost(ctx, plugins.HostConfig{
			DefaultMemoryLimitMB:     cfg.Plugin.DefaultMemoryLimitMB,
			DefaultCallTimeout:       time.Duration(cfg.Plugin.DefaultCPUTimeoutMS) * time.Millisecond,
			HTTPRatePerMinute:        cfg.Plugin.HTTPRatePerMinute,
			MaxHTTPResponseBytes:     int64(cfg.Plugin.MaxHTTPResponseMB) << 20,
			DBMaxConcurrentPerPlugin: cfg.Plugin.DBMaxConcurrentPerPlugin,
			PluginSystemQuotaBytes:   int64(cfg.Plugin.SystemStorageQuotaMB) << 20,
			Cache:                    a.Cache,
			DB:                       a.DB,
			Bus:                      bus,
			Registry:                 reg,
			Files:                    dav,
			SystemStorage:            st,
			SystemPrefix:             "appdata_" + a.instanceID + "/plugins",
			AppConfig:                appconfig.NewStore(a.DB),
			Jobs:                     jr,
			Metrics:                  a.metrics,
			TracerProvider:           a.tracing,
		}, logger)
		if err != nil {
			if cerr := a.closeResources(ctx); cerr != nil {
				return nil, errors.Join(err, cerr)
			}
			return nil, err
		}
		a.PluginHost = ph
		dav.LiveProps = plugins.NewPropProvider(ph, reg, logger)
	}
	if err := a.mountRoutes(); err != nil {
		if cerr := a.closeResources(ctx); cerr != nil {
			return nil, errors.Join(err, cerr)
		}
		return nil, err
	}
	// The refresh loop runs only when refresh_interval > 0; the boot-time
	// Sync mountRoutes already performed keeps the 0 semantics identical to
	// the pre-hot-reload startup mount (ADR-0062).
	if a.reconciler != nil && cfg.Plugin.RefreshInterval > 0 {
		var recCtx context.Context
		recCtx, a.recCancel = context.WithCancel(context.Background())
		go a.reconciler.Run(recCtx, cfg.Plugin.RefreshInterval)
	}
	return a, nil
}

func randomHex(logger *slog.Logger, n int, what string) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		logger.Error("failed to generate secret material", slog.String("what", what), slog.Any("error", err))
		return hex.EncodeToString(make([]byte, n))
	}
	logger.Warn("generated ephemeral secret material", slog.String("what", what))
	return hex.EncodeToString(buf)
}

// UseHTTPClient replaces the outbound OCM and lookup HTTP clients (tests).
func (a *App) UseHTTPClient(c *http.Client) {
	if a == nil || a.shares == nil {
		return
	}
	if a.shares.OCM == nil {
		a.shares.OCM = &ocm.Client{}
	}
	a.shares.OCM.HTTP = c
	if a.lookup != nil {
		a.lookup.HTTP = c
	}
}

// Handler returns the HTTP handler.
func (a *App) Handler() http.Handler {
	return a.Router
}

// Run serves HTTP until ctx is done. When observability.metrics_listen is
// set, /metrics is served on a dedicated listener (ADR-0076) running in a
// goroutine alongside the main server: a metrics-server failure (e.g. a bind
// error) cancels the shared context so the main server shuts down gracefully
// and the metrics error is returned; on normal shutdown the metrics result is
// collected and errors.Join'd with the main one.
func (a *App) Run(ctx context.Context) error {
	srv := httpx.NewServer(httpx.ServerConfig{
		Addr:    a.Cfg.Server.Listen,
		Handler: a.Router,
		Logger:  a.Logger,
	})
	msrv := a.metricsServer()
	if msrv == nil {
		return srv.Run(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	metricsErrCh := make(chan error, 1)
	go func() {
		err := msrv.Run(ctx)
		if err != nil {
			// The dedicated listener died: take the main server down too.
			cancel()
		}
		metricsErrCh <- err
	}()
	mainErr := srv.Run(ctx)
	cancel()
	return errors.Join(mainErr, <-metricsErrCh)
}

// metricsHandler is the dedicated listener's entire surface (ADR-0076): only
// GET /metrics, behind the same bearer token as the main-listener mount. The
// Go 1.22+ pattern makes other methods 405 and other paths 404 on its own.
func (a *App) metricsHandler() http.Handler {
	mux := http.NewServeMux()
	if a.metrics != nil {
		mux.Handle("GET /metrics", a.metrics.Handler(a.Cfg.Observability.MetricsToken))
	}
	return mux
}

// metricsServer builds the dedicated metrics listener, or nil when metrics
// are disabled or observability.metrics_listen is empty (today's behavior:
// /metrics on the main router). ServerConfig zero-value timeouts are fine —
// the handler is a cheap in-memory render.
func (a *App) metricsServer() *httpx.Server {
	if a.metrics == nil || a.Cfg.Observability.MetricsListen == "" {
		return nil
	}
	return httpx.NewServer(httpx.ServerConfig{
		Addr:    a.Cfg.Observability.MetricsListen,
		Handler: a.metricsHandler(),
		Logger:  a.Logger,
	})
}

// Close releases dependencies in reverse order.
func (a *App) Close(ctx context.Context) error {
	return a.closeResources(ctx)
}

func (a *App) closeResources(ctx context.Context) error {
	var err error
	// Stop the refresh loop first so no Sync can start a plugin while
	// teardown is closing the others.
	if a.recCancel != nil {
		a.recCancel()
		a.recCancel = nil
	}
	if a.jobs != nil {
		err = joinErr(err, a.jobs.Stop(ctx))
		a.jobs = nil
	}
	if a.reconciler != nil {
		err = joinErr(err, a.reconciler.Close(ctx))
		a.reconciler = nil
	}
	if a.PluginHost != nil {
		err = joinErr(err, a.PluginHost.Close(ctx))
		a.PluginHost = nil
	}
	if a.tracing != nil {
		// Flush in-flight spans with a bounded budget; ctx may already be
		// done during teardown.
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = joinErr(err, a.tracing.Shutdown(shutCtx))
		cancel()
		a.tracing = nil
	}
	if a.redisCache != nil {
		err = joinErr(err, a.redisCache.Close())
		a.redisCache = nil
	}
	if a.memCache != nil {
		a.memCache.Close()
		a.memCache = nil
	}
	if a.DB != nil {
		err = joinErr(err, a.DB.Close())
		a.DB = nil
	}
	return err
}

func joinErr(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return errors.Join(a, b)
}

func openStorage(cfg *config.Config) (storage.Storage, error) {
	name := cfg.Storage.DefaultBackend
	if name == "" {
		name = "local"
	}
	b, ok := cfg.Storage.Backends[name]
	if !ok {
		return nil, fmt.Errorf("app: storage backend %q not configured", name)
	}
	var st storage.Storage
	var err error
	switch b.Type {
	case "localfs":
		st, err = localfs.New(b.Root)
	case "s3":
		st, err = s3store.New(b)
	default:
		return nil, fmt.Errorf("app: storage backend %q type %q unsupported", name, b.Type)
	}
	if err != nil {
		return nil, err
	}
	if cfg.Encryption.Enabled {
		current, previous, err := encrypt.LoadKeyring(cfg.Encryption.MasterKeyPath, cfg.Encryption.PreviousKeyPaths)
		if err != nil {
			return nil, fmt.Errorf("app: encryption: %w", err)
		}
		st, err = encrypt.NewWithPrevious(current, previous, st)
		if err != nil {
			return nil, fmt.Errorf("app: encryption: %w", err)
		}
	}
	return st, nil
}
