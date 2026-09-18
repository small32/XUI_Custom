package entity

type ServerSetting struct {
	Name             string `json:"name" form:"name"`
	SyncStrategy     string `json:"syncStrategy" form:"syncStrategy"`
	Host             string `json:"host" form:"host"`
	Port             int    `json:"port" form:"port"`
	Username         string `json:"username" form:"username"`
	Password         string `json:"password,omitempty" form:"password"`
	AutoDisable      bool   `json:"autoDisable" form:"autoDisable"`
	SyncAccounts     bool   `json:"syncAccounts" form:"syncAccounts"`
	HeartbeatMinutes int    `json:"heartbeatMinutes" form:"heartbeatMinutes"`
}

type ServerTraffic struct {
	Port   int   `json:"port"`
	Up     int64 `json:"up"`
	Down   int64 `json:"down"`
	Used   int64 `json:"used"`
	Total  int64 `json:"total"`
	Enable bool  `json:"enable"`
}
type TrafficSummary struct {
	Username string `json:"username"`
	Port     int    `json:"port"`
	Local    int64  `json:"local"`
	Remote   int64  `json:"remote"`
	Total    int64  `json:"total"`
	Limit    int64  `json:"limit"`
	Enable   bool   `json:"enable"`
}
