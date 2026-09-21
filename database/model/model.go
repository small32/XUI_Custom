package model

import (
	"fmt"
	"x-ui/util/json_util"
	"x-ui/xray"
)

type Protocol string

const (
	VMess       Protocol = "vmess"
	VLESS       Protocol = "vless"
	Dokodemo    Protocol = "Dokodemo-door"
	Http        Protocol = "http"
	Trojan      Protocol = "trojan"
	Shadowsocks Protocol = "shadowsocks"
	Socks       Protocol = "socks"
)

type User struct {
	Id       int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type Inbound struct {
	Id         int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	UserId     int    `json:"-"`
	Up         int64  `json:"up" form:"up"`
	Down       int64  `json:"down" form:"down"`
	Total      int64  `json:"total" form:"total"`
	Remark     string `json:"remark" form:"remark"`
	Enable     bool   `json:"enable" form:"enable"`
	ExpiryTime int64  `json:"expiryTime" form:"expiryTime"`
	// MonthlyReset 是“按月计算”开关：勾选后该账号进入月度周期，
	// 每月 1 日 00:00:01 清零已用流量并重新累计，清零前按月留档；
	// 不勾选则用量一直累计、不做月度清零，流量用完即止（Total 为 0 时同样不限量）。
	MonthlyReset bool `json:"monthlyReset" form:"monthlyReset" gorm:"column:monthly_reset"`

	// config part
	Listen         string   `json:"listen" form:"listen"`
	Port           int      `json:"port" form:"port" gorm:"unique"`
	Protocol       Protocol `json:"protocol" form:"protocol"`
	Settings       string   `json:"settings" form:"settings"`
	StreamSettings string   `json:"streamSettings" form:"streamSettings"`
	Tag            string   `json:"tag" form:"tag" gorm:"unique"`
	Sniffing       string   `json:"sniffing" form:"sniffing"`
}

func (i *Inbound) GenXrayInboundConfig() *xray.InboundConfig {
	listen := i.Listen
	if listen != "" {
		listen = fmt.Sprintf("\"%v\"", listen)
	}
	return &xray.InboundConfig{
		Listen:         json_util.RawMessage(listen),
		Port:           i.Port,
		Protocol:       string(i.Protocol),
		Settings:       json_util.RawMessage(i.Settings),
		StreamSettings: json_util.RawMessage(i.StreamSettings),
		Tag:            i.Tag,
		Sniffing:       json_util.RawMessage(i.Sniffing),
	}
}

type Setting struct {
	Id    int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Key   string `json:"key" form:"key"`
	Value string `json:"value" form:"value"`
}

// TrafficSnapshot 是账号月度流量留档，一行代表某账号某个月，
// 用 Yyyymm（形如 202609）区分是哪个月的流量。
//
// 所有账号共用这一张表，靠 InboundId 区分账号。(InboundId, Yyyymm) 上的复合唯一索引
// 既保证同一账号同月只有一行（清零重试不会写重），其最左列又直接支撑按账号查询，
// 不必再单独建一个 inbound_id 索引。
type TrafficSnapshot struct {
	Id        int    `json:"id" gorm:"primaryKey;autoIncrement"`
	InboundId int    `json:"inboundId" gorm:"column:inbound_id;uniqueIndex:idx_traffic_snapshot_account_month,priority:1"`
	Yyyymm    int    `json:"yyyymm" gorm:"column:yyyymm;uniqueIndex:idx_traffic_snapshot_account_month,priority:2"`
	Port      int    `json:"port" gorm:"column:port"`
	Remark    string `json:"remark" gorm:"column:remark"`
	LocalUp   int64  `json:"localUp" gorm:"column:local_up"`
	LocalDown int64  `json:"localDown" gorm:"column:local_down"`
	// 第三方服务器的同端口用量，远程未配置时为零值。
	RemoteUp   int64 `json:"remoteUp" gorm:"column:remote_up"`
	RemoteDown int64 `json:"remoteDown" gorm:"column:remote_down"`
	// Total 是套餐上限，清零时不会被改动，留档以便回溯当时的额度。
	Total   int64 `json:"total" gorm:"column:total"`
	ResetAt int64 `json:"resetAt" gorm:"column:reset_at"`
}

// TableName 固定单表名，所有账号的月度留档都落在这里。
func (TrafficSnapshot) TableName() string {
	return "traffic_snapshots"
}
