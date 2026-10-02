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
	"strconv"
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
	"github.com/PhantomMatthew/nextcloud-go/internal/mail"
	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/netx"
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
	"github.com/PhantomMatthew/nextcloud-go/internal/wopi"

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

	hasher           auth.PasswordHasher
	authStore        *auth.SQLStore
	loginStore       login.Store
	sessions         session.Store
	keyResolver      *encrypt.SQLResolver
	secret           string
	instanceID       string
	memCache         *cache.Memory
	redisCache       *cache.Redis
	fileMeta         files.Store
	nameSweep        *files.NameSweep
	davFS            webdav.FS
	uploadsFS        webdav.FS
	trashFS          *files.Trash
	versionsFS       *files.Versions
	publicFS         *files.PublicDAV
	shares           *sharing.Service
	jobs             jobs.Runner
	jobsStore        *jobs.SQLStore
	calendarStore    *caldav.SQLStore
	calendarFS       *caldav.DAV
	contactsStore    *carddav.SQLStore
	contactsFS       *carddav.DAV
	notifStore       *notifications.SQLStore
	notifSubjects    notifications.SubjectDecryptor
	activityStore    *activity.SQLStore
	activitySubjects activity.SubjectDecryptor
	ocmStore         *ocm.SQLStore
	lookup           *sharing.LookupClient
	principalFS      *caldav.PrincipalDAV
	davRootFS        *caldav.RootDAV
	previewGen       *preview.Generator
	wopiSvc          *wopi.Service
	wopiDisc         *wopi.Discovery
	mailSvc          *mail.Service
	mailSyncer       *mail.Syncer
	staticUI         *web.StaticUI
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
	userStore := users.NewSQLStore(db)
	a.Users = userStore
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
	st, rawStorage, keyResolver, err := openStorage(cfg, db)
	if err != nil {
		mem.Close()
		if a.redisCache != nil {
			_ = a.redisCache.Close()
		}
		_ = db.Close()
		return nil, err
	}
	a.keyResolver = keyResolver
	// ADR-0102: revoking an app password also deletes its key wrap. The hook
	// is best-effort (a failure never fails the delete; reconcile's orphan
	// purge is the safety net), wired only when the resolver exists.
	if keyResolver != nil {
		a.authStore.TokenKeys = keyResolver
	}
	meta := files.NewSQLStore(db)
	a.fileMeta = meta
	bus := events.NewBus(logger)
	dav := files.NewDAV(st, meta, a.Users)
	dav.Events = bus
	dav.Props = files.NewSQLPropertyStore(db)
	dav.Locks = files.NewSQLLockStore(db)
	shareStore := sharing.NewSQLShareStore(db)
	dav.Shares = shareStore
	a.davFS = dav
	// Per-user key mode: one KeySharer feeds every ADR-0098 hook — the DAV
	// write path, share create/delete, the expire sweep, and group
	// membership changes — and the resolver itself is the ADR-0099 user
	// lifecycle hook (eager UK mint at creation, key-row purge at deletion).
	// Nil when the mode is off (hooks nil-checked). The bootstrap admin runs
	// before this block, so its UK is minted lazily on its first sealed
	// write or by `ncgo-cli encryption reconcile` (ADR-0099 known gap).
	var keySharer *files.KeySharer
	// nameTranslator, when non-nil (ADR-0104 filename encryption on), is the
	// one translation core every files-store consumer below is wrapped with.
	var nameTranslator *files.NameTranslator
	if keyResolver != nil {
		keySharer = &files.KeySharer{
			Meta:    meta,
			Wrapper: keyResolver,
			Shares:  shareStore,
			Users:   a.Users,
			Logger:  logger,
		}
		dav.KeySharer = keySharer
		dav.Logger = logger
		userStore.MemberKeys = keySharer
		userStore.UserKeys = keyResolver
		userStore.Logger = logger
		if cfg.Encryption.FilenameEncryption {
			// ADR-0104 phase 1: directory keys mint at folder creation
			// through the same resolver (validation guarantees per_user_keys
			// + enabled when the flag is set).
			dav.DirKeys = keyResolver
			// ADR-0104 phase 2: one translating decorator around the raw
			// filecache, reused by the satellite path-keyed stores. Every
			// consumer keeps speaking plaintext paths; only DB rows carry
			// ciphertext. Flag off leaves the raw stores — bit-identical.
			nameTranslator = files.NewNameTranslator(meta, keyResolver, userStore)
			tMeta := files.NewTranslatingStore(meta, nameTranslator)
			a.fileMeta = tMeta
			dav.Meta = tMeta
			// Phase 3a: the KeySharer reads the RAW store — share rows now
			// carry ciphertext paths, so the translating wrapper would
			// double-encrypt on the way in. Covering's prefix matching works
			// on ciphertext strings by construction (tokens joined by "/").
			keySharer.Meta = meta
			dav.Names = nameTranslator
			dav.RawMeta = meta
			dav.Locks = files.NewTranslatingLockStore(dav.Locks, nameTranslator)
			// The write switch is users.name_scheme: users created while the
			// mode is on start at scheme 1. Server wiring only — the
			// CLI/importer does not set it; imported trees stay scheme 0
			// until the phase-4 encrypt-names sweep.
			userStore.UserKeys = nameSchemeUserKeys{UserKeysHook: keyResolver, users: userStore}
			// ADR-0104 phase 4: the per-user conversion sweep. The CLI
			// encrypt-names is the bulk tool; this server's sweep closes the
			// two gaps the CLI cannot reach — enrolled users (their key boxes
			// open only in an unlocked-session ctx, so the login hook runs
			// the sweep at password login) and the bootstrap admin (created
			// before this wiring, master-wrapped, converts at first login).
			a.nameSweep = &files.NameSweep{
				DB:      db,
				Store:   meta,
				Keys:    keyResolver,
				Users:   userStore,
				Storage: st,
				Logger:  logger,
			}
		}
	}
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
	if keySharer != nil {
		a.shares.Keys = keySharer
	}
	a.ocmStore = ocm.NewSQLStore(db)
	a.lookup = &sharing.LookupClient{BaseURL: cfg.Sharing.LookupServer}
	dav.Incoming = files.MultiIncoming{a.shares, a.ocmStore}
	dav.Remote = a.shares.OCM
	// ADR-0023 provider side: /public.php/webdav serves OCM remote-share
	// tokens (Basic(token, "")) in addition to public-link tokens.
	a.publicFS = &files.PublicDAV{Files: dav, Resolve: a.shares.LookupValidPublicDAV}
	if nameTranslator != nil {
		// ADR-0104 phase 3a: the sharing service seals share rows
		// (ciphertext file_path + grant-time sealed metadata copies), the
		// public-link jail opens them, and DAV re-seals them after renames.
		a.shares.NameCodec = nameTranslator
		a.publicFS.NameCodec = nameTranslator
		dav.ShareResealer = a.shares
	}
	// ADR-0104 phase 3b: with filename encryption on, upload sessions carry
	// the tokenized destination (no plaintext window) through the same
	// translation core; flag off leaves the raw store — bit-identical.
	var uploadStore files.UploadStore = files.NewSQLUploadStore(db)
	if nameTranslator != nil {
		uploadStore = files.NewTranslatingUploadStore(uploadStore, nameTranslator)
	}
	a.uploadsFS = files.NewUploads(st, uploadStore, dav, a.Users)
	if nameTranslator != nil {
		// The notifications and activity renders decrypt ADR-0104 §9 subject
		// tokens in the viewer's ctx (nil seam = verbatim passthrough).
		a.notifSubjects = nameTranslator
		a.activitySubjects = nameTranslator
	}
	// Trash and versions carry ciphertext paths when filename encryption is
	// on (ADR-0104 §6) through thin wrappers on the same translation core.
	var trashStore files.TrashStore = files.NewSQLTrashStore(db)
	var versionStore files.VersionStore = files.NewSQLVersionStore(db)
	if nameTranslator != nil {
		trashStore = files.NewTranslatingTrashStore(trashStore, nameTranslator)
		versionStore = files.NewTranslatingVersionStore(versionStore, nameTranslator)
		// file_properties too: oc:favorite is the one persisted path-keyed
		// property (all other custom props compute live, ADR-0046).
		dav.Props = files.NewTranslatingPropsStore(dav.Props, nameTranslator)
	}
	tr := files.NewTrash(st, trashStore, dav, a.Users)
	if nameTranslator != nil {
		tr.LocationNamer = nameTranslator.TrashLocationBase
	}
	dav.Trash = tr
	a.trashFS = tr
	ver := files.NewVersions(st, versionStore, dav, a.Users)
	dav.Versions = ver
	a.versionsFS = ver
	jobsStore := jobs.NewSQLStore(db)
	a.jobsStore = jobsStore
	jr := jobs.NewRunner(jobsStore, time.Now, cfg.Jobs.Workers, cfg.Jobs.PollInterval)
	jr.Logger = logger
	var shareKeys sharing.ShareKeys
	if keySharer != nil {
		shareKeys = keySharer
	}
	if err := jr.Register(sharing.NewExpireJob(dav.Shares, dav.Clock, a.notifStore, logger, shareKeys)); err != nil {
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
		if cfg.Previews.OfficeEnabled {
			// ADR-0053's deferred scope: office documents rasterize through
			// the admin-configured headless converter (soffice) behind the
			// Rasterizer seam; off leaves the image whitelist bit-identical.
			a.previewGen.Rasterizer = &preview.ExecRasterizer{Command: cfg.Previews.OfficeCommand, Timeout: cfg.Previews.OfficeTimeout}
		}
		var gcRaw storage.Storage
		var gcEncPrefix string
		if keyResolver != nil {
			// ADR-0105: previews of v3-sealed sources self-seal under the
			// source file key (NCGOPV1) into the previews_enc prefix on the
			// RAW backend — the decorator never sees them — and GC sweeps
			// both prefixes. Per-user mode off leaves nil seams: the
			// decorated path stays bit-identical.
			a.previewGen.CacheRaw = rawStorage
			a.previewGen.SourceKeys = dav
			a.previewGen.Keys = keyResolver
			gcRaw = rawStorage
			gcEncPrefix = a.previewGen.EncPrefix()
		}
		// preview.gc is periodic, and Start seeds periodic jobs only for
		// names already registered — so this must precede jr.Start.
		if err := jr.Register(preview.NewGCJobSealed(st, a.previewGen.CachePrefix, gcRaw, gcEncPrefix, cfg.Previews.CacheMaxAge, time.Now, logger)); err != nil {
			if cerr := a.closeResources(ctx); cerr != nil {
				return nil, errors.Join(err, cerr)
			}
			return nil, err
		}
	}
	if cfg.Office.Enabled {
		// ADR-0106 (WOPI host core): the service shares the app's DAV (with
		// its TranslatingStore Meta — never the raw store) and the sharing
		// service for sharee resolution; the mint route resolves in the
		// caller's session ctx, the Collabora callbacks in the anonymous one.
		wopiStore := wopi.NewSQLStore(db)
		a.wopiSvc = &wopi.Service{
			Store:  wopiStore,
			Files:  dav,
			Users:  a.Users,
			Shares: a.shares,
			TTL:    cfg.Office.TokenTTL,
			Clock:  time.Now,
		}
		// ADR-0107 (token-bound key wraps): widen only a non-nil resolver —
		// a typed nil *SQLResolver would become a non-nil interface and key
		// handling would dispatch to a nil receiver (routes.go's idiom).
		// Nil keeps mint keyless and the GC sweep store-only.
		var wopiGCKeys wopi.TokenKeyCleaner
		if keyResolver != nil {
			a.wopiSvc.Keys = keyResolver
			wopiGCKeys = keyResolver
		}
		a.wopiDisc = &wopi.Discovery{BaseURL: cfg.Office.CollaboraURL, Clock: time.Now}
		// Same ordering rule as preview.gc: wopi.tokens.gc is periodic, and
		// Start seeds periodic jobs only for names already registered.
		if err := jr.Register(wopi.NewGCJob(wopiStore, time.Now, wopiGCKeys, logger)); err != nil {
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
	// ADR-0104 §9's first production writer: the DAV verbs emit the
	// file-lifecycle stream into the owner's activities rows (best-effort,
	// scheme-1 names tokenized by the producer). Unconditional — nil would
	// only ever come from a miswired store.
	dav.Activity = a.activityStore
	a.principalFS = &caldav.PrincipalDAV{Users: a.Users}
	a.davRootFS = &caldav.RootDAV{Users: a.Users}

	a.secret = cfg.Instance.Secret
	if a.secret == "" {
		a.secret = randomHex(logger, 32, "NCGO_SECRET / instance.secret")
	}
	if cfg.Mail.Enabled {
		// ADR-0108 (Mail): the service seals account credentials under a
		// key derived from the instance secret — deliberately independent of
		// the per-user-keys encryption module (keyResolver), so the M3+
		// background sync opens credentials without any user's unlocked key.
		// The wiring must follow the secret resolution above. M2's
		// verify-on-create dial goes through the ADR-0057 egress guard with
		// the mail.egress_allow_private CIDR allowlist (empty = fail closed
		// for private targets); the config was validated at Load. M3's
		// mail.sync registration shares the ordering rule of preview.gc and
		// wopi.tokens.gc: Start seeds periodic jobs only for names already
		// registered, so all of this must precede jr.Start.
		allowPrivate, err := cfg.Mail.EgressPrefixes()
		if err != nil {
			if cerr := a.closeResources(ctx); cerr != nil {
				return nil, errors.Join(err, cerr)
			}
			return nil, err
		}
		dialContext := netx.GuardedDialContext(allowPrivate)
		dialIMAP := func(ctx context.Context, opts imap.DialOptions) (*imap.Client, error) {
			opts.DialContext = dialContext
			return imap.Dial(ctx, opts)
		}
		mailStore := mail.NewSQLStore(db)
		a.mailSvc = &mail.Service{
			Store:    mailStore,
			Secret:   a.secret,
			Logger:   logger,
			DialIMAP: dialIMAP,
		}
		a.mailSyncer = &mail.Syncer{
			Store:    mailStore,
			Secret:   a.secret,
			DialIMAP: dialIMAP,
			Logger:   logger,
			Bus:      bus,
		}
		if err := jr.Register(mail.NewSyncJob(a.mailSyncer, mailStore, logger)); err != nil {
			if cerr := a.closeResources(ctx); cerr != nil {
				return nil, errors.Join(err, cerr)
			}
			return nil, err
		}
		// New-mail arrival → a bell notification for the owning user
		// (best-effort, like the sharing bells: insert failures are logged,
		// never fatal to the sync that published the event).
		bus.Subscribe(func(ctx context.Context, ev events.Event) {
			if ev.Topic != mail.EventMessageArrived {
				return
			}
			arrival, err := mail.DecodeArrival(ev.Payload)
			if err != nil {
				logger.WarnContext(ctx, "mail: arrival notification: decode failed", slog.Any("error", err))
				return
			}
			u, err := a.Users.GetByUID(ctx, arrival.UserID)
			if err != nil {
				logger.WarnContext(ctx, "mail: arrival notification: user lookup failed", slog.String("uid", arrival.UserID), slog.Any("error", err))
				return
			}
			n := &notifications.Notification{
				UserID:       u.ID,
				App:          "mail",
				UserUID:      u.UID,
				ObjectType:   "mail_account",
				ObjectID:     strconv.FormatInt(arrival.AccountID, 10),
				Subject:      mailArrivalSubject(arrival),
				ShouldNotify: true,
			}
			if err := a.notifStore.Insert(ctx, n); err != nil {
				logger.WarnContext(ctx, "mail: arrival notification insert failed", slog.String("uid", u.UID), slog.Any("error", err))
			}
		})
	}

	if err := jr.Start(ctx); err != nil {
		if cerr := a.closeResources(ctx); cerr != nil {
			return nil, errors.Join(err, cerr)
		}
		return nil, err
	}
	a.jobs = jr
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

// mailArrivalSubject renders the bell text for one mail.message.arrived
// event (count + latest sender, the fields the payload carries).
func mailArrivalSubject(arrival mail.Arrival) string {
	from := arrival.LatestFrom
	if from == "" {
		from = "unknown sender"
	}
	if arrival.Count == 1 {
		return fmt.Sprintf("1 new message from %s", from)
	}
	return fmt.Sprintf("%d new messages (latest from %s)", arrival.Count, from)
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

// nameSchemeUserKeys wraps the ADR-0099 user-key lifecycle hook with the
// ADR-0104 write switch: a user created while filename encryption is on
// starts at users.name_scheme = 1, so their tree encrypts from the first
// write (their root DK still mints lazily at the first request). Wired by the
// server only — CLI/importer user creation keeps scheme 0 until the phase-4
// encrypt-names sweep. Both legs stay best-effort like the wrapped hook: a
// failure is Warn-logged by the users store, never fatal to Create.
type nameSchemeUserKeys struct {
	users.UserKeysHook
	users *users.SQLStore
}

func (h nameSchemeUserKeys) OnUserCreated(ctx context.Context, uid string) error {
	err := h.UserKeysHook.OnUserCreated(ctx, uid)
	flipErr := h.users.SetNameScheme(ctx, uid, encrypt.NameSchemeNCGOFN1)
	return errors.Join(err, flipErr)
}

// openStorage builds the configured storage backend, wrapping it with the
// encryption decorator when enabled. The returned resolver is non-nil
// exactly when encryption.per_user_keys is on (ADR-0097); the caller reuses
// it as the ADR-0098 KeyWrapper for the share key hooks. raw is the
// undecorated backend (the same handle as st when encryption is off), kept
// for the ADR-0105 previews_enc self-sealed cache, which the decorator must
// never see.
func openStorage(cfg *config.Config, db database.DB) (st, raw storage.Storage, resolver *encrypt.SQLResolver, err error) {
	name := cfg.Storage.DefaultBackend
	if name == "" {
		name = "local"
	}
	b, ok := cfg.Storage.Backends[name]
	if !ok {
		return nil, nil, nil, fmt.Errorf("app: storage backend %q not configured", name)
	}
	switch b.Type {
	case "localfs":
		st, err = localfs.New(b.Root)
	case "s3":
		st, err = s3store.New(b)
	default:
		return nil, nil, nil, fmt.Errorf("app: storage backend %q type %q unsupported", name, b.Type)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	raw = st
	if cfg.Encryption.Enabled {
		current, previous, err := encrypt.LoadKeyring(cfg.Encryption.MasterKeyPath, cfg.Encryption.PreviousKeyPaths)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("app: encryption: %w", err)
		}
		if cfg.Encryption.PerUserKeys {
			// Ring order: previous keys first, current last — positions
			// are the key IDs UK rows reference (ADR-0097).
			ring := make([][]byte, 0, len(previous)+1)
			ring = append(ring, previous...)
			ring = append(ring, current)
			resolver, err = encrypt.NewSQLResolver(db, ring)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("app: encryption: %w", err)
			}
			// ADR-0100: password-wrapped enrollment is driven from the
			// password-login path; the KDF follows the password-hash params.
			resolver.PasswordWrapped = cfg.Encryption.PasswordWrappedKeys
			resolver.KDF = encrypt.KeyDerivationParams{
				MemoryKB:    cfg.Auth.Argon2id.MemoryKB,
				Iterations:  cfg.Auth.Argon2id.Iterations,
				Parallelism: cfg.Auth.Argon2id.Parallelism,
			}
		}
		// Widen only a non-nil resolver: a typed nil *SQLResolver would
		// otherwise become a non-nil KeyResolver interface and the FS
		// would dispatch per-user allocation to a nil receiver.
		var keyResolver encrypt.KeyResolver
		if resolver != nil {
			keyResolver = resolver
		}
		st, err = encrypt.NewWithResolver(current, previous, st, keyResolver)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("app: encryption: %w", err)
		}
	}
	return st, raw, resolver, nil
}
