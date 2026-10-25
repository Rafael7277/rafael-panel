package controller

import (
	"net"
	"net/http"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/entity"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"

	"github.com/gin-gonic/gin"
)

// getRemoteIp extracts the real IP address from the request headers or remote address.
func getRemoteIp(c *gin.Context) string {
	value := c.GetHeader("X-Real-IP")
	if value != "" {
		return value
	}
	value = c.GetHeader("X-Forwarded-For")
	if value != "" {
		ips := strings.Split(value, ",")
		return ips[0]
	}
	addr := c.Request.RemoteAddr
	ip, _, _ := net.SplitHostPort(addr)
	return ip
}

// jsonMsg sends a JSON response with a message and error status.
func jsonMsg(c *gin.Context, msg string, err error) {
	jsonMsgObj(c, msg, nil, err)
}

// jsonObj sends a JSON response with an object and error status.
func jsonObj(c *gin.Context, obj any, err error) {
	jsonMsgObj(c, "", obj, err)
}

// jsonMsgObj sends a JSON response with a message, object, and error status.
func jsonMsgObj(c *gin.Context, msg string, obj any, err error) {
	m := entity.Msg{
		Obj: obj,
	}
	if err == nil {
		m.Success = true
		if msg != "" {
			m.Msg = msg
		}
	} else {
		m.Success = false
		errStr := err.Error()
		if errStr != "" {
			m.Msg = msg + " (" + errStr + ")"
			logger.Warning(msg+" "+I18nWeb(c, "fail")+": ", err)
		} else if msg != "" {
			m.Msg = msg
			logger.Warning(msg + " " + I18nWeb(c, "fail"))
		} else {
			m.Msg = I18nWeb(c, "somethingWentWrong")
			logger.Warning(I18nWeb(c, "somethingWentWrong") + " " + I18nWeb(c, "fail"))
		}
	}
	c.JSON(http.StatusOK, m)
}

// pureJsonMsg sends a pure JSON message response with custom status code.
func pureJsonMsg(c *gin.Context, statusCode int, success bool, msg string) {
	c.JSON(statusCode, entity.Msg{
		Success: success,
		Msg:     msg,
	})
}

// html renders an HTML template with the provided data and title.
func firstAllowedPanelPath(user *model.User, panelPath string) string {
	area := (&service.UserService{}).FirstAllowedPanelArea(user)
	switch area {
	case "overview":
		return panelPath
	case "inbounds":
		return panelPath + "inbounds"
	case "multilinks":
		return panelPath + "multilinks"
	default:
		return ""
	}
}

func html(c *gin.Context, name string, title string, data gin.H) {
	if data == nil {
		data = gin.H{}
	}
	data["title"] = title
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.GetHeader("X-Real-IP")
	}
	if host == "" {
		var err error
		host, _, err = net.SplitHostPort(c.Request.Host)
		if err != nil {
			host = c.Request.Host
		}
	}
	data["host"] = host
	data["request_uri"] = c.Request.RequestURI
	data["base_path"] = c.GetString("base_path")
	assetPath := c.GetString("asset_path")
	if assetPath == "" {
		assetPath = c.GetString("base_path")
	}
	data["asset_path"] = assetPath
	data["portal_root"] = c.GetString("portal_root")
	data["panel_path"] = c.GetString("panel_path")
	data["api_path"] = c.GetString("api_path")
	data["logout_path"] = c.GetString("logout_path")
	data["portal_username"] = c.GetString("portal_username")
	data["portal_is_reseller"] = c.GetBool("portal_is_reseller")
	if _, ok := data["portal_name"]; !ok {
		data["portal_name"] = ""
	}
	if _, ok := data["portal_key"]; !ok {
		data["portal_key"] = ""
	}
	if _, ok := data["prefill_username"]; !ok {
		data["prefill_username"] = ""
	}
	if user := session.GetLoginUser(c); user != nil {
		data["login_user"] = user
		data["login_is_admin"] = (&service.UserService{}).IsAdmin(user)
		data["login_role_name"] = user.Role
		data["login_permissions"] = user.Permissions
	}
	c.HTML(http.StatusOK, name, getContext(data))
}

// getContext adds version and other context data to the provided gin.H.
func getContext(h gin.H) gin.H {
	a := gin.H{
		"cur_ver": config.GetVersion(),
	}
	for key, value := range h {
		a[key] = value
	}
	return a
}

// isAjax checks if the request is an AJAX request.
func isAjax(c *gin.Context) bool {
	return c.GetHeader("X-Requested-With") == "XMLHttpRequest"
}
