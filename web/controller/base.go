// Package controller provides HTTP request handlers and controllers for the 3x-ui web management panel.
// It handles routing, authentication, and API endpoints for managing Xray inbounds, settings, and more.
package controller

import (
	"net/http"

	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/locale"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// BaseController provides common functionality for all controllers, including authentication checks.
type BaseController struct{}

// checkLogin is a middleware that verifies user authentication and handles unauthorized access.
func (a *BaseController) checkLogin(c *gin.Context) {
	currentUser := session.GetLoginUser(c)
	if currentUser == nil {
		if isAjax(c) {
			pureJsonMsg(c, http.StatusUnauthorized, false, I18nWeb(c, "pages.login.loginAgain"))
		} else {
			target := c.GetString("portal_root")
			if target == "" {
				target = c.GetString("base_path")
			}
			c.Redirect(http.StatusTemporaryRedirect, target)
		}
		c.Abort()
		return
	}
	if freshUser, err := (&service.UserService{}).GetUserByID(currentUser.Id); err == nil && freshUser != nil {
		if !freshUser.Enabled {
			session.ClearSession(c)
			_ = sessions.Default(c).Save()
			if isAjax(c) {
				pureJsonMsg(c, http.StatusUnauthorized, false, I18nWeb(c, "pages.login.loginAgain"))
			} else {
				target := c.GetString("portal_root")
				if target == "" {
					target = c.GetString("base_path")
				}
				c.Redirect(http.StatusTemporaryRedirect, target)
			}
			c.Abort()
			return
		}
		session.SetLoginUser(c, freshUser)
		_ = sessions.Default(c).Save()
	} else if err != nil {
		session.ClearSession(c)
		_ = sessions.Default(c).Save()
		if isAjax(c) {
			pureJsonMsg(c, http.StatusUnauthorized, false, I18nWeb(c, "pages.login.loginAgain"))
		} else {
			target := c.GetString("portal_root")
			if target == "" {
				target = c.GetString("base_path")
			}
			c.Redirect(http.StatusTemporaryRedirect, target)
		}
		c.Abort()
		return
	}
	c.Next()
}

func (a *BaseController) checkAdmin(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil || !(&service.UserService{}).IsAdmin(user) {
		if isAjax(c) {
			pureJsonMsg(c, http.StatusForbidden, false, "Admin access required")
		} else {
			target := c.GetString("panel_path")
			if target == "" {
				target = c.GetString("base_path") + "panel/"
			}
			c.Redirect(http.StatusTemporaryRedirect, target)
		}
		c.Abort()
		return
	}
	c.Next()
}

// I18nWeb retrieves an internationalized message for the web interface based on the current locale.
func I18nWeb(c *gin.Context, name string, params ...string) string {
	anyfunc, funcExists := c.Get("I18n")
	if !funcExists {
		logger.Warning("I18n function not exists in gin context!")
		return ""
	}
	i18nFunc, _ := anyfunc.(func(i18nType locale.I18nType, key string, keyParams ...string) string)
	msg := i18nFunc(locale.Web, name, params...)
	return msg
}
