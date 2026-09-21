package controller

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"time"
	"x-ui/logger"
	"x-ui/web/job"
	"x-ui/web/service"
	"x-ui/web/session"

	"github.com/gin-gonic/gin"
)

type LoginForm struct {
	Username string `json:"username" form:"username"`
	Password string `json:"password" form:"password"`
}

type IndexController struct {
	BaseController

	userService    service.UserService
	inboundService service.InboundService
}

func NewIndexController(g *gin.RouterGroup) *IndexController {
	a := &IndexController{}
	a.initRouter(g)
	return a
}

func (a *IndexController) initRouter(g *gin.RouterGroup) {
	g.GET("/", a.index)
	g.POST("/login", a.login)
	g.GET("/logout", a.logout)
}

func (a *IndexController) index(c *gin.Context) {
	if session.IsLogin(c) {
		c.Redirect(http.StatusTemporaryRedirect, "xui/")
		return
	}
	html(c, "login.html", "登录", nil)
}

func (a *IndexController) login(c *gin.Context) {
	var form LoginForm
	err := c.ShouldBind(&form)
	if err != nil {
		pureJsonMsg(c, false, "数据格式错误")
		return
	}
	if form.Username == "" {
		pureJsonMsg(c, false, "请输入用户名")
		return
	}
	if form.Password == "" {
		pureJsonMsg(c, false, "请输入密码")
		return
	}
	timeStr := time.Now().Format("2006-01-02 15:04:05")

	// 先按面板管理员账号校验
	user := a.userService.CheckUser(form.Username, form.Password)
	if user != nil {
		logger.Infof("%s login success,Ip Address:%s\n", form.Username, getRemoteIp(c))
		job.NewStatsNotifyJob().UserLoginNotify(form.Username, getRemoteIp(c), timeStr, 1)
		err = session.SetLoginUser(c, user)
		logger.Info("user", user.Id, "login success")
		jsonMsg(c, "登录", err)
		return
	}

	// 管理员校验失败后，尝试受限登录：账号=入站端口号，密码=入站密码
	if port, convErr := strconv.Atoi(form.Username); convErr == nil {
		if inbound := a.inboundService.CheckInboundCredential(port, form.Password); inbound != nil {
			err = session.SetRestrictedLogin(c, inbound.Id, form.Password)
			if err == nil {
				logger.Infof("inbound %d restricted login success, Ip Address:%s\n", inbound.Id, getRemoteIp(c))
			}
			jsonMsg(c, "登录", err)
			return
		}
	}

	job.NewStatsNotifyJob().UserLoginNotify(form.Username, getRemoteIp(c), timeStr, 0)
	logger.Infof("wrong username or password: username=%q password_base64=%q", form.Username, base64.StdEncoding.EncodeToString([]byte(form.Password)))
	pureJsonMsg(c, false, "用户名或密码错误")
}

func (a *IndexController) logout(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user != nil {
		logger.Info("user", user.Id, "logout")
	}
	session.ClearSession(c)
	c.Redirect(http.StatusTemporaryRedirect, c.GetString("base_path"))
}
