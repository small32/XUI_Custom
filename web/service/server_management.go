package service

import (
	"bytes"
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

func (s *ServerManagementService) SyncInbound(inbound *model.Inbound, create bool) error {
	return s.syncInbound(inbound, create, false)
}

func (s *ServerManagementService) DeleteSyncedInbound(inbound *model.Inbound) error {
	return s.syncInbound(inbound, false, true)
}

func (s *ServerManagementService) syncInbound(inbound *model.Inbound, create, remove bool) error {
	v, err := s.GetSetting()
	if err != nil {
		return err
	}
	if !v.SyncAccounts {
		return nil
	}
	if v.Host == "" {
		return nil
	}
	if v.Password == "" {
		return fmt.Errorf("未配置第三方服务器SSH密码")
	}
	if inbound.Port <= 0 {
		return fmt.Errorf("节点端口无效")
	}
	cfg := &ssh.ClientConfig{User: v.Username, Auth: []ssh.AuthMethod{ssh.Password(v.Password)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", v.Host, v.Port), cfg)
	if err != nil {
		return fmt.Errorf("SSH连接失败: %w", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sql := syncInboundSQL(inbound, create, v.SyncStrategy == "full")
	if remove {
		sql = deleteSyncedInboundSQL(inbound.Port)
	}
	sess.Stdin = strings.NewReader(sql)
	cmd := `test -f /etc/x-ui/x-ui.db || exit 1; changed=$(sqlite3 -bail /etc/x-ui/x-ui.db) || exit $?; if [ "$changed" = "1" ]; then systemctl restart x-ui; fi`
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Run(cmd); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("远程端口 %d 账号同步失败: %s: %w", inbound.Port, msg, err)
		}
		return fmt.Errorf("远程端口 %d 账号同步失败: %w", inbound.Port, err)
	}
	return nil
}

// A missing owner makes an inbound invisible to the remote panel. Preserve an
// existing valid owner; infer a new owner only when the remote panel has one user.
func syncInboundSQL(inbound *model.Inbound, create bool, full ...bool) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	enable := 0
	if inbound.Enable {
		enable = 1
	}
	owner := fmt.Sprintf("COALESCE((SELECT user_id FROM inbounds WHERE port=%d AND user_id IN (SELECT id FROM users)), (SELECT min(id) FROM users HAVING count(*)=1))", inbound.Port)
	stream := quote(inbound.StreamSettings)
	paths, _ := certificatePaths(inbound.StreamSettings)
	for _, path := range paths {
		stream = fmt.Sprintf("json_set(%s, '%s.certificateFile', (SELECT value FROM settings WHERE key='webCertFile'), '%s.keyFile', (SELECT value FROM settings WHERE key='webKeyFile'))", stream, path, path)
	}
	upsert := !create && len(full) > 0 && full[0]
	guard := fmt.Sprintf(" WHERE EXISTS (SELECT 1 FROM inbounds WHERE port=%d)", inbound.Port)
	var statement string
	if create || upsert {
		guard = ""
		statement = fmt.Sprintf("INSERT INTO inbounds (user_id,port,protocol,settings,stream_settings,tag,sniffing,remark,listen,enable,expiry_time,total,up,down) VALUES ((SELECT id FROM sync_owner),%d,%s,%s,%s,%s,%s,%s,%s,%d,%d,%d,0,0);", inbound.Port, quote(string(inbound.Protocol)), quote(inbound.Settings), stream, quote(fmt.Sprintf("inbound-%d", inbound.Port)), quote(inbound.Sniffing), quote(inbound.Remark), quote(inbound.Listen), enable, inbound.ExpiryTime, inbound.Total)
		if upsert {
			statement = strings.TrimSuffix(statement, ";") + ` ON CONFLICT(port) DO UPDATE SET user_id=excluded.user_id,protocol=excluded.protocol,settings=excluded.settings,stream_settings=excluded.stream_settings,tag=excluded.tag,sniffing=excluded.sniffing,remark=excluded.remark,listen=excluded.listen,enable=excluded.enable,expiry_time=excluded.expiry_time,total=excluded.total;`
		}

	} else {
		statement = fmt.Sprintf("UPDATE inbounds SET user_id=(SELECT id FROM sync_owner),protocol=%s,settings=%s,stream_settings=%s,tag=%s,sniffing=%s,remark=%s,listen=%s,enable=%d,expiry_time=%d,total=%d WHERE port=%d;", quote(string(inbound.Protocol)), quote(inbound.Settings), stream, quote(fmt.Sprintf("inbound-%d", inbound.Port)), quote(inbound.Sniffing), quote(inbound.Remark), quote(inbound.Listen), enable, inbound.ExpiryTime, inbound.Total, inbound.Port)
	}
	validation := ""
	if len(paths) > 0 {
		validation = "CREATE TEMP TABLE sync_cert (cert TEXT NOT NULL CHECK(length(cert)>0), key TEXT NOT NULL CHECK(length(key)>0)); INSERT INTO sync_cert SELECT (SELECT value FROM settings WHERE key='webCertFile'),(SELECT value FROM settings WHERE key='webKeyFile')" + guard + ";"
	}
	return fmt.Sprintf(`.timeout 10000
BEGIN IMMEDIATE;
CREATE TEMP TABLE sync_owner (id INTEGER NOT NULL);
INSERT INTO sync_owner SELECT %s%s;
%s
%s
SELECT changes();
COMMIT;
`, owner, guard, validation, statement)
}

func deleteSyncedInboundSQL(port int) string {
	return fmt.Sprintf(".timeout 10000\nBEGIN IMMEDIATE;\nDELETE FROM inbounds WHERE port=%d;\nSELECT changes();\nCOMMIT;\n", port)
}

func (s *ServerManagementService) RemoteInbound(port int) (map[string]interface{}, error) {
	v, err := s.GetSetting()
	if err != nil {
		return nil, err
	}
	if !v.SyncAccounts || v.Host == "" {
		return nil, nil
	}
	if v.Password == "" {
		return nil, fmt.Errorf("未配置第三方服务器SSH密码")
	}
	cfg := &ssh.ClientConfig{User: v.Username, Auth: []ssh.AuthMethod{ssh.Password(v.Password)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", v.Host, v.Port), cfg)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(remoteInboundSQL(port))
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	out, err := sess.Output("test -f /etc/x-ui/x-ui.db && sqlite3 -batch -noheader -bail /etc/x-ui/x-ui.db")
	if err != nil {
		return nil, fmt.Errorf("查询远程端口 %d 失败: %w: %s", port, err, strings.TrimSpace(stderr.String()))
	}
	node, err := parseRemoteInbound(out, port, v.Host)
	if err != nil {
		return nil, err
	}
	node["remoteName"] = v.Name
	return node, nil
}

func remoteInboundSQL(port int) string {
	return fmt.Sprintf("SELECT json_object('protocol',protocol,'settings',settings,'streamSettings',stream_settings,'sniffing',sniffing,'remark',remark,'port',port) FROM inbounds WHERE port=%d;", port)
}

func parseRemoteInbound(out []byte, port int, host string) (map[string]interface{}, error) {
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, fmt.Errorf("第三方服务器不存在端口 %d", port)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("远程端口 %d 返回数据解析失败: %w", port, err)
	}
	if result == nil || result["port"] != float64(port) {
		return nil, fmt.Errorf("远程端口 %d 返回数据不匹配", port)
	}
	result["port"] = port
	result["remoteAddress"] = host
	return result, nil
}

const serverManagementSettingKey = "serverManagement"

type ServerManagementService struct{}

func (s *ServerManagementService) Summary() ([]*entity.TrafficSummary, error) {
	remote, err := s.GetTrafficCache()
	if err != nil {
		return nil, err
	}
	by := map[int]*entity.ServerTraffic{}
	for _, v := range remote {
		by[v.Port] = v
	}
	var local []model.Inbound
	if err = database.GetDB().Find(&local).Error; err != nil {
		return nil, err
	}
	out := make([]*entity.TrafficSummary, 0, len(local))
	for _, in := range local {
		r := by[in.Port]
		var ru int64
		re := true
		if r != nil {
			ru = r.Used
			re = r.Enable
		}
		lu := in.Up + in.Down
		out = append(out, &entity.TrafficSummary{Username: in.Remark, Port: in.Port, Local: lu, Remote: ru, Total: lu + ru, Limit: in.Total, Enable: in.Enable && re})
	}
	return out, nil
}

const trafficCacheKey = "serverTrafficCache"

func (s *ServerManagementService) SaveTrafficCache(v []*entity.ServerTraffic) error {
	b, _ := json.Marshal(v)
	db := database.GetDB()
	row := &model.Setting{}
	err := db.Where("key = ?", trafficCacheKey).First(row).Error
	if database.IsNotFound(err) {
		return db.Create(&model.Setting{Key: trafficCacheKey, Value: string(b)}).Error
	}
	if err != nil {
		return err
	}
	row.Value = string(b)
	return db.Save(row).Error
}
func (s *ServerManagementService) GetTrafficCache() ([]*entity.ServerTraffic, error) {
	row := &model.Setting{}
	err := database.GetDB().Where("key = ?", trafficCacheKey).First(row).Error
	if database.IsNotFound(err) {
		return []*entity.ServerTraffic{}, nil
	}
	if err != nil {
		return nil, err
	}
	var v []*entity.ServerTraffic
	if err = json.Unmarshal([]byte(row.Value), &v); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *ServerManagementService) GetSetting() (*entity.ServerSetting, error) {
	v := entity.ServerSetting{Name: "第三方服务器", SyncStrategy: "normal"}
	row := &model.Setting{}
	err := database.GetDB().Where("key = ?", serverManagementSettingKey).First(row).Error
	if database.IsNotFound(err) {
		return &entity.ServerSetting{Port: 22, Name: "第三方服务器", SyncStrategy: "normal"}, nil
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
	if v.SyncStrategy == "" {
		v.SyncStrategy = "normal"
	}
	if v.SyncStrategy != "normal" && v.SyncStrategy != "full" {
		return fmt.Errorf("同步策略无效")
	}
	if strings.TrimSpace(v.Host) == "" || strings.TrimSpace(v.Username) == "" {
		return fmt.Errorf("服务器地址和用户名不能为空")
	}
	if v.Port < 1 || v.Port > 65535 {
		return fmt.Errorf("SSH端口无效")
	}
	if v.HeartbeatMinutes <= 0 {
		v.HeartbeatMinutes = 10
	}
	if v.HeartbeatMinutes < 10 {
		return fmt.Errorf("心跳间隔不能低于10分钟，10分钟可以使用")
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
		var localInbounds []model.Inbound
		if err := database.GetDB().Find(&localInbounds).Error; err != nil {
			return nil, err
		}
		localByPort := map[int]model.Inbound{}
		for _, in := range localInbounds {
			localByPort[in.Port] = in
		}
		changed := false
		for _, item := range result {
			local, exists := localByPort[item.Port]
			if !exists || local.Total <= 0 || local.Up+local.Down+item.Used < local.Total {
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
