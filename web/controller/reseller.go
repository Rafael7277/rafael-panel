package controller

import (
	"encoding/json"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"
	"github.com/mhsanaei/3x-ui/v2/web/websocket"
)

type ResellerController struct {
	BaseController
	userService service.UserService
}

type resellerUpsertForm struct {
	Username          string                      `json:"username" form:"username"`
	Password          string                      `json:"password" form:"password"`
	PortalPath        *string                     `json:"portalPath" form:"portalPath"`
	Enabled           bool                        `json:"enabled" form:"enabled"`
	MaxUsers          int                         `json:"maxUsers" form:"maxUsers"`
	MaxTraffic        int64                       `json:"maxTraffic" form:"maxTraffic"`
	Permissions       service.ResellerPermissions `json:"permissions" form:"-"`
	AllowedInboundIDs []int                       `json:"allowedInboundIds" form:"allowedInboundIds"`
	ClientAction      string                      `json:"clientAction" form:"clientAction"`
}

// bindResellerUpsertForm handles the reseller permissions payload explicitly.
// The panel's shared Axios setup sends application/x-www-form-urlencoded and
// serializes nested values as permissions[overview]=true, which Gin does not
// bind into a nested struct. Newer clients send the permissions object as JSON
// in the form field; this also accepts the legacy bracketed fields.
func bindResellerUpsertForm(c *gin.Context, form *resellerUpsertForm) error {
	if c.ContentType() == "application/json" {
		return c.ShouldBindJSON(form)
	}
	if err := c.ShouldBind(form); err != nil {
		return err
	}
	if raw := c.PostForm("permissions"); raw != "" {
		return json.Unmarshal([]byte(raw), &form.Permissions)
	}
	form.Permissions = service.ResellerPermissions{
		Overview:   resellerFormBool(c, "overview"),
		Inbounds:   resellerFormBool(c, "inbounds"),
		MultiLinks: resellerFormBool(c, "multiLinks"),
	}
	return nil
}

func resellerFormBool(c *gin.Context, name string) bool {
	value := c.PostForm("permissions[" + name + "]")
	if value == "" {
		value = c.PostForm("permissions." + name)
	}
	parsed, _ := strconv.ParseBool(value)
	return parsed
}

type resellerDeleteForm struct {
	ClientAction string `json:"clientAction" form:"clientAction"`
}

func NewResellerController(g *gin.RouterGroup) *ResellerController {
	a := &ResellerController{}
	a.initRouter(g)
	return a
}

func (a *ResellerController) initRouter(g *gin.RouterGroup) {
	g.GET("/list", a.list)
	g.GET("/me", a.me)
	g.GET("/inbound-options", a.inboundOptions)
	g.POST("/add", a.add)
	g.POST("/update/:id", a.update)
	g.POST("/del/:id", a.del)
}

func (a *ResellerController) list(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil || !a.userService.IsAdmin(user) {
		pureJsonMsg(c, 403, false, "Admin access required")
		return
	}
	rows, err := a.userService.ListResellers(user.Id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonObj(c, rows, nil)
}

func (a *ResellerController) me(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil || !a.userService.IsReseller(user) {
		pureJsonMsg(c, 403, false, "Access denied")
		return
	}
	row, err := a.userService.GetResellerView(user.Id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonObj(c, row, nil)
}

func (a *ResellerController) inboundOptions(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil || !a.userService.IsAdmin(user) {
		pureJsonMsg(c, 403, false, "Access denied")
		return
	}
	inbounds, err := (&service.InboundService{}).GetInbounds(user.Id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	type inboundOption struct {
		Id      int    `json:"id"`
		Label   string `json:"label"`
		Remark  string `json:"remark"`
		Port    int    `json:"port"`
		Enabled bool   `json:"enabled"`
	}
	rows := make([]inboundOption, 0, len(inbounds))
	for _, inbound := range inbounds {
		if inbound == nil {
			continue
		}
		label := "#" + strconv.Itoa(inbound.Id) + " | " + inbound.Remark + " | " + strconv.Itoa(inbound.Port)
		rows = append(rows, inboundOption{
			Id:      inbound.Id,
			Label:   label,
			Remark:  inbound.Remark,
			Port:    inbound.Port,
			Enabled: inbound.Enable,
		})
	}
	jsonObj(c, rows, nil)
}

func (a *ResellerController) add(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil || !a.userService.IsAdmin(user) {
		pureJsonMsg(c, 403, false, "Admin access required")
		return
	}
	form := &resellerUpsertForm{}
	if err := bindResellerUpsertForm(c, form); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	if form.Enabled && !service.HasAnyResellerPermission(form.Permissions) {
		pureJsonMsg(c, 200, false, I18nWeb(c, "pages.resellerAccess.permissionRequired"))
		return
	}
	portalPath := ""
	if form.PortalPath != nil {
		portalPath = *form.PortalPath
	}
	row, err := a.userService.CreateResellerWithPortalPath(user.Id, form.Username, form.Password, portalPath, form.MaxUsers, form.MaxTraffic, form.Enabled, form.Permissions, form.AllowedInboundIDs)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
	jsonObj(c, row, nil)
}

func (a *ResellerController) update(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil || !a.userService.IsAdmin(user) {
		pureJsonMsg(c, 403, false, "Admin access required")
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	form := &resellerUpsertForm{}
	if err := bindResellerUpsertForm(c, form); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	if form.Enabled && !service.HasAnyResellerPermission(form.Permissions) {
		pureJsonMsg(c, 200, false, I18nWeb(c, "pages.resellerAccess.permissionRequired"))
		return
	}
	row, err := a.userService.UpdateResellerWithOptions(user.Id, id, form.Username, form.Password, form.MaxUsers, form.MaxTraffic, form.Enabled, form.Permissions, form.AllowedInboundIDs, service.ResellerUpdateOptions{
		ClientAction: form.ClientAction,
		PortalPath:   form.PortalPath,
	})
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
	jsonObj(c, row, nil)
}

func (a *ResellerController) del(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil || !a.userService.IsAdmin(user) {
		pureJsonMsg(c, 403, false, "Admin access required")
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	form := &resellerDeleteForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	if err := a.userService.DeleteResellerWithOptions(user.Id, id, service.ResellerDeleteOptions{
		ClientAction: form.ClientAction,
	}); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
	jsonMsg(c, "Reseller deleted", nil)
}
