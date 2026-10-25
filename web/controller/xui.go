package controller

import (
	"github.com/gin-gonic/gin"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"
)

// XUIController is the main controller for the X-UI panel, managing sub-controllers.
type XUIController struct {
	BaseController

	settingController     *SettingController
	xraySettingController *XraySettingController
}

// NewXUIController creates a new XUIController and initializes its routes.
func NewXUIController(g *gin.RouterGroup) *XUIController {
	a := &XUIController{}
	a.initRouter(g)
	return a
}

// initRouter sets up the main panel routes and initializes sub-controllers.
func (a *XUIController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/panel")
	g.Use(a.checkLogin)

	g.GET("/", a.index)
	g.GET("/inbounds", a.inbounds)
	g.GET("/nodes", a.nodes)
	g.GET("/multilinks", a.multilinks)
	g.GET("/resellers", a.resellers)
	g.GET("/settings", a.settings)
	g.GET("/xray", a.xraySettings)

	a.settingController = NewSettingController(g)
	a.xraySettingController = NewXraySettingController(g)
}

// index renders the main panel index page.
func (a *XUIController) index(c *gin.Context) {
	if !a.allowArea(c, "overview") {
		if landingPath := firstAllowedPanelPath(session.GetLoginUser(c), c.GetString("panel_path")); landingPath != "" {
			c.Redirect(302, landingPath)
			return
		}
		if (&service.UserService{}).IsReseller(session.GetLoginUser(c)) {
			html(c, "reseller_no_access.html", "pages.resellerAccess.title", nil)
			return
		}
		c.AbortWithStatus(403)
		return
	}
	html(c, "index.html", "pages.index.title", nil)
}

// inbounds renders the inbounds management page.
func (a *XUIController) inbounds(c *gin.Context) {
	if !a.allowArea(c, "inbounds") {
		a.redirectToAllowedArea(c)
		return
	}
	html(c, "inbounds.html", "pages.inbounds.title", nil)
}

// nodes renders the node management page.
func (a *XUIController) nodes(c *gin.Context) {
	if !(&service.UserService{}).IsAdmin(session.GetLoginUser(c)) {
		c.Redirect(302, c.GetString("panel_path"))
		return
	}
	html(c, "nodes.html", "menu.inbounds", nil)
}

func (a *XUIController) multilinks(c *gin.Context) {
	if !a.allowArea(c, "multilinks") {
		a.redirectToAllowedArea(c)
		return
	}
	html(c, "multilinks.html", "Multi link", nil)
}

func (a *XUIController) userServiceCanAccess(c *gin.Context, area string) bool {
	return (&service.UserService{}).CanAccess(session.GetLoginUser(c), area)
}

func (a *XUIController) allowArea(c *gin.Context, area string) bool {
	return a.userServiceCanAccess(c, area)
}

func (a *XUIController) redirectToAllowedArea(c *gin.Context) {
	if landingPath := firstAllowedPanelPath(session.GetLoginUser(c), c.GetString("panel_path")); landingPath != "" {
		c.Redirect(302, landingPath)
		return
	}
	if (&service.UserService{}).IsReseller(session.GetLoginUser(c)) {
		html(c, "reseller_no_access.html", "pages.resellerAccess.title", nil)
		return
	}
	c.AbortWithStatus(403)
}

func (a *XUIController) resellers(c *gin.Context) {
	if !(&service.UserService{}).IsAdmin(session.GetLoginUser(c)) {
		c.Redirect(302, c.GetString("panel_path"))
		return
	}
	html(c, "resellers.html", "Resellers", nil)
}

// settings renders the settings management page.
func (a *XUIController) settings(c *gin.Context) {
	if !(&service.UserService{}).IsAdmin(session.GetLoginUser(c)) {
		c.Redirect(302, c.GetString("panel_path"))
		return
	}
	html(c, "settings.html", "pages.settings.title", nil)
}

// xraySettings renders the Xray settings page.
func (a *XUIController) xraySettings(c *gin.Context) {
	if !(&service.UserService{}).IsAdmin(session.GetLoginUser(c)) {
		c.Redirect(302, c.GetString("panel_path"))
		return
	}
	html(c, "xray.html", "pages.xray.title", nil)
}
