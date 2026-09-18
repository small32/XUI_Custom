package service

import (
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/ssh"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/web/entity"
)

const serverManagementSettingKey = "serverManagement"

type ServerManagementService struct{}

func (s *ServerManagementService) GetSetting() (*entity.ServerSetting, error) {
	var v entity.ServerSetting
	row := &model.Setting{}
	err := database.GetDB().Where("key = ?", serverManagementSettingKey).First(row).Error
	if database.IsNotFound(err) {
		return &entity.ServerSetting{Port: 22}, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(row.Value), &v); err != nil {
		return nil, err
	}
	if v.Port == 0 {
		v.Port = 22
	}
	if v.HeartbeatMinutes == 0 {
		v.HeartbeatMinutes = 10
	}
	return &v, nil
}
func (s *ServerManagementService) SaveSetting(v *entity.ServerSetting) error {
	if strings.TrimSpace(v.Host) == "" || strings.TrimSpace(v.Username) == "" {
		return fmt.Errorf("服务器地址和用户名不能为空")
	}
	if v.Port < 1 || v.Port > 65535 {
		return fmt.Errorf("SSH端口无效")
	}
	if v.HeartbeatMinutes < 10 {
		return fmt.Errorf("心跳间隔不能少于10分钟")
	}
	old, err := s.GetSetting()
	if err != nil {
		return err
	}
	if v.Password == "" {
		v.Password = old.Password
	}
	b, _ := json.Marshal(v)
	db := database.GetDB()
	row := &model.Setting{}
	err = db.Where("key = ?", serverManagementSettingKey).First(row).Error
	if database.IsNotFound(err) {
		return db.Create(&model.Setting{Key: serverManagementSettingKey, Value: string(b)}).Error
	}
	if err != nil {
		return err
	}
	row.Value = string(b)
	return db.Save(row).Error
}
func (s *ServerManagementService) Traffic() ([]*entity.ServerTraffic, error) {
	v, err := s.GetSetting()
	if err != nil {
		return nil, err
	}
	if v.Host == "" || v.Password == "" {
		return nil, fmt.Errorf("请先配置第三方服务器及SSH密码")
	}
	cfg := &ssh.ClientConfig{User: v.Username, Auth: []ssh.AuthMethod{ssh.Password(v.Password)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", v.Host, v.Port), cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH连接失败: %w", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	out, err := sess.Output("sqlite3 -separator '\\t' /etc/x-ui/x-ui.db 'SELECT port, up, down, total, enable FROM inbounds ORDER BY port;'")
	if err != nil {
		return nil, fmt.Errorf("读取远程流量失败: %w", err)
	}
	result := make([]*entity.ServerTraffic, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			continue
		}
		port, e1 := strconv.Atoi(f[0])
		up, e2 := strconv.ParseInt(f[1], 10, 64)
		down, e3 := strconv.ParseInt(f[2], 10, 64)
		total, e4 := strconv.ParseInt(f[3], 10, 64)
		en, e5 := strconv.Atoi(f[4])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
			continue
		}
		result = append(result, &entity.ServerTraffic{Port: port, Up: up, Down: down, Used: up + down, Total: total, Enable: en == 1})
	}
	if v.AutoDisable {
		changed := false
		for _, item := range result {
			if item.Total <= 0 || item.Used < item.Total {
				continue
			}
			if err := s.disableLocal(item.Port); err != nil {
				return nil, fmt.Errorf("禁用本地端口 %d 失败: %w", item.Port, err)
			}
			cmd := fmt.Sprintf("sqlite3 /etc/x-ui/x-ui.db 'UPDATE inbounds SET enable = 0 WHERE port = %d;' && x-ui restart", item.Port)
			r, e := client.NewSession()
			if e != nil {
				return nil, e
			}
			e = r.Run(cmd)
			r.Close()
			if e != nil {
				return nil, fmt.Errorf("禁用远程端口 %d 失败: %w", item.Port, e)
			}
			item.Enable = false
			changed = true
		}
		if changed {
			if err := exec.Command("x-ui", "restart").Run(); err != nil {
				return nil, fmt.Errorf("重启本地面板失败: %w", err)
			}
		}
	}
	return result, nil
}

func (s *ServerManagementService) disableLocal(port int) error {
	return database.GetDB().Model(&model.Inbound{}).Where("port = ? AND enable = ?", port, true).Update("enable", false).Error
}
