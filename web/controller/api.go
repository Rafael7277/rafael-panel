package controller

import (
	"net/http"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"

	"github.com/gin-gonic/gin"
)

// APIController handles the main API routes for the 3x-ui panel, including inbounds and server management.
type APIController struct {
	BaseController
	inboundController *InboundController
	serverController  *ServerController
	nodeController    *NodeController
	settingService    service.SettingService
	apiTokenService   service.ApiTokenService
	Tgbot             service.Tgbot
}

// NewAPIController creates a new APIController instance and initializes its routes.
func NewAPIController(g *gin.RouterGroup, customGeo *service.CustomGeoService) *APIController {
	a := &APIController{}
	a.initRouter(g, customGeo)
	return a
}

// checkAPIAuth is a middleware that returns 404 for unauthenticated API requests
// to hide the existence of API endpoints from unauthorized users
func (a *APIController) checkAPIAuth(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		if a.apiTokenService.Match(after) {
			c.Next()
			return
		}
		secret, err := a.settingService.GetSecret()
		if err == nil && strings.TrimSpace(after) != "" && after == string(secret) {
			c.Next()
			return
		}
	}
	if !session.IsLogin(c) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Next()
}

// initRouter sets up the API routes for inbounds, server, and other endpoints.
func (a *APIController) initRouter(g *gin.RouterGroup, customGeo *service.CustomGeoService) {
	// Main API group
	api := g.Group("/panel/api")
	api.Use(a.checkAPIAuth)

	// Inbounds API
	inbounds := api.Group("/inbounds")
	inbounds.Use(a.requireResellerPermission("inbounds"))
	a.inboundController = NewInboundController(inbounds)

	multiLinks := api.Group("/multilinks")
	multiLinks.Use(a.requireResellerPermission("multilinks"))
	NewMultiLinkController(multiLinks)

	// Server API
	server := api.Group("/server")
	a.serverController = NewServerController(server)

	// Nodes API
	nodes := api.Group("/nodes")
	nodes.Use(a.checkAdmin)
	a.nodeController = NewNodeController(nodes)

	NewCustomGeoController(api.Group("/custom-geo"), customGeo)

	resellers := api.Group("/resellers")
	NewResellerController(resellers)

	// Extra routes
	api.GET("/backuptotgbot", a.BackuptoTgbot)
}

func (a *APIController) requireResellerPermission(area string) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := session.GetLoginUser(c)
		userService := &service.UserService{}
		if user != nil && userService.IsReseller(user) {
			freshUser, err := userService.GetUserByID(user.Id)
			if err != nil || freshUser == nil || !freshUser.Enabled || !userService.CanAccess(freshUser, area) {
				pureJsonMsg(c, http.StatusForbidden, false, "Access denied")
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

// BackuptoTgbot sends a backup of the panel data to Telegram bot admins.
func (a *APIController) BackuptoTgbot(c *gin.Context) {
	a.Tgbot.SendBackupToAdmins()
}
