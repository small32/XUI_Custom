package controller

import (
	"github.com/gin-gonic/gin"
	"sync"
	"time"
	"x-ui/logger"
	"x-ui/web/entity"
	"x-ui/web/global"
	"x-ui/web/service"
)

type ServerManagementController struct {
	service       service.ServerManagementService
	lastHeartbeat time.Time
	heartbeatMu   sync.Mutex
}

func NewServerManagementController(g *gin.RouterGroup) *ServerManagementController {
	a := &ServerManagementController{}
	g.GET("/server", a.page)
	g.POST("/server/setting", a.setting)
	g.POST("/server/setting/all", a.getSetting)
	g.POST("/server/traffic", a.traffic)
	global.GetWebServer().GetCron().AddFunc("@every 1m", func() {
		if v, err := a.service.GetSetting(); err == nil && v.Host != "" {
			a.heartbeatMu.Lock()
			if !a.lastHeartbeat.IsZero() && time.Since(a.lastHeartbeat) < time.Duration(v.HeartbeatMinutes)*time.Minute {
				a.heartbeatMu.Unlock()
				return
			}
			a.lastHeartbeat = time.Now()
			a.heartbeatMu.Unlock()
			if _, err = a.service.Traffic(); err != nil {
				logger.Warning("服务器流量审计失败: ", err)
			}
		}
	})
	return a
}
func (a *ServerManagementController) page(c *gin.Context) {
	html(c, "server.html", "服务器管理", nil)
}
func (a *ServerManagementController) setting(c *gin.Context) {
	var v entity.ServerSetting
	if err := c.ShouldBind(&v); err != nil {
		jsonMsg(c, "保存服务器设置", err)
		return
	}
	jsonMsg(c, "保存服务器设置", a.service.SaveSetting(&v))
}
func (a *ServerManagementController) getSetting(c *gin.Context) {
	v, err := a.service.GetSetting()
	if err != nil {
		jsonMsg(c, "获取服务器设置", err)
		return
	}
	v.Password = ""
	jsonObj(c, v, nil)
}
func (a *ServerManagementController) traffic(c *gin.Context) {
	v, err := a.service.Traffic()
	if err != nil {
		jsonMsg(c, "读取服务器流量", err)
		return
	}
	jsonObj(c, v, nil)
}
