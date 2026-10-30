// Package web provides the main web server implementation for the 3x-ui panel,
// including HTTP/HTTPS serving, routing, templates, and background job scheduling.
package web

import (
	"context"
	"crypto/tls"
	"embed"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/util/common"
	"github.com/mhsanaei/3x-ui/v2/web/controller"
	"github.com/mhsanaei/3x-ui/v2/web/job"
	"github.com/mhsanaei/3x-ui/v2/web/locale"
	"github.com/mhsanaei/3x-ui/v2/web/middleware"
	"github.com/mhsanaei/3x-ui/v2/web/network"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"
	"github.com/mhsanaei/3x-ui/v2/web/websocket"

	"github.com/gin-contrib/gzip"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
)

//go:embed assets
var assetsFS embed.FS

//go:embed html/*
var htmlFS embed.FS

//go:embed translation/*
var i18nFS embed.FS

var startTime = time.Now()

type wrapAssetsFS struct {
	embed.FS
}

func (f *wrapAssetsFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open("assets/" + name)
	if err != nil {
		return nil, err
	}
	return &wrapAssetsFile{
		File: file,
	}, nil
}

type wrapAssetsFile struct {
	fs.File
}

func (f *wrapAssetsFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return &wrapAssetsFileInfo{
		FileInfo: info,
	}, nil
}

type wrapAssetsFileInfo struct {
	fs.FileInfo
}

func (f *wrapAssetsFileInfo) ModTime() time.Time {
	return startTime
}

func resellerPortalRoot(username, portalPath string) string {
	root := "/reseller/" + url.PathEscape(strings.TrimSpace(username)) + "/"
	portalPath = strings.Trim(strings.TrimSpace(portalPath), "/")
	if portalPath != "" {
		root += url.PathEscape(portalPath) + "/"
	}
	return root
}

// EmbeddedHTML returns the embedded HTML templates filesystem for reuse by other servers.
func EmbeddedHTML() embed.FS {
	return htmlFS
}

// EmbeddedAssets returns the embedded assets filesystem for reuse by other servers.
func EmbeddedAssets() embed.FS {
	return assetsFS
}

// Server represents the main web server for the 3x-ui panel with controllers, services, and scheduled jobs.
type Server struct {
	httpServer *http.Server
	listener   net.Listener

	index *controller.IndexController
	panel *controller.XUIController
	api   *controller.APIController
	ws    *controller.WebSocketController

	xrayService      service.XrayService
	settingService   service.SettingService
	tgbotService     service.Tgbot
	customGeoService *service.CustomGeoService

	wsHub *websocket.Hub

	cron *cron.Cron

	ctx    context.Context
	cancel context.CancelFunc
}

// NewServer creates a new web server instance with a cancellable context.
func NewServer() *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		ctx:    ctx,
		cancel: cancel,
	}
}

// getHtmlFiles walks the local `web/html` directory and returns a list of
// template file paths. Used only in debug/development mode.
func (s *Server) getHtmlFiles() ([]string, error) {
	files := make([]string, 0)
	dir, _ := os.Getwd()
	err := fs.WalkDir(os.DirFS(dir), "web/html", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// getHtmlTemplate parses embedded HTML templates from the bundled `htmlFS`
// using the provided template function map and returns the resulting
// template set for production usage.
func (s *Server) getHtmlTemplate(funcMap template.FuncMap) (*template.Template, error) {
	t := template.New("").Funcs(funcMap)
	err := fs.WalkDir(htmlFS, "html", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			newT, err := t.ParseFS(htmlFS, path+"/*.html")
			if err != nil {
				// ignore
				return nil
			}
			t = newT
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// initRouter initializes Gin, registers middleware, templates, static
// assets, controllers and returns the configured engine.
func (s *Server) initRouter() (*gin.Engine, error) {
	if config.IsDebug() {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.DefaultWriter = io.Discard
		gin.DefaultErrorWriter = io.Discard
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.Default()

	webDomain, err := s.settingService.GetWebDomain()
	if err != nil {
		return nil, err
	}

	if webDomain != "" {
		engine.Use(middleware.DomainValidatorMiddleware(webDomain))
	}

	secret, err := s.settingService.GetSecret()
	if err != nil {
		return nil, err
	}

	basePath, err := s.settingService.GetBasePath()
	if err != nil {
		return nil, err
	}
	engine.Use(middleware.PerformanceMiddleware(basePath))
	engine.Use(gzip.Gzip(gzip.DefaultCompression))
	assetsBasePath := basePath + "assets/"

	store := cookie.NewStore(secret)
	// Configure default session cookie options, including expiration (MaxAge)
	sessionOptions := sessions.Options{
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if sessionMaxAge, err := s.settingService.GetSessionMaxAge(); err == nil && sessionMaxAge > 0 {
		sessionOptions.MaxAge = sessionMaxAge * 60 // minutes -> seconds
	}
	store.Options(sessionOptions)
	engine.Use(sessions.Sessions("3x-ui", store))
	engine.Use(func(c *gin.Context) {
		portalRoot := basePath
		panelPath := basePath + "panel/"
		apiPath := basePath + "panel/api"
		logoutPath := basePath + "logout"
		portalUsername := strings.TrimSpace(c.Param("portal_reseller"))
		portalPath := strings.TrimSpace(c.Param("portal_path"))
		portalIsReseller := portalUsername != ""
		if portalIsReseller {
			portalRoot = resellerPortalRoot(portalUsername, portalPath)
			panelPath = portalRoot + "panel/"
			apiPath = panelPath + "api"
			logoutPath = portalRoot + "logout"
		}
		// Keep the admin web path out of reseller HTML and use the canonical
		// reseller root for every panel, API, login, and logout URL.
		c.Set("base_path", portalRoot)
		c.Set("asset_path", "/")
		c.Set("portal_root", portalRoot)
		c.Set("panel_path", panelPath)
		c.Set("api_path", apiPath)
		c.Set("logout_path", logoutPath)
		c.Set("portal_username", portalUsername)
		c.Set("portal_is_reseller", portalIsReseller)
	})
	engine.Use(func(c *gin.Context) {
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, assetsBasePath) {
			c.Header("Cache-Control", "max-age=31536000")
		}
	})
	// Reseller portals always live below /reseller/{username}/. A configured
	// portal path adds one segment; an empty path leaves the username as the
	// complete portal address. Legacy aliases are redirected to the current URL.
	engine.Use(func(c *gin.Context) {
		path := c.Request.URL.Path
		lookupPath := path
		legacyBasePath := false
		if basePath != "/" && strings.HasPrefix(path, basePath+"reseller/") {
			lookupPath = "/reseller/" + strings.TrimPrefix(path, basePath+"reseller/")
			legacyBasePath = true
		}
		if !strings.HasPrefix(lookupPath, "/reseller/") {
			c.Next()
			return
		}
		parts := strings.Split(strings.Trim(strings.TrimPrefix(lookupPath, "/reseller/"), "/"), "/")
		if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
			c.Next()
			return
		}
		userService := service.UserService{}
		reseller, err := userService.GetResellerByUsername(parts[0])
		if err != nil || reseller == nil || !reseller.Enabled {
			c.Next()
			return
		}
		portalPath := strings.Trim(reseller.PortalPath, "/")
		canonicalRoot := resellerPortalRoot(reseller.Username, portalPath)
		if !legacyBasePath && portalPath != "" && len(parts) > 1 && parts[1] == portalPath {
			c.Next()
			return
		}
		if len(parts) > 1 && userService.IsResellerPortalPathAlias(reseller, parts[1]) {
			tail := ""
			if len(parts) > 2 {
				tail = strings.Join(parts[2:], "/")
			}
			target := canonicalRoot + tail
			if c.Request.URL.RawQuery != "" {
				target += "?" + c.Request.URL.RawQuery
			}
			c.Redirect(http.StatusTemporaryRedirect, target)
			c.Abort()
			return
		}
		if !legacyBasePath && portalPath == "" {
			c.Next()
			return
		}
		tail := ""
		if len(parts) > 1 {
			tailParts := parts[1:]
			// When migrating from the old main-path-prefixed URL, remove a
			// known portal token so it is not duplicated in the new address.
			if legacyBasePath && (parts[1] == portalPath || userService.IsResellerPortalPathAlias(reseller, parts[1])) {
				tailParts = parts[2:]
			}
			tail = strings.Join(tailParts, "/")
		}
		target := canonicalRoot + tail
		if c.Request.URL.RawQuery != "" {
			target += "?" + c.Request.URL.RawQuery
		}
		c.Redirect(http.StatusTemporaryRedirect, target)
		c.Abort()
	})

	// init i18n
	err = locale.InitLocalizer(i18nFS, &s.settingService)
	if err != nil {
		return nil, err
	}

	// Apply locale middleware for i18n
	i18nWebFunc := func(key string, params ...string) string {
		return locale.I18n(locale.Web, key, params...)
	}
	// Register template functions before loading templates
	funcMap := template.FuncMap{
		"i18n": i18nWebFunc,
	}
	engine.SetFuncMap(funcMap)
	engine.Use(locale.LocalizerMiddleware())

	// set static files and template
	if config.IsDebug() {
		// for development
		files, err := s.getHtmlFiles()
		if err != nil {
			return nil, err
		}
		// Use the registered func map with the loaded templates
		engine.LoadHTMLFiles(files...)
		engine.StaticFS("/assets", http.FS(os.DirFS("web/assets")))
		if basePath != "/" {
			engine.StaticFS(basePath+"assets", http.FS(os.DirFS("web/assets")))
		}
	} else {
		// for production
		template, err := s.getHtmlTemplate(funcMap)
		if err != nil {
			return nil, err
		}
		engine.SetHTMLTemplate(template)
		engine.StaticFS("/assets", http.FS(&wrapAssetsFS{FS: assetsFS}))
		if basePath != "/" {
			engine.StaticFS(basePath+"assets", http.FS(&wrapAssetsFS{FS: assetsFS}))
		}
	}

	// Apply the redirect middleware (`/xui` to `/panel`)
	engine.Use(middleware.RedirectMiddleware(basePath))

	g := engine.Group(basePath)

	s.index = controller.NewIndexController(g)
	s.panel = controller.NewXUIController(g)
	s.api = controller.NewAPIController(g, s.customGeoService)

	resellerPortalMiddleware := func(requirePortalPath bool) gin.HandlerFunc {
		return func(c *gin.Context) {
			username := strings.TrimSpace(c.Param("portal_reseller"))
			portalPath := strings.TrimSpace(c.Param("portal_path"))
			if username == "" {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			userService := service.UserService{}
			reseller, err := userService.GetResellerByUsername(username)
			if err != nil || reseller == nil || !reseller.Enabled ||
				(requirePortalPath && (reseller.PortalPath == "" || portalPath != reseller.PortalPath)) ||
				(!requirePortalPath && (reseller.PortalPath != "" || portalPath != "")) {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			if user := session.GetLoginUser(c); user != nil {
				if userService.IsAdmin(user) {
					c.Redirect(http.StatusTemporaryRedirect, basePath+"panel/")
					c.Abort()
					return
				}
				if userService.IsReseller(user) && !strings.EqualFold(user.Username, username) {
					ownReseller, ownErr := userService.GetResellerByUsername(user.Username)
					if ownErr == nil && ownReseller != nil {
						c.Redirect(http.StatusTemporaryRedirect, resellerPortalRoot(ownReseller.Username, ownReseller.PortalPath)+"panel/")
					} else {
						c.AbortWithStatus(http.StatusNotFound)
					}
					c.Abort()
					return
				}
			}
			c.Next()
		}
	}

	resellerPortal := engine.Group("/reseller/:portal_reseller/:portal_path")
	resellerPortal.Use(resellerPortalMiddleware(true))
	controller.NewResellerPortalController(resellerPortal)
	controller.NewXUIController(resellerPortal)
	controller.NewAPIController(resellerPortal, s.customGeoService)

	resellerRoot := engine.Group("/reseller/:portal_reseller")
	resellerRoot.Use(resellerPortalMiddleware(false))
	controller.NewResellerPortalController(resellerRoot)
	controller.NewXUIController(resellerRoot)
	controller.NewAPIController(resellerRoot, s.customGeoService)

	// Initialize WebSocket hub
	s.wsHub = websocket.NewHub()
	go s.wsHub.Run()

	// Initialize WebSocket controller
	s.ws = controller.NewWebSocketController(s.wsHub)
	// Register WebSocket route with basePath (g already has basePath prefix)
	g.GET("/ws", s.ws.HandleWebSocket)
	resellerPortal.GET("/ws", s.ws.HandleWebSocket)
	resellerRoot.GET("/ws", s.ws.HandleWebSocket)

	// Chrome DevTools endpoint for debugging web apps
	engine.GET("/.well-known/appspecific/com.chrome.devtools.json", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{})
	})

	// Add a catch-all route to handle undefined paths and return 404
	engine.NoRoute(func(c *gin.Context) {
		c.AbortWithStatus(http.StatusNotFound)
	})

	return engine, nil
}

// startTask schedules background jobs (Xray checks, traffic jobs, cron
// jobs) which the panel relies on for periodic maintenance and monitoring.
func (s *Server) startTask() {
	s.customGeoService.EnsureOnStartup()
	err := s.xrayService.RestartXray(true)
	if err != nil {
		logger.Warning("start xray failed:", err)
	}
	// Check whether xray is running every second
	s.cron.AddJob("@every 1s", job.NewCheckXrayRunningJob())

	// Apply requested Xray changes promptly. In particular, a quota poll removes
	// an exhausted client first and requests a restart so existing sessions are
	// dropped; a 30-second restart interval allowed avoidable quota overshoot.
	s.cron.AddFunc("@every 1s", func() {
		if s.xrayService.IsNeedRestartAndSetFalse() {
			err := s.xrayService.RestartXray(false)
			if err != nil {
				logger.Error("restart xray failed:", err)
			}
		}
	})

	go func() {
		time.Sleep(time.Second * 5)
		// Sample traffic every 2 seconds so quota exhaustion is enforced with a
		// shorter polling delay. Expensive reseller sync and full WebSocket
		// snapshots stay throttled inside the job to 10 seconds.
		s.cron.AddJob("@every 2s", job.NewXrayTrafficJob())
	}()

	// Recent client IP observations for the UI. Production admission and
	// enforcement are handled synchronously by the forked Xray gate.
	service.StartClientLimitEngine()

	// check client ips from log file every day
	s.cron.AddJob("@daily", job.NewClearLogsJob())

	// Inbound traffic reset jobs
	// Run every hour
	s.cron.AddJob("@hourly", job.NewPeriodicTrafficResetJob("hourly"))
	// Run once a day, midnight
	s.cron.AddJob("@daily", job.NewPeriodicTrafficResetJob("daily"))
	// Run once a week, midnight between Sat/Sun
	s.cron.AddJob("@weekly", job.NewPeriodicTrafficResetJob("weekly"))
	// Run once a month, midnight, first of month
	s.cron.AddJob("@monthly", job.NewPeriodicTrafficResetJob("monthly"))

	// LDAP sync scheduling
	if ldapEnabled, _ := s.settingService.GetLdapEnable(); ldapEnabled {
		runtime, err := s.settingService.GetLdapSyncCron()
		if err != nil || runtime == "" {
			runtime = "@every 1m"
		}
		j := job.NewLdapSyncJob()
		// job has zero-value services with method receivers that read settings on demand
		s.cron.AddJob(runtime, j)
	}

	// Make a traffic condition every day, 8:30
	var entry cron.EntryID
	isTgbotenabled, err := s.settingService.GetTgbotEnabled()
	if (err == nil) && (isTgbotenabled) {
		runtime, err := s.settingService.GetTgbotRuntime()
		if err != nil || runtime == "" {
			logger.Errorf("Add NewStatsNotifyJob error[%s], Runtime[%s] invalid, will run default", err, runtime)
			runtime = "@daily"
		}
		logger.Infof("Tg notify enabled,run at %s", runtime)
		_, err = s.cron.AddJob(runtime, job.NewStatsNotifyJob())
		if err != nil {
			logger.Warning("Add NewStatsNotifyJob error", err)
			return
		}

		// check for Telegram bot callback query hash storage reset
		s.cron.AddJob("@every 2m", job.NewCheckHashStorageJob())

		// Check CPU load and alarm to TgBot if threshold passes
		cpuThreshold, err := s.settingService.GetTgCpu()
		if (err == nil) && (cpuThreshold > 0) {
			s.cron.AddJob("@every 10s", job.NewCheckCpuJob())
		}
	} else {
		s.cron.Remove(entry)
	}
}

// Start initializes and starts the web server with configured settings, routes, and background jobs.
func (s *Server) Start() (err error) {
	// This is an anonymous function, no function name
	defer func() {
		if err != nil {
			s.Stop()
		}
	}()

	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return err
	}
	s.cron = cron.New(cron.WithLocation(loc), cron.WithSeconds())
	s.cron.Start()

	s.customGeoService = service.NewCustomGeoService()

	engine, err := s.initRouter()
	if err != nil {
		return err
	}

	certFile, err := s.settingService.GetCertFile()
	if err != nil {
		return err
	}
	keyFile, err := s.settingService.GetKeyFile()
	if err != nil {
		return err
	}
	listen, err := s.settingService.GetListen()
	if err != nil {
		return err
	}
	port, err := s.settingService.GetPort()
	if err != nil {
		return err
	}
	listenAddr := net.JoinHostPort(listen, strconv.Itoa(port))
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	if certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err == nil {
			c := &tls.Config{
				Certificates: []tls.Certificate{cert},
			}
			listener = network.NewAutoHttpsListener(listener)
			listener = tls.NewListener(listener, c)
			logger.Info("Web server running HTTPS on", listener.Addr())
		} else {
			logger.Error("Error loading certificates:", err)
			logger.Info("Web server running HTTP on", listener.Addr())
		}
	} else {
		logger.Info("Web server running HTTP on", listener.Addr())
	}
	s.listener = listener

	s.httpServer = &http.Server{
		Handler: engine,
	}

	go func() {
		s.httpServer.Serve(listener)
	}()

	s.startTask()

	isTgbotenabled, err := s.settingService.GetTgbotEnabled()
	if (err == nil) && (isTgbotenabled) {
		tgBot := s.tgbotService.NewTgbot()
		tgBot.Start(i18nFS)
	}

	return nil
}

// Stop gracefully shuts down the web server, stops Xray, cron jobs, and Telegram bot.
func (s *Server) Stop() error {
	s.cancel()
	s.xrayService.StopXray()
	if s.cron != nil {
		s.cron.Stop()
	}
	if s.tgbotService.IsRunning() {
		s.tgbotService.Stop()
	}
	// Gracefully stop WebSocket hub
	if s.wsHub != nil {
		s.wsHub.Stop()
	}
	var err1 error
	var err2 error
	if s.httpServer != nil {
		err1 = s.httpServer.Shutdown(s.ctx)
	}
	if s.listener != nil {
		err2 = s.listener.Close()
	}
	return common.Combine(err1, err2)
}

// GetCtx returns the server's context for cancellation and deadline management.
func (s *Server) GetCtx() context.Context {
	return s.ctx
}

// GetCron returns the server's cron scheduler instance.
func (s *Server) GetCron() *cron.Cron {
	return s.cron
}

// GetWSHub returns the WebSocket hub instance.
func (s *Server) GetWSHub() any {
	return s.wsHub
}

func (s *Server) RestartXray() error {
	return s.xrayService.RestartXray(true)
}
