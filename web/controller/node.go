package controller

import (
	"context"
	"strconv"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

type NodeController struct {
	BaseController
	nodeService service.NodeService
}

func NewNodeController(g *gin.RouterGroup) *NodeController {
	a := &NodeController{}
	a.initRouter(g)
	return a
}

func (a *NodeController) initRouter(g *gin.RouterGroup) {
	g.GET("/list", a.list)
	g.GET("/get/:id", a.get)
	g.POST("/add", a.add)
	g.POST("/update/:id", a.update)
	g.POST("/del/:id", a.del)
	g.POST("/setEnable/:id", a.setEnable)
	g.POST("/test", a.test)
	g.POST("/probe/:id", a.probe)
	g.POST("/sync/:id", a.sync)
	g.POST("/syncAll", a.syncAll)
}

func (a *NodeController) list(c *gin.Context) {
	nodes, err := a.nodeService.GetAll()
	jsonObj(c, nodes, err)
}

func (a *NodeController) get(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	n, err := a.nodeService.GetById(id)
	jsonObj(c, n, err)
}

func (a *NodeController) add(c *gin.Context) {
	n := &model.Node{}
	if err := c.ShouldBind(n); err != nil {
		jsonMsg(c, "add node", err)
		return
	}
	if err := a.nodeService.Create(n); err != nil {
		jsonMsg(c, "add node", err)
		return
	}
	jsonMsgObj(c, "add node", n, nil)
}

func (a *NodeController) update(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	n := &model.Node{}
	if err := c.ShouldBind(n); err != nil {
		jsonMsg(c, "update node", err)
		return
	}
	if err := a.nodeService.Update(id, n); err != nil {
		jsonMsg(c, "update node", err)
		return
	}
	jsonMsg(c, "update node", nil)
}

func (a *NodeController) del(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	jsonMsg(c, "delete node", a.nodeService.Delete(id))
}

func (a *NodeController) setEnable(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	body := struct {
		Enable bool `json:"enable" form:"enable"`
	}{}
	if err := c.ShouldBind(&body); err != nil {
		jsonMsg(c, "set node enable", err)
		return
	}
	jsonMsg(c, "set node enable", a.nodeService.SetEnable(id, body.Enable))
}

func (a *NodeController) test(c *gin.Context) {
	n := &model.Node{}
	if err := c.ShouldBind(n); err != nil {
		jsonMsg(c, "test node", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
	defer cancel()
	patch, err := a.nodeService.Probe(ctx, n)
	jsonObj(c, patch.ToUI(err == nil), nil)
}

func (a *NodeController) probe(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	n, err := a.nodeService.GetById(id)
	if err != nil {
		jsonMsg(c, "get node", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
	defer cancel()
	patch, probeErr := a.nodeService.Probe(ctx, n)
	if probeErr != nil {
		patch.Status = "offline"
	} else {
		patch.Status = "online"
	}
	_ = a.nodeService.UpdateHeartbeat(id, patch)
	jsonObj(c, patch.ToUI(probeErr == nil), nil)
}

func (a *NodeController) sync(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	inbounds, err := a.nodeService.SyncNode(ctx, id)
	jsonObj(c, inbounds, err)
}

func (a *NodeController) syncAll(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	jsonMsg(c, "sync all nodes", a.nodeService.SyncAll(ctx))
}
