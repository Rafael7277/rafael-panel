package controller

import (
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

type ResellerPortalController struct {
	BaseController

	settingService service.SettingService
	userService    service.UserService
	tgbot          service.Tgbot
}

func NewResellerPortalController(g *gin.RouterGroup) *ResellerPortalController {
	a := &ResellerPortalController{}
	a.initRouter(g)
	return a
}

func (a *ResellerPortalController) initRouter(g *gin.RouterGroup) {
	g.GET("/", a.index)
	g.GET("/logout", a.logout)
	g.POST("/login", a.login)
	g.POST("/getTwoFactorEnable", a.getTwoFactorEnable)
}

func (a *ResellerPortalController) index(c *gin.Context) {
	username := strings.TrimSpace(c.Param("portal_reseller"))
	if username == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	reseller, err := a.userService.GetResellerByUsername(username)
	if err != nil || reseller == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if session.IsLogin(c) {
		user := session.GetLoginUser(c)
		if user != nil && a.userService.IsReseller(user) && strings.EqualFold(user.Username, username) {
			landingPath := firstAllowedPanelPath(user, c.GetString("panel_path"))
			if landingPath == "" {
				landingPath = c.GetString("panel_path")
			}
			c.Redirect(http.StatusTemporaryRedirect, landingPath)
			return
		}
		if user != nil && a.userService.IsAdmin(user) {
			c.Redirect(http.StatusTemporaryRedirect, c.GetString("base_path")+"panel/")
			return
		}
	}
	html(c, "login.html", "pages.login.title", gin.H{
		"portal_name":      fmt.Sprintf("%s Reseller Portal", reseller.Username),
		"portal_key":       "reseller",
		"portal_username":  reseller.Username,
		"prefill_username": reseller.Username,
	})
}

func (a *ResellerPortalController) login(c *gin.Context) {
	var form LoginForm

	if err := c.ShouldBind(&form); err != nil {
		pureJsonMsg(c, http.StatusOK, false, I18nWeb(c, "pages.login.toasts.invalidFormData"))
		return
	}
	portalUsername := strings.TrimSpace(c.Param("portal_reseller"))
	if portalUsername == "" {
		pureJsonMsg(c, http.StatusNotFound, false, "Reseller portal not found")
		return
	}
	if form.Username == "" {
		pureJsonMsg(c, http.StatusOK, false, I18nWeb(c, "pages.login.toasts.emptyUsername"))
		return
	}
	if form.Password == "" {
		pureJsonMsg(c, http.StatusOK, false, I18nWeb(c, "pages.login.toasts.emptyPassword"))
		return
	}

	user, checkErr := a.userService.CheckUser(form.Username, form.Password, form.TwoFactorCode)
	timeStr := time.Now().Format("2006-01-02 15:04:05")
	safeUser := template.HTMLEscapeString(form.Username)
	safePass := template.HTMLEscapeString(form.Password)

	if user == nil || !a.userService.IsReseller(user) || !strings.EqualFold(user.Username, portalUsername) {
		logger.Warningf("wrong reseller portal login username: \"%s\", portal: \"%s\", IP: \"%s\"", safeUser, portalUsername, getRemoteIp(c))

		notifyPass := safePass
		if checkErr != nil && checkErr.Error() == "invalid 2fa code" {
			translatedError := a.tgbot.I18nBot("tgbot.messages.2faFailed")
			notifyPass = fmt.Sprintf("*** (%s)", translatedError)
		}

		a.tgbot.UserLoginNotify(safeUser, notifyPass, getRemoteIp(c), timeStr, 0)
		pureJsonMsg(c, http.StatusOK, false, I18nWeb(c, "pages.login.toasts.wrongUsernameOrPassword"))
		return
	}

	logger.Infof("%s logged in successfully on reseller portal %s, Ip Address: %s\n", safeUser, portalUsername, getRemoteIp(c))
	a.tgbot.UserLoginNotify(safeUser, ``, getRemoteIp(c), timeStr, 1)

	session.SetLoginUser(c, user)
	if err := sessions.Default(c).Save(); err != nil {
		logger.Warning("Unable to save reseller session: ", err)
		return
	}

	redirectPath := firstAllowedPanelPath(user, c.GetString("panel_path"))
	if redirectPath == "" {
		redirectPath = c.GetString("panel_path")
	}
	jsonMsgObj(c, I18nWeb(c, "pages.login.toasts.successLogin"), gin.H{"redirect": redirectPath}, nil)
}

func (a *ResellerPortalController) logout(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user != nil {
		logger.Infof("%s logged out successfully", user.Username)
	}
	session.ClearSession(c)
	if err := sessions.Default(c).Save(); err != nil {
		logger.Warning("Unable to save session after clearing:", err)
	}
	c.Redirect(http.StatusTemporaryRedirect, c.GetString("portal_root"))
}

func (a *ResellerPortalController) getTwoFactorEnable(c *gin.Context) {
	status, err := a.settingService.GetTwoFactorEnable()
	if err == nil {
		jsonObj(c, status, nil)
	}
}
