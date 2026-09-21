package controller

import (
	"github.com/gin-gonic/gin"
	"strconv"
	"sync"
	"time"
	"x-ui/logger"
	"x-ui/web/entity"
	"x-ui/web/global"
	"x-ui/web/service"
	"x-ui/web/session"
)

// 流量月度清零的定时表达式。cron 已启用秒级解析，字段顺序为 秒 分 时 日 月 周，
// 因此 "1 0 0 1 * *" 即每月 1 日 00:00:01；兜底任务按间隔轮询，避免停机跨月漏做。
const (
	monthlyResetCronSpec     = "1 0 0 1 * *"
	monthlyResetFallbackSpec = "@every 5m"
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
	g.GET("/traffic-summary", a.summaryPage)
	g.POST("/traffic-summary/list", a.summary)
	g.POST("/traffic-summary/snapshots", a.resetSnapshots)
	g.POST("/server/inbound/:port", a.remoteInbound)
	g.POST("/server/pending-deletes", a.getPendingDeletes)
	cron := global.GetWebServer().GetCron()
	// 每月 1 日 00:00:01 清零流量；再加一条每 5 分钟的兜底，服务重启或停机跨月后不会漏做。
	if _, err := cron.AddFunc(monthlyResetCronSpec, a.monthlyReset); err != nil {
		logger.Warning("注册流量月度清零任务失败: ", err)
	}
	if _, err := cron.AddFunc(monthlyResetFallbackSpec, a.monthlyReset); err != nil {
		logger.Warning("注册流量月度清零兜底任务失败: ", err)
	}
	global.GetWebServer().GetCron().AddFunc("@every 1m", func() {
		if v, err := a.service.GetSetting(); err == nil && v.Host != "" {
			a.heartbeatMu.Lock()
			if !a.lastHeartbeat.IsZero() && time.Since(a.lastHeartbeat) < time.Duration(v.HeartbeatMinutes)*time.Minute {
				a.heartbeatMu.Unlock()
				return
			}
			a.lastHeartbeat = time.Now()
			a.heartbeatMu.Unlock()
			if _, e := a.service.Traffic(); e != nil {
				err = e
			}
			if err != nil {
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
func (a *ServerManagementController) summaryPage(c *gin.Context) {
	html(c, "traffic_summary.html", "流量汇总", nil)
}

// summary 返回流量汇总。受限登录只返回其绑定入站那一条，管理员返回全部。
func (a *ServerManagementController) summary(c *gin.Context) {
	v, err := a.service.Summary(session.GetLoginInboundId(c))
	if err != nil {
		jsonMsg(c, "获取流量汇总", err)
		return
	}
	jsonObj(c, v, nil)
}

// monthlyReset 由定时任务触发，跨月后清零流量并留档。失败只记日志，下一轮自动重试。
func (a *ServerManagementController) monthlyReset() {
	if err := a.service.MaybeMonthlyReset(); err != nil {
		logger.Warning("流量月度清零失败: ", err)
	}
}

// resetSnapshots 返回指定账号的月度流量留档（按 yyyymm 从最近往最前排序）。
// 请求体里的 inboundId 决定看哪个账号；受限登录一律以会话绑定的入站为准，
// 传别的 id 也会被覆盖，防止越权查看他人记录。
func (a *ServerManagementController) resetSnapshots(c *gin.Context) {
	var form struct {
		InboundId int `form:"inboundId"`
	}
	_ = c.ShouldBind(&form)
	if bound := session.GetLoginInboundId(c); bound > 0 {
		form.InboundId = bound
	}
	v, err := a.service.TrafficResetSnapshots(form.InboundId)
	jsonObj(c, v, err)
}
func (a *ServerManagementController) remoteInbound(c *gin.Context) {
	port, err := strconv.Atoi(c.Param("port"))
	if err != nil {
		jsonMsg(c, "读取远程节点", err)
		return
	}
	v, err := a.service.RemoteInbound(port)
	if err != nil {
		jsonMsg(c, "读取远程节点", err)
		return
	}
	jsonObj(c, v, nil)
}

// getPendingDeletes 返回需要同步删除的端口列表
func (a *ServerManagementController) getPendingDeletes(c *gin.Context) {
	jsonObj(c, a.service.GetPendingDeletes(), nil)
}
