package session

import (
	"encoding/gob"
	"fmt"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"x-ui/database/model"
)

const (
	loginUser      = "LOGIN_USER"
	loginInboundId = "LOGIN_INBOUND_ID"
	loginPassword  = "LOGIN_INBOUND_PASSWORD"
)

func init() {
	gob.Register(model.User{})
}

func SetLoginUser(c *gin.Context, user *model.User) error {
	s := sessions.Default(c)
	s.Set(loginUser, user)
	return s.Save()
}

func GetLoginUser(c *gin.Context) *model.User {
	s := sessions.Default(c)
	obj := s.Get(loginUser)
	if obj == nil {
		return nil
	}
	user := obj.(model.User)
	return &user
}

func IsLogin(c *gin.Context) bool {
	return GetLoginUser(c) != nil
}

// SetLoginInboundId 记录受限登录绑定的入站 id
func SetLoginInboundId(c *gin.Context, inboundId int) error {
	s := sessions.Default(c)
	s.Set(loginInboundId, inboundId)
	return s.Save()
}

// GetLoginInboundId 取受限登录绑定的入站 id，非受限登录返回 0
func GetLoginInboundId(c *gin.Context) int {
	s := sessions.Default(c)
	obj := s.Get(loginInboundId)
	if obj == nil {
		return 0
	}
	inboundId, ok := obj.(int)
	if !ok {
		return 0
	}
	return inboundId
}

// IsRestricted 是否为受限登录（仅能查看绑定入站）
func IsRestricted(c *gin.Context) bool {
	return GetLoginInboundId(c) > 0
}

// SetRestrictedLogin 建立受限登录会话，密码校验已在 service 层完成
func SetRestrictedLogin(c *gin.Context, inboundId int, password string) error {
	// 写入占位用户，使 IsLogin 判定通过；其 Id 为 0 不会命中任何真实入站
	placeholder := &model.User{
		Id:       0,
		Username: fmt.Sprintf("inbound-%d", inboundId),
		Password: "",
	}
	s := sessions.Default(c)
	s.Set(loginUser, placeholder)
	s.Set(loginInboundId, inboundId)
	s.Set(loginPassword, password)
	return s.Save()
}

func GetLoginPassword(c *gin.Context) (string, bool) {
	password, ok := sessions.Default(c).Get(loginPassword).(string)
	return password, ok && password != ""
}

func ClearSession(c *gin.Context) {
	s := sessions.Default(c)
	s.Clear()
	s.Options(sessions.Options{
		Path:   "/",
		MaxAge: -1,
	})
	s.Save()
}
