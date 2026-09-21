package controller

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"strconv"
	"x-ui/database/model"
	"x-ui/logger"
	"x-ui/web/global"
	"x-ui/web/service"
	"x-ui/web/session"
)

type InboundController struct {
	inboundService service.InboundService
	xrayService    service.XrayService
	serverService  service.ServerManagementService
	settingService service.SettingService
}

func NewInboundController(g *gin.RouterGroup) *InboundController {
	a := &InboundController{}
	a.initRouter(g)
	a.startTask()
	return a
}

func (a *InboundController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/inbound")

	g.POST("/list", a.getInbounds)
	g.POST("/add", a.addInbound)
	g.POST("/del/:id", a.delInbound)
	g.POST("/update/:id", a.updateInbound)
	g.POST("/subscription", a.restrictedSubscription)
}

func (a *InboundController) startTask() {
	webServer := global.GetWebServer()
	c := webServer.GetCron()
	c.AddFunc("@every 10s", func() {
		if a.xrayService.IsNeedRestartAndSetFalse() {
			err := a.xrayService.RestartXray(false)
			if err != nil {
				logger.Error("restart xray failed:", err)
			}
		}
	})
}

func (a *InboundController) getInbounds(c *gin.Context) {
	// 受限登录仅返回绑定的那一条入站
	if inboundId := session.GetLoginInboundId(c); inboundId > 0 {
		password, ok := session.GetLoginPassword(c)
		if !ok {
			pureJsonMsg(c, false, "登录信息已过期，请退出后重新登录")
			return
		}
		inbound, err := a.inboundService.GetInbound(inboundId)
		if err != nil {
			jsonMsg(c, "获取", err)
			return
		}
		inbound.Settings, err = service.WithLoginPassword(inbound.Protocol, inbound.Settings, password)
		if err != nil {
			jsonMsg(c, "获取", err)
			return
		}
		jsonObj(c, []*model.Inbound{inbound}, nil)
		return
	}
	user := session.GetLoginUser(c)
	inbounds, err := a.inboundService.GetInbounds(user.Id)
	if err != nil {
		jsonMsg(c, "获取", err)
		return
	}
	jsonObj(c, inbounds, nil)
}

// restrictedSubscription 受限登录账号获取"生成订阅"所需的只读数据，
// 替代仅管理员可用的 /xui/setting/all 与 /xui/server/inbound/:port。
func (a *InboundController) restrictedSubscription(c *gin.Context) {
	password, ok := session.GetLoginPassword(c)
	if !ok {
		pureJsonMsg(c, false, "登录信息已过期，请退出后重新登录")
		return
	}
	inboundId := session.GetLoginInboundId(c)
	if inboundId <= 0 {
		pureJsonMsg(c, false, "无权访问")
		return
	}
	inbound, err := a.inboundService.GetInbound(inboundId)
	if err != nil {
		jsonMsg(c, "获取", err)
		return
	}
	serverName := ""
	if allSetting, e := a.settingService.GetAllSetting(); e == nil && allSetting != nil {
		serverName = allSetting.ServerName
	}
	var remoteInbound interface{}
	v, e := a.serverService.RemoteInbound(inbound.Port)
	if e != nil {
		jsonMsg(c, "获取订阅", e)
		return
	}
	if v != nil {
		protocol, _ := v["protocol"].(string)
		settings, _ := v["settings"].(string)
		masked, err := service.WithLoginPassword(model.Protocol(protocol), settings, password)
		if err != nil {
			jsonMsg(c, "获取订阅", err)
			return
		}
		v["settings"] = masked
		remoteInbound = v
	}
	jsonObj(c, gin.H{
		"serverName":    serverName,
		"remoteInbound": remoteInbound,
	}, nil)
}

func (a *InboundController) addInbound(c *gin.Context) {
	inbound := &model.Inbound{}
	err := c.ShouldBind(inbound)
	if err != nil {
		jsonMsg(c, "添加", err)
		return
	}
	user := session.GetLoginUser(c)
	inbound.UserId = user.Id
	inbound.Enable = true
	inbound.Tag = fmt.Sprintf("inbound-%v", inbound.Port)
	if err = a.inboundService.ApplyPanelCertificates(inbound); err != nil {
		jsonMsg(c, "添加", err)
		return
	}
	err = a.inboundService.AddInbound(inbound)
	if err == nil {
		if syncErr := a.serverService.SyncInbound(inbound, true); syncErr != nil {
			logger.Warning("第三方账号同步失败: ", syncErr)
			err = fmt.Errorf("本地账号已创建，但第三方同步失败: %w", syncErr)
		}
		a.xrayService.SetToNeedRestart()
	}
	jsonMsg(c, "添加", err)
}

func (a *InboundController) delInbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "删除", err)
		return
	}
	inbound, err := a.inboundService.GetInbound(id)
	if err != nil {
		jsonMsg(c, "删除", err)
		return
	}
	err = a.inboundService.DelInbound(id)
	if err == nil {
		a.xrayService.SetToNeedRestart()
		if syncErr := a.serverService.DeleteSyncedInbound(inbound); syncErr != nil {
			err = fmt.Errorf("本地账号已删除，但第三方同步删除失败: %w", syncErr)
		}
	}
	jsonMsg(c, "删除", err)
}

func (a *InboundController) updateInbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "修改", err)
		return
	}
	inbound := &model.Inbound{
		Id: id,
	}
	err = c.ShouldBind(inbound)
	if err != nil {
		jsonMsg(c, "修改", err)
		return
	}
	if err = a.inboundService.ApplyPanelCertificates(inbound); err != nil {
		jsonMsg(c, "修改", err)
		return
	}
	err = a.inboundService.UpdateInbound(inbound)
	if err == nil {
		if syncErr := a.serverService.SyncInbound(inbound, false); syncErr != nil {
			logger.Warning("第三方账号同步失败: ", syncErr)
			err = fmt.Errorf("本地账号已修改，但第三方同步失败: %w", syncErr)
		}
		a.xrayService.SetToNeedRestart()
	}
	jsonMsg(c, "修改", err)
}
