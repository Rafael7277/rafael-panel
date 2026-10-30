package controller

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"
	"github.com/mhsanaei/3x-ui/v2/web/websocket"
	"github.com/mhsanaei/3x-ui/v2/xray"

	"github.com/gin-gonic/gin"
)

func clientBelongsToUser(client model.Client, user *model.User) bool {
	return service.ClientOwnedByReseller(client, user)
}

type addClientRequest struct {
	model.Inbound
	MultiLink       bool  `json:"multiLink" form:"multiLink"`
	MultiInboundIDs []int `json:"multiInboundIds" form:"multiInboundIds"`
}

// InboundController handles HTTP requests related to Xray inbounds management.
type InboundController struct {
	inboundService service.InboundService
	xrayService    service.XrayService
	userService    service.UserService
}

func inboundClientMatches(protocol model.Protocol, client model.Client, clientID string) bool {
	switch protocol {
	case model.Trojan:
		return client.Password == clientID
	case model.Shadowsocks:
		return client.Email == clientID
	case model.Hysteria:
		return client.Auth == clientID
	default:
		return client.ID == clientID
	}
}

func (a *InboundController) ensureInboundAccess(c *gin.Context, inboundID int) (*model.Inbound, bool) {
	inbound, err := a.inboundService.GetInbound(inboundID)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return nil, false
	}
	user := session.GetLoginUser(c)
	if user != nil && !a.userService.IsAdmin(user) {
		if a.userService.IsReseller(user) {
			if !a.userService.CanResellerUseInbound(user, inbound) {
				pureJsonMsg(c, 403, false, "Access denied")
				return nil, false
			}
		} else if inbound.UserId != user.Id {
			pureJsonMsg(c, 403, false, "Access denied")
			return nil, false
		}
	}
	return inbound, true
}

func (a *InboundController) getAccessibleInbounds(c *gin.Context) ([]*model.Inbound, error) {
	user := session.GetLoginUser(c)
	if user == nil {
		return []*model.Inbound{}, nil
	}
	if user.Id > 0 {
		if freshUser, err := a.userService.GetUserByID(user.Id); err == nil && freshUser != nil {
			user = freshUser
		}
	}
	inbounds, err := a.userService.GetAccessibleInbounds(user)
	if err != nil {
		return nil, err
	}
	sort.Slice(inbounds, func(i, j int) bool {
		if inbounds[i].Port == inbounds[j].Port {
			return inbounds[i].Id < inbounds[j].Id
		}
		return inbounds[i].Port < inbounds[j].Port
	})
	return inbounds, nil
}

func (a *InboundController) resellerCanManageClients(c *gin.Context, inbound *model.Inbound) bool {
	user := session.GetLoginUser(c)
	if user == nil || !a.userService.IsReseller(user) {
		return true
	}
	if inbound == nil {
		pureJsonMsg(c, 404, false, "Inbound not found")
		return false
	}
	if a.userService.CanResellerUseInbound(user, inbound) {
		return true
	}
	pureJsonMsg(c, 403, false, "Access denied")
	return false
}

func (a *InboundController) forbidResellerInboundConfig(c *gin.Context) bool {
	user := session.GetLoginUser(c)
	if user != nil && a.userService.IsReseller(user) {
		pureJsonMsg(c, 403, false, "Access denied")
		return true
	}
	return false
}

func (a *InboundController) ensureEmailAccess(c *gin.Context, email string) bool {
	_, inbound, err := a.inboundService.GetClientInboundByEmail(email)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return false
	}
	if inbound == nil {
		pureJsonMsg(c, 404, false, "Client not found")
		return false
	}
	user := session.GetLoginUser(c)
	if user != nil && !a.userService.IsAdmin(user) {
		if _, ok := a.ensureInboundAccess(c, inbound.Id); !ok {
			return false
		}
	}
	if user != nil && a.userService.IsReseller(user) && !a.resellerClientOwnedInInbound(c, inbound, func(client model.Client) bool {
		return strings.EqualFold(strings.TrimSpace(client.Email), strings.TrimSpace(email))
	}) {
		pureJsonMsg(c, 403, false, "Access denied")
		return false
	}
	return true
}

func (a *InboundController) resellerOwnedEmails(c *gin.Context) map[string]struct{} {
	user := session.GetLoginUser(c)
	allowed := map[string]struct{}{}
	if user == nil {
		return allowed
	}
	inbounds, err := a.getAccessibleInbounds(c)
	if err != nil {
		return allowed
	}
	for _, inbound := range inbounds {
		clients, err := a.inboundService.GetClients(inbound)
		if err != nil {
			continue
		}
		for _, client := range clients {
			if clientBelongsToUser(client, user) {
				allowed[strings.ToLower(strings.TrimSpace(client.Email))] = struct{}{}
			}
		}
	}
	return allowed
}

func (a *InboundController) resellerClientOwnedInInbound(c *gin.Context, inbound *model.Inbound, match func(model.Client) bool) bool {
	user := session.GetLoginUser(c)
	if user == nil || !a.userService.IsReseller(user) || inbound == nil {
		return true
	}
	clients, err := a.inboundService.GetClients(inbound)
	if err != nil {
		return false
	}
	for _, client := range clients {
		if match(client) {
			return clientBelongsToUser(client, user)
		}
	}
	return false
}

func (a *InboundController) filterInboundForReseller(userID int, inbound *model.Inbound) error {
	if inbound == nil {
		return nil
	}
	clients, err := a.inboundService.GetClients(inbound)
	if err != nil {
		return err
	}
	user, err := a.userService.GetUserByID(userID)
	if err != nil {
		return err
	}
	filteredClients := make([]model.Client, 0, len(clients))
	allowedEmails := make(map[string]struct{}, len(clients))
	for _, client := range clients {
		if clientBelongsToUser(client, user) {
			client.Comment = service.StripResellerComment(client.Comment)
			filteredClients = append(filteredClients, client)
			allowedEmails[strings.ToLower(strings.TrimSpace(client.Email))] = struct{}{}
		}
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		return err
	}
	if settings == nil {
		return fmt.Errorf("inbound %d settings is not an object", inbound.Id)
	}
	settings["clients"] = filteredClients
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	inbound.Settings = string(body)
	filteredStats := make([]xray.ClientTraffic, 0, len(inbound.ClientStats))
	var up int64
	var down int64
	var allTime int64
	for _, stat := range inbound.ClientStats {
		if _, ok := allowedEmails[strings.ToLower(strings.TrimSpace(stat.Email))]; ok {
			filteredStats = append(filteredStats, stat)
			up += stat.Up
			down += stat.Down
			allTime += stat.AllTime
		}
	}
	inbound.ClientStats = filteredStats
	inbound.Up = up
	inbound.Down = down
	inbound.AllTime = allTime
	return nil
}

func (a *InboundController) applyResellerOwnership(c *gin.Context, inbound *model.Inbound) error {
	user := session.GetLoginUser(c)
	if user == nil || !a.userService.IsReseller(user) || inbound == nil {
		return nil
	}
	clients, err := a.inboundService.GetClients(inbound)
	if err != nil {
		return err
	}
	for i := range clients {
		clients[i].OwnerUserID = user.Id
		clients[i].Comment = service.UpsertResellerComment(clients[i].Comment, user.Username)
	}
	body, err := json.Marshal(map[string]any{"clients": clients})
	if err != nil {
		return err
	}
	inbound.Settings = string(body)
	return nil
}

// NewInboundController creates a new InboundController and sets up its routes.
func NewInboundController(g *gin.RouterGroup) *InboundController {
	a := &InboundController{}
	a.initRouter(g)
	return a
}

// initRouter initializes the routes for inbound-related operations.
func (a *InboundController) initRouter(g *gin.RouterGroup) {

	g.GET("/list", a.getInbounds)
	g.GET("/get/:id", a.getInbound)
	g.GET("/getClientTraffics/:email", a.getClientTraffics)
	g.GET("/getClientTrafficsById/:id", a.getClientTrafficsById)

	g.POST("/add", a.addInbound)
	g.POST("/del/:id", a.delInbound)
	g.POST("/update/:id", a.updateInbound)
	g.POST("/clientIps/:email", a.getClientIps)
	g.POST("/clearClientIps/:email", a.clearClientIps)
	g.POST("/addClient", a.addInboundClient)
	g.POST("/:id/delClient/:clientId", a.delInboundClient)
	g.POST("/updateClient/:clientId", a.updateInboundClient)
	g.POST("/:id/resetClientTraffic/:email", a.resetClientTraffic)
	g.POST("/resetAllTraffics", a.resetAllTraffics)
	g.POST("/resetAllClientTraffics/:id", a.resetAllClientTraffics)
	g.POST("/delDepletedClients/:id", a.delDepletedClients)
	g.POST("/import", a.importInbound)
	g.POST("/onlines", a.onlines)
	g.POST("/lastOnline", a.lastOnline)
	g.POST("/updateClientTraffic/:email", a.updateClientTraffic)
	g.POST("/:id/delClientByEmail/:email", a.delInboundClientByEmail)
}

// getInbounds retrieves the list of inbounds for the logged-in user.
func (a *InboundController) getInbounds(c *gin.Context) {
	user := session.GetLoginUser(c)
	inbounds, err := a.getAccessibleInbounds(c)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.obtain"), err)
		return
	}
	if a.userService.IsReseller(user) {
		for _, inbound := range inbounds {
			if err := a.filterInboundForReseller(user.Id, inbound); err != nil {
				jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.obtain"), err)
				return
			}
		}
	}
	jsonObj(c, inbounds, nil)
}

// getInbound retrieves a specific inbound by its ID.
func (a *InboundController) getInbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	inbound, ok := a.ensureInboundAccess(c, id)
	if !ok {
		return
	}
	user := session.GetLoginUser(c)
	if user != nil && a.userService.IsReseller(user) {
		if err := a.filterInboundForReseller(user.Id, inbound); err != nil {
			jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
			return
		}
	}
	jsonObj(c, inbound, nil)
}

// getClientTraffics retrieves client traffic information by email.
func (a *InboundController) getClientTraffics(c *gin.Context) {
	email := c.Param("email")
	if !a.ensureEmailAccess(c, email) {
		return
	}
	clientTraffics, err := a.inboundService.GetClientTrafficByEmail(email)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.trafficGetError"), err)
		return
	}
	jsonObj(c, clientTraffics, nil)
}

// getClientTrafficsById retrieves client traffic information by inbound ID.
func (a *InboundController) getClientTrafficsById(c *gin.Context) {
	id := c.Param("id")
	trafficID, convErr := strconv.Atoi(id)
	if convErr != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.trafficGetError"), convErr)
		return
	}
	_, inbound, err := a.inboundService.GetClientInboundByTrafficID(trafficID)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.trafficGetError"), err)
		return
	}
	user := session.GetLoginUser(c)
	if inbound != nil && user != nil && !a.userService.IsAdmin(user) {
		if !a.userService.CanResellerUseInbound(user, inbound) {
			pureJsonMsg(c, 403, false, "Access denied")
			return
		}
	}
	clientTraffics, err := a.inboundService.GetClientTrafficByID(id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.trafficGetError"), err)
		return
	}
	jsonObj(c, clientTraffics, nil)
}

// addInbound creates a new inbound configuration.
func (a *InboundController) addInbound(c *gin.Context) {
	if a.forbidResellerInboundConfig(c) {
		return
	}
	inbound := &model.Inbound{}
	err := c.ShouldBind(inbound)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundCreateSuccess"), err)
		return
	}
	user := session.GetLoginUser(c)
	inbound.UserId = user.Id
	if inbound.NodeID != nil && *inbound.NodeID <= 0 {
		inbound.NodeID = nil
	}
	if inbound.Listen == "" || inbound.Listen == "0.0.0.0" || inbound.Listen == "::" || inbound.Listen == "::0" {
		inbound.Tag = fmt.Sprintf("inbound-%v", inbound.Port)
	} else {
		inbound.Tag = fmt.Sprintf("inbound-%v:%v", inbound.Listen, inbound.Port)
	}

	inbound, needRestart, err := a.inboundService.AddInbound(inbound)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsgObj(c, I18nWeb(c, "pages.inbounds.toasts.inboundCreateSuccess"), inbound, nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
	websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
}

// delInbound deletes an inbound configuration by its ID.
func (a *InboundController) delInbound(c *gin.Context) {
	if a.forbidResellerInboundConfig(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundDeleteSuccess"), err)
		return
	}
	if _, ok := a.ensureInboundAccess(c, id); !ok {
		return
	}
	needRestart, err := a.inboundService.DelInbound(id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsgObj(c, I18nWeb(c, "pages.inbounds.toasts.inboundDeleteSuccess"), id, nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
	websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
}

// updateInbound updates an existing inbound configuration.
func (a *InboundController) updateInbound(c *gin.Context) {
	if a.forbidResellerInboundConfig(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), err)
		return
	}
	inbound := &model.Inbound{
		Id: id,
	}
	err = c.ShouldBind(inbound)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), err)
		return
	}
	if inbound.NodeID != nil && *inbound.NodeID <= 0 {
		inbound.NodeID = nil
	}
	if _, ok := a.ensureInboundAccess(c, id); !ok {
		return
	}
	inbound, needRestart, err := a.inboundService.UpdateInbound(inbound)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsgObj(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), inbound, nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
	websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
}

// getClientIps retrieves the IP addresses associated with a client by email.
func (a *InboundController) getClientIps(c *gin.Context) {
	email := c.Param("email")
	if !a.ensureEmailAccess(c, email) {
		return
	}

	ips, err := a.inboundService.GetInboundClientIps(email)
	if err != nil || ips == "" {
		jsonObj(c, "No IP Record", nil)
		return
	}

	// Prefer returning a normalized string list for consistent UI rendering
	type ipWithTimestamp struct {
		IP        string `json:"ip"`
		Timestamp int64  `json:"timestamp"`
	}

	var ipsWithTime []ipWithTimestamp
	if err := json.Unmarshal([]byte(ips), &ipsWithTime); err == nil && len(ipsWithTime) > 0 {
		formatted := make([]string, 0, len(ipsWithTime))
		for _, item := range ipsWithTime {
			if item.IP == "" {
				continue
			}
			if item.Timestamp > 0 {
				ts := time.Unix(item.Timestamp, 0).Local().Format("2006-01-02 15:04:05")
				formatted = append(formatted, fmt.Sprintf("%s (%s)", item.IP, ts))
				continue
			}
			formatted = append(formatted, item.IP)
		}
		jsonObj(c, formatted, nil)
		return
	}

	var oldIps []string
	if err := json.Unmarshal([]byte(ips), &oldIps); err == nil && len(oldIps) > 0 {
		jsonObj(c, oldIps, nil)
		return
	}

	// If parsing fails, return as string
	jsonObj(c, ips, nil)
}

// clearClientIps clears the IP addresses for a client by email.
func (a *InboundController) clearClientIps(c *gin.Context) {
	email := c.Param("email")
	if !a.ensureEmailAccess(c, email) {
		return
	}

	err := a.inboundService.ClearClientIps(email)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.updateSuccess"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.logCleanSuccess"), nil)
}

// addInboundClient adds a new client to an existing inbound.
func (a *InboundController) addInboundClient(c *gin.Context) {
	data := &addClientRequest{}
	err := c.ShouldBind(data)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), err)
		return
	}
	user := session.GetLoginUser(c)
	if err := a.applyResellerOwnership(c, &data.Inbound); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	if _, ok := a.ensureInboundAccess(c, data.Id); !ok {
		return
	}
	targetInbound, _ := a.inboundService.GetInbound(data.Id)
	if !a.resellerCanManageClients(c, targetInbound) {
		return
	}
	for _, inboundID := range data.MultiInboundIDs {
		if _, ok := a.ensureInboundAccess(c, inboundID); !ok {
			return
		}
		targetInbound, _ := a.inboundService.GetInbound(inboundID)
		if !a.resellerCanManageClients(c, targetInbound) {
			return
		}
	}
	if a.userService.IsReseller(user) {
		clients, parseErr := a.inboundService.GetClients(&data.Inbound)
		if parseErr != nil {
			jsonMsg(c, I18nWeb(c, "somethingWentWrong"), parseErr)
			return
		}
		currentClients, currentAssigned, _, quotaErr := a.userService.GetUserQuotaSnapshot(user.Id)
		if quotaErr != nil {
			jsonMsg(c, I18nWeb(c, "somethingWentWrong"), quotaErr)
			return
		}
		addedClients := len(clients)
		addedAssigned := int64(0)
		for _, client := range clients {
			if client.TotalGB <= 0 {
				jsonMsg(c, "Resellers cannot create unlimited clients", serviceErr("unlimited clients are not allowed for resellers"))
				return
			}
			addedAssigned += model.InitialClientTrafficQuota(client)
		}
		if user.MaxUsers > 0 && currentClients+addedClients > user.MaxUsers {
			jsonMsg(c, "Reseller client quota exceeded", serviceErr("max users exceeded"))
			return
		}
		if user.MaxTraffic > 0 && currentAssigned+addedAssigned > user.MaxTraffic {
			jsonMsg(c, "Reseller assigned traffic quota exceeded", serviceErr("max traffic exceeded"))
			return
		}
	}

	needRestart := false
	if data.MultiLink {
		needRestart, err = a.inboundService.AddMultiInboundClient(&data.Inbound, data.MultiInboundIDs)
	} else {
		needRestart, err = a.inboundService.AddInboundClient(&data.Inbound)
	}
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundClientAddSuccess"), nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
}

func serviceErr(msg string) error {
	return fmt.Errorf("%s", msg)
}

func remainingAssignedQuota(totalGB, usedTraffic int64) int64 {
	if totalGB <= 0 {
		return 0
	}
	remaining := totalGB - usedTraffic
	if remaining < 0 {
		return 0
	}
	return remaining
}

// delInboundClient deletes a client from an inbound by inbound ID and client ID.
func (a *InboundController) delInboundClient(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), err)
		return
	}
	if _, ok := a.ensureInboundAccess(c, id); !ok {
		return
	}
	clientId := c.Param("clientId")
	inbound, err := a.inboundService.GetInbound(id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	if !a.resellerClientOwnedInInbound(c, inbound, func(client model.Client) bool {
		return inboundClientMatches(inbound.Protocol, client, clientId)
	}) {
		pureJsonMsg(c, 403, false, "Access denied")
		return
	}

	needRestart, err := a.inboundService.DelInboundClient(id, clientId)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundClientDeleteSuccess"), nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
}

// updateInboundClient updates a client's configuration in an inbound.
func (a *InboundController) updateInboundClient(c *gin.Context) {
	clientId := c.Param("clientId")

	inbound := &model.Inbound{}
	err := c.ShouldBind(inbound)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), err)
		return
	}
	if _, ok := a.ensureInboundAccess(c, inbound.Id); !ok {
		return
	}
	if !a.resellerCanManageClients(c, inbound) {
		return
	}
	user := session.GetLoginUser(c)
	currentInbound, getErr := a.inboundService.GetInbound(inbound.Id)
	if getErr != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), getErr)
		return
	}
	if !a.resellerClientOwnedInInbound(c, currentInbound, func(client model.Client) bool {
		return inboundClientMatches(currentInbound.Protocol, client, clientId)
	}) {
		pureJsonMsg(c, 403, false, "Access denied")
		return
	}
	if err := a.applyResellerOwnership(c, inbound); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	if a.userService.IsReseller(user) {
		currentClients, getErr := a.inboundService.GetClients(currentInbound)
		if getErr != nil {
			jsonMsg(c, I18nWeb(c, "somethingWentWrong"), getErr)
			return
		}
		nextClients, getErr := a.inboundService.GetClients(inbound)
		if getErr != nil {
			jsonMsg(c, I18nWeb(c, "somethingWentWrong"), getErr)
			return
		}
		var oldAssigned int64
		var oldClient *model.Client
		var currentUsage int64
		for _, client := range currentClients {
			if inboundClientMatches(currentInbound.Protocol, client, clientId) && client.TotalGB > 0 {
				oldClient = &client
				oldAssigned = model.InitialClientTrafficQuota(client)
				for _, stat := range currentInbound.ClientStats {
					if strings.EqualFold(strings.TrimSpace(stat.Email), strings.TrimSpace(client.Email)) {
						currentUsage = stat.Up + stat.Down
						if stat.Total > 0 {
							oldAssigned = stat.Total
						}
						break
					}
				}
				break
			}
		}
		var newAssigned int64
		for _, client := range nextClients {
			if inboundClientMatches(inbound.Protocol, client, clientId) {
				if client.TotalGB <= 0 {
					jsonMsg(c, "Resellers cannot create unlimited clients", serviceErr("unlimited clients are not allowed for resellers"))
					return
				}
				newAssigned = model.InitialClientTrafficQuota(client)
				if oldClient != nil {
					newAssigned = model.UpdatedClientTrafficQuota(*oldClient, client, oldAssigned)
				}
				break
			}
		}
		_, currentAssigned, _, quotaErr := a.userService.GetUserQuotaSnapshot(user.Id)
		if quotaErr != nil {
			jsonMsg(c, I18nWeb(c, "somethingWentWrong"), quotaErr)
			return
		}
		nextAssigned := currentAssigned - remainingAssignedQuota(oldAssigned, currentUsage) + remainingAssignedQuota(newAssigned, currentUsage)
		if user.MaxTraffic > 0 && nextAssigned > user.MaxTraffic {
			jsonMsg(c, "Reseller assigned traffic quota exceeded", serviceErr("max traffic exceeded"))
			return
		}
	}

	needRestart, err := a.inboundService.UpdateInboundClient(inbound, clientId)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundClientUpdateSuccess"), nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
}

// resetClientTraffic resets the traffic counter for a specific client in an inbound.
func (a *InboundController) resetClientTraffic(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), err)
		return
	}
	if _, ok := a.ensureInboundAccess(c, id); !ok {
		return
	}
	email := c.Param("email")
	if !a.ensureEmailAccess(c, email) {
		return
	}

	needRestart, err := a.inboundService.ResetClientTraffic(id, email)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.resetInboundClientTrafficSuccess"), nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
}

// resetAllTraffics resets all traffic counters across all inbounds.
func (a *InboundController) resetAllTraffics(c *gin.Context) {
	if !a.userService.IsAdmin(session.GetLoginUser(c)) {
		pureJsonMsg(c, 403, false, "Access denied")
		return
	}
	err := a.inboundService.ResetAllTraffics()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	} else {
		a.xrayService.SetToNeedRestart()
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.resetAllTrafficSuccess"), nil)
}

// resetAllClientTraffics resets traffic counters for all clients in a specific inbound.
func (a *InboundController) resetAllClientTraffics(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), err)
		return
	}
	if _, ok := a.ensureInboundAccess(c, id); !ok {
		return
	}
	if a.userService.IsReseller(session.GetLoginUser(c)) {
		pureJsonMsg(c, 403, false, "Access denied")
		return
	}

	err = a.inboundService.ResetAllClientTraffics(id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	} else {
		a.xrayService.SetToNeedRestart()
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.resetAllClientTrafficSuccess"), nil)
}

// importInbound imports an inbound configuration from provided data.
func (a *InboundController) importInbound(c *gin.Context) {
	user := session.GetLoginUser(c)
	if a.userService.IsReseller(user) {
		pureJsonMsg(c, 403, false, "Access denied")
		return
	}
	inbound := &model.Inbound{}
	err := json.Unmarshal([]byte(c.PostForm("data")), inbound)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	inbound.Id = 0
	inbound.UserId = user.Id
	if inbound.Listen == "" || inbound.Listen == "0.0.0.0" || inbound.Listen == "::" || inbound.Listen == "::0" {
		inbound.Tag = fmt.Sprintf("inbound-%v", inbound.Port)
	} else {
		inbound.Tag = fmt.Sprintf("inbound-%v:%v", inbound.Listen, inbound.Port)
	}

	for index := range inbound.ClientStats {
		inbound.ClientStats[index].Id = 0
		inbound.ClientStats[index].Enable = true
	}

	needRestart := false
	inbound, needRestart, err = a.inboundService.AddInbound(inbound)
	jsonMsgObj(c, I18nWeb(c, "pages.inbounds.toasts.inboundCreateSuccess"), inbound, err)
	if err == nil && needRestart {
		a.xrayService.SetToNeedRestart()
	}
}

// delDepletedClients deletes clients in an inbound who have exhausted their traffic limits.
func (a *InboundController) delDepletedClients(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), err)
		return
	}
	if _, ok := a.ensureInboundAccess(c, id); !ok {
		return
	}
	if a.userService.IsReseller(session.GetLoginUser(c)) {
		pureJsonMsg(c, 403, false, "Access denied")
		return
	}
	err = a.inboundService.DelDepletedClients(id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.delDepletedClientsSuccess"), nil)
}

// onlines retrieves the list of currently online clients.
func (a *InboundController) onlines(c *gin.Context) {
	user := session.GetLoginUser(c)
	online := a.inboundService.GetOnlineClients()
	if user != nil && !a.userService.IsAdmin(user) {
		allowed := a.resellerOwnedEmails(c)
		filtered := make([]string, 0, len(online))
		for _, email := range online {
			if _, ok := allowed[strings.ToLower(strings.TrimSpace(email))]; ok {
				filtered = append(filtered, email)
			}
		}
		jsonObj(c, filtered, nil)
		return
	}
	jsonObj(c, online, nil)
}

// lastOnline retrieves the last online timestamps for clients.
func (a *InboundController) lastOnline(c *gin.Context) {
	data, err := a.inboundService.GetClientsLastOnline()
	user := session.GetLoginUser(c)
	if err == nil && user != nil && !a.userService.IsAdmin(user) {
		allowed := a.resellerOwnedEmails(c)
		filtered := map[string]int64{}
		for email, ts := range data {
			if _, ok := allowed[strings.ToLower(strings.TrimSpace(email))]; ok {
				filtered[email] = ts
			}
		}
		jsonObj(c, filtered, nil)
		return
	}
	jsonObj(c, data, err)
}

// updateClientTraffic updates the traffic statistics for a client by email.
func (a *InboundController) updateClientTraffic(c *gin.Context) {
	email := c.Param("email")
	if !a.ensureEmailAccess(c, email) {
		return
	}

	// Define the request structure for traffic update
	type TrafficUpdateRequest struct {
		Upload   int64 `json:"upload"`
		Download int64 `json:"download"`
	}

	var request TrafficUpdateRequest
	err := c.ShouldBindJSON(&request)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundUpdateSuccess"), err)
		return
	}

	err = a.inboundService.UpdateClientTrafficByEmail(email, request.Upload, request.Download)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}

	jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.inboundClientUpdateSuccess"), nil)
}

// delInboundClientByEmail deletes a client from an inbound by email address.
func (a *InboundController) delInboundClientByEmail(c *gin.Context) {
	inboundId, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "Invalid inbound ID", err)
		return
	}
	if _, ok := a.ensureInboundAccess(c, inboundId); !ok {
		return
	}

	email := c.Param("email")
	if !a.ensureEmailAccess(c, email) {
		return
	}
	needRestart, err := a.inboundService.DelInboundClientByEmail(inboundId, email)
	if err != nil {
		jsonMsg(c, "Failed to delete client by email", err)
		return
	}

	jsonMsg(c, "Client deleted successfully", nil)
	if needRestart {
		a.xrayService.SetToNeedRestart()
	}
}
