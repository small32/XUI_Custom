package service

import (
	"encoding/json"
	"fmt"
	"time"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/util/common"
	"x-ui/xray"

	"gorm.io/gorm"
)

type InboundService struct {
}

func (s *InboundService) GetInbounds(userId int) ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Where("user_id = ?", userId).Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	return inbounds, nil
}

func (s *InboundService) GetAllInbounds() ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	return inbounds, nil
}

func (s *InboundService) checkPortExist(port int, ignoreId int) (bool, error) {
	db := database.GetDB()
	db = db.Model(model.Inbound{}).Where("port = ?", port)
	if ignoreId > 0 {
		db = db.Where("id != ?", ignoreId)
	}
	var count int64
	err := db.Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *InboundService) AddInbound(inbound *model.Inbound) error {
	exist, err := s.checkPortExist(inbound.Port, 0)
	if err != nil {
		return err
	}
	if exist {
		return common.NewError("端口已存在:", inbound.Port)
	}
	db := database.GetDB()
	return db.Save(inbound).Error
}

func (s *InboundService) AddInbounds(inbounds []*model.Inbound) error {
	for _, inbound := range inbounds {
		exist, err := s.checkPortExist(inbound.Port, 0)
		if err != nil {
			return err
		}
		if exist {
			return common.NewError("端口已存在:", inbound.Port)
		}
	}

	db := database.GetDB()
	tx := db.Begin()
	var err error
	defer func() {
		if err == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}()

	for _, inbound := range inbounds {
		err = tx.Save(inbound).Error
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *InboundService) DelInbound(id int) error {
	db := database.GetDB()
	return db.Delete(model.Inbound{}, id).Error
}

func (s *InboundService) GetInbound(id int) (*model.Inbound, error) {
	db := database.GetDB()
	inbound := &model.Inbound{}
	err := db.Model(model.Inbound{}).First(inbound, id).Error
	if err != nil {
		return nil, err
	}
	return inbound, nil
}

// GetInboundByPort 按端口号取入站
func (s *InboundService) GetInboundByPort(port int) (*model.Inbound, error) {
	db := database.GetDB()
	inbound := &model.Inbound{}
	err := db.Model(model.Inbound{}).Where("port = ?", port).First(inbound).Error
	if err != nil {
		return nil, err
	}
	return inbound, nil
}

// CheckInboundCredential 校验受限登录凭据：账号为入站端口号，密码为入站密码。
// 仅支持 trojan / shadowsocks / socks / http 四种带密码的协议，其余返回 nil。
func (s *InboundService) CheckInboundCredential(port int, password string) *model.Inbound {
	if password == "" {
		return nil
	}
	inbound, err := s.GetInboundByPort(port)
	if err != nil || inbound == nil {
		return nil
	}
	expected := inboundPassword(inbound)
	if expected == "" || expected != password {
		return nil
	}
	return inbound
}

// inboundPassword 按协议从入站 settings 中取"密码"，口径与面板详细信息弹窗一致
func inboundPassword(inbound *model.Inbound) string {
	var settings map[string]interface{}
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		return ""
	}
	switch inbound.Protocol {
	case model.Trojan:
		// settings.clients[0].password
		return arrayFieldPassword(settings["clients"], "password")
	case model.Shadowsocks:
		if v, ok := settings["password"].(string); ok {
			return v
		}
	case model.Socks, model.Http:
		// settings.accounts[0].pass
		return arrayFieldPassword(settings["accounts"], "pass")
	}
	return ""
}

// arrayFieldPassword 取数组首个元素的指定字符串字段
func arrayFieldPassword(raw interface{}, field string) string {
	arr, ok := raw.([]interface{})
	if !ok || len(arr) == 0 {
		return ""
	}
	obj, ok := arr[0].(map[string]interface{})
	if !ok {
		return ""
	}
	if v, ok := obj[field].(string); ok {
		return v
	}
	return ""
}

func (s *InboundService) UpdateInbound(inbound *model.Inbound) error {
	exist, err := s.checkPortExist(inbound.Port, inbound.Id)
	if err != nil {
		return err
	}
	if exist {
		return common.NewError("端口已存在:", inbound.Port)
	}

	oldInbound, err := s.GetInbound(inbound.Id)
	if err != nil {
		return err
	}
	oldInbound.Up = inbound.Up
	oldInbound.Down = inbound.Down
	oldInbound.Total = inbound.Total
	oldInbound.Remark = inbound.Remark
	oldInbound.Enable = inbound.Enable
	oldInbound.ExpiryTime = inbound.ExpiryTime
	oldInbound.MonthlyReset = inbound.MonthlyReset
	oldInbound.Listen = inbound.Listen
	oldInbound.Port = inbound.Port
	oldInbound.Protocol = inbound.Protocol
	oldInbound.Settings = inbound.Settings
	oldInbound.StreamSettings = inbound.StreamSettings
	oldInbound.Sniffing = inbound.Sniffing
	oldInbound.Tag = fmt.Sprintf("inbound-%v", inbound.Port)

	db := database.GetDB()
	return db.Save(oldInbound).Error
}

func (s *InboundService) AddTraffic(traffics []*xray.Traffic) (err error) {
	if len(traffics) == 0 {
		return nil
	}
	db := database.GetDB()
	db = db.Model(model.Inbound{})
	tx := db.Begin()
	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()
	for _, traffic := range traffics {
		if traffic.IsInbound {
			err = tx.Where("tag = ?", traffic.Tag).
				UpdateColumn("up", gorm.Expr("up + ?", traffic.Up)).
				UpdateColumn("down", gorm.Expr("down + ?", traffic.Down)).
				Error
			if err != nil {
				return
			}
		}
	}
	return
}

// expiredAt 判断入站是否已过到期时间（毫秒时间戳，0 表示无限期）。
func expiredAt(in *model.Inbound, nowMs int64) bool {
	return in.ExpiryTime > 0 && in.ExpiryTime <= nowMs
}

// DisableInvalidInbounds 停用已超限或已过期的入站，返回被停用的条数。
//
// 超限按汇总用量判断：本地 up+down 加上第三方服务器同端口的用量，
// 与流量汇总页、三态筛选走同一套口径（TrafficOverlimit）。
// 未配置第三方服务器时缓存为空，等价于只看本地用量，与改造前一致。
//
// 这里不区分“按月计算”与否：两种计费都是用量达到 total 即停用，
// 区别只在月初是否恢复——按月账号由月度清零重新计数后恢复启用，
// 未按月的账号累计到底，停用后不会自动恢复（流量用完即止）。
func (s *InboundService) DisableInvalidInbounds() (int64, error) {
	db := database.GetDB()
	now := time.Now().Unix() * 1000
	remote, err := readTrafficCache(db)
	if err != nil {
		return 0, err
	}
	remoteUsed := make(map[int]int64, len(remote))
	for _, r := range remote {
		remoteUsed[r.Port] = r.Used
	}
	var enabled []model.Inbound
	if err = db.Where("enable = ?", true).Find(&enabled).Error; err != nil {
		return 0, err
	}
	ids := make([]int, 0)
	for i := range enabled {
		in := &enabled[i]
		if expiredAt(in, now) || TrafficOverlimit(in.Up+in.Down, remoteUsed[in.Port], in.Total) {
			ids = append(ids, in.Id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	result := db.Model(model.Inbound{}).Where("id in ?", ids).Update("enable", false)
	return result.RowsAffected, result.Error
}
