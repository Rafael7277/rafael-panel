package controller

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"

	"github.com/gin-gonic/gin"
)

type MultiLinkController struct {
	multiLinkService service.MultiLinkService
	xrayService      service.XrayService
	settingService   service.SettingService
}

func NewMultiLinkController(g *gin.RouterGroup) *MultiLinkController {
	a := &MultiLinkController{}
	a.initRouter(g)
	return a
}

func (a *MultiLinkController) initRouter(g *gin.RouterGroup) {
	g.GET("/list", a.list)
	g.GET("/get/:subId", a.get)
	g.POST("/setEnabled/:subId", a.setEnabled)
	g.POST("/update/:subId", a.update)
	g.POST("/delete/:subId", a.delete)
}

func (a *MultiLinkController) list(c *gin.Context) {
	user := session.GetLoginUser(c)
	var userID *int
	if user != nil && !(&service.UserService{}).IsAdmin(user) {
		userID = &user.Id
	}
	rows, err := a.multiLinkService.ListByUser(userID)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	a.fillSubURLs(c, rows)
	jsonObj(c, rows, nil)
}

func (a *MultiLinkController) get(c *gin.Context) {
	subID, _ := url.PathUnescape(c.Param("subId"))
	user := session.GetLoginUser(c)
	var userID *int
	if user != nil && !(&service.UserService{}).IsAdmin(user) {
		userID = &user.Id
	}
	row, err := a.multiLinkService.GetByUser(subID, userID)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	a.fillSubURL(c, row)
	jsonObj(c, row, nil)
}

func (a *MultiLinkController) fillSubURLs(c *gin.Context, rows []*service.MultiLinkRow) {
	for _, row := range rows {
		a.fillSubURL(c, row)
	}
}

func (a *MultiLinkController) fillSubURL(c *gin.Context, row *service.MultiLinkRow) {
	if row == nil || row.SubID == "" {
		return
	}
	allSetting, err := a.settingService.GetAllSetting()
	if err != nil {
		return
	}
	scheme := "http"
	if strings.TrimSpace(allSetting.SubKeyFile) != "" && strings.TrimSpace(allSetting.SubCertFile) != "" {
		scheme = "https"
	}
	host := strings.TrimSpace(allSetting.SubDomain)
	if host == "" {
		host = c.GetHeader("X-Forwarded-Host")
		if host == "" {
			host = c.Request.Host
		}
	}
	hostOnly := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = parsedHost
	} else if strings.Contains(host, ":") && !strings.Contains(host, "]") {
		hostOnly = strings.Split(host, ":")[0]
	}
	subPort := allSetting.SubPort
	if subPort <= 0 {
		subPort = 2096
	}
	path := allSetting.SubPath
	if path == "" {
		path = "/sub/"
	}
	base := fmt.Sprintf("%s://%s:%d%s", scheme, hostOnly, subPort, path)
	if strings.HasSuffix(base, "/") {
		row.SubURL = base + url.PathEscape(row.SubID)
		return
	}
	row.SubURL = base + "/" + url.PathEscape(row.SubID)
}

type multiLinkEnabledForm struct {
	Enabled bool `json:"enabled" form:"enabled"`
}

func (a *MultiLinkController) setEnabled(c *gin.Context) {
	subID, _ := url.PathUnescape(c.Param("subId"))
	form := &multiLinkEnabledForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	user := session.GetLoginUser(c)
	var userID *int
	if user != nil && !(&service.UserService{}).IsAdmin(user) {
		userID = &user.Id
	}
	needRestart, err := a.multiLinkService.SetEnabledByUser(subID, form.Enabled, userID)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundClientUpdateSuccess"), nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *MultiLinkController) update(c *gin.Context) {
	subID, _ := url.PathUnescape(c.Param("subId"))
	form := &service.MultiLinkUpdate{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	user := session.GetLoginUser(c)
	var userID *int
	if user != nil && !(&service.UserService{}).IsAdmin(user) {
		userID = &user.Id
	}
	if user != nil && (&service.UserService{}).IsReseller(user) {
		if form.TotalGB <= 0 {
			jsonMsg(c, "Resellers cannot create unlimited multi links", fmt.Errorf("unlimited multi links are not allowed for resellers"))
			return
		}
		currentRow, err := a.multiLinkService.GetByUser(subID, userID)
		if err != nil {
			jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
			return
		}
		_, currentAssigned, _, err := (&service.UserService{}).GetUserQuotaSnapshot(user.Id)
		if err != nil {
			jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
			return
		}
		currentReserved := remainingAssignedQuota(currentRow.Total, currentRow.Used)
		nextReserved := form.TotalGB
		if !form.ResetTraffic {
			nextReserved = remainingAssignedQuota(form.TotalGB, currentRow.Used)
		}
		nextAssigned := currentAssigned - currentReserved + nextReserved
		if user.MaxTraffic > 0 && nextAssigned > user.MaxTraffic {
			jsonMsg(c, "Reseller assigned traffic quota exceeded", fmt.Errorf("max traffic exceeded"))
			return
		}
	}
	needRestart, err := a.multiLinkService.UpdateByUser(subID, form, userID)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundClientUpdateSuccess"), nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *MultiLinkController) delete(c *gin.Context) {
	subID, _ := url.PathUnescape(c.Param("subId"))
	user := session.GetLoginUser(c)
	var userID *int
	if user != nil && !(&service.UserService{}).IsAdmin(user) {
		userID = &user.Id
	}
	needRestart, err := a.multiLinkService.DeleteByUser(subID, userID)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundClientDeleteSuccess"), nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
}
