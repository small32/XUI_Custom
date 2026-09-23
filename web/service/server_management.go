package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/logger"
	"x-ui/web/entity"
)

// SyncInbound 把入站同步到远端。create 为真时插入新账号；否则更新已有账号。
// oldPort 用于端口变更时的同步：改端口后必须把远端旧端口账号一并迁移/删除，
// 否则旧端口账号会残留并继续可用。新增或端口未变时传入 0 或当前端口即可。
func (s *ServerManagementService) SyncInbound(inbound *model.Inbound, oldPort int, create bool) error {
	return s.syncInbound(inbound, oldPort, create, false)
}

func (s *ServerManagementService) DeleteSyncedInbound(inbound *model.Inbound) error {
	err := s.syncInbound(inbound, 0, false, true)
	if err != nil {
		// 同步删除失败，记录端口到待处理队列，提示管理员手动处理
		if addErr := s.AddPendingDelete(inbound.Port); addErr != nil {
			logger.Warning("记录待删除端口失败: ", addErr)
		}
		return err
	}
	// 同步成功，清除该端口的待处理记录（如果存在）
	s.ClearPendingDelete(inbound.Port)
	return nil
}

func (s *ServerManagementService) syncInbound(inbound *model.Inbound, oldPort int, create, remove bool) error {
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
	if inbound.Port <= 0 {
		return fmt.Errorf("节点端口无效")
	}
	cfg, err := remoteSSHClientConfig(v)
	if err != nil {
		return err
	}
	client, err := dialRemoteSSH("tcp", fmt.Sprintf("%s:%d", v.Host, v.Port), cfg)
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
	// 端口变更：远端旧端口账号必须一并迁移，否则残留可用账号成为孤儿。
	if oldPort > 0 && oldPort != inbound.Port {
		sql = syncInboundSQLWithOldPort(inbound, oldPort, v.SyncStrategy == "full")
	}
	if remove {
		sql = deleteSyncedInboundSQL(inbound.Port)
	}
	sess.Stdin = strings.NewReader(withSyncReloadIntent(sql))
	cmd := `test -f /etc/x-ui/x-ui.db || exit 1; sqlite3 -bail /etc/x-ui/x-ui.db || exit $?; ` + syncReloadCommand
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

var serverStateMu sync.Mutex

// Bound the entire SSH exchange, including handshake, session creation and output.
func dialRemoteSSH(network, address string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := net.DialTimeout(network, address, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if err = conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		conn.Close()
		return nil, err
	}
	c, channels, requests, err := ssh.NewClientConn(conn, address, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, channels, requests), nil
}

func remoteSSHClientConfig(v *entity.ServerSetting) (*ssh.ClientConfig, error) {
	mode := v.AuthMode
	if mode == "" {
		mode = "password"
	}
	var auth ssh.AuthMethod
	switch mode {
	case "password":
		if v.Password == "" {
			return nil, fmt.Errorf("未配置第三方服务器SSH密码")
		}
		auth = ssh.Password(v.Password)
	case "privateKey":
		if strings.TrimSpace(v.PrivateKey) == "" {
			return nil, fmt.Errorf("未上传SSH私钥")
		}
		var signer ssh.Signer
		var err error
		if v.PrivateKeyPassword != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(v.PrivateKey), []byte(v.PrivateKeyPassword))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(v.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("SSH私钥无法读取，请确认是 OpenSSH/PEM 格式且口令正确（PuTTY PPK 请先转换）: %w", err)
		}
		auth = ssh.PublicKeys(signer)
	default:
		return nil, fmt.Errorf("SSH认证方式无效")
	}
	return &ssh.ClientConfig{
		User:            v.Username,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
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

// syncInboundSQLWithOldPort 处理端口变更时，优先原位迁移旧端口行以保留流量；
// 若旧端口不存在，则普通策略只更新已存在的新端口，完全同步才允许创建。
func syncInboundSQLWithOldPort(inbound *model.Inbound, oldPort int, full bool) string {
	// Move an existing row in place so its remote up/down counters survive the
	// port change. The normal policy leaves a missing remote port untouched;
	// full sync falls back to the usual upsert when the old port is absent.
	oldInbound := *inbound
	oldInbound.Port = oldPort
	updateScript := syncInboundSQL(&oldInbound, false)
	oldOwner := fmt.Sprintf("COALESCE((SELECT user_id FROM inbounds WHERE port=%d AND user_id IN (SELECT id FROM users)), (SELECT min(id) FROM users HAVING count(*)=1))", oldPort)
	owner := fmt.Sprintf("COALESCE((SELECT user_id FROM inbounds WHERE port=%d AND user_id IN (SELECT id FROM users)), (SELECT user_id FROM inbounds WHERE port=%d AND user_id IN (SELECT id FROM users)), (SELECT min(id) FROM users HAVING count(*)=1))", oldPort, inbound.Port)
	updateScript = strings.Replace(updateScript, oldOwner, owner, 1)
	ownerGuard := " WHERE EXISTS (SELECT 1 FROM inbounds WHERE port=" + strconv.Itoa(oldPort) + ") OR EXISTS (SELECT 1 FROM inbounds WHERE port=" + strconv.Itoa(inbound.Port) + ")"
	if full {
		ownerGuard += " OR 1=1"
	}
	updateScript = strings.Replace(updateScript,
		"INSERT INTO sync_owner SELECT "+owner+";",
		"INSERT INTO sync_owner SELECT "+owner+ownerGuard+";", 1)
	updateScript = strings.Replace(updateScript, ") WHERE EXISTS (SELECT 1 FROM inbounds WHERE port="+strconv.Itoa(oldPort)+");", ")"+ownerGuard+";", 1)
	updateScript = strings.Replace(updateScript, "UPDATE inbounds SET user_id=", "UPDATE inbounds SET port="+strconv.Itoa(inbound.Port)+",user_id=", 1)
	updateScript = strings.Replace(updateScript, "tag='inbound-"+strconv.Itoa(oldPort)+"'", "tag='inbound-"+strconv.Itoa(inbound.Port)+"'", 1)

	prefix := ".timeout 10000\nBEGIN IMMEDIATE;\n"
	suffix := "SELECT changes();\nCOMMIT;\n"
	body := strings.TrimSuffix(strings.TrimPrefix(updateScript, prefix), suffix)
	// If the old remote row is missing but the new port already exists, normal
	// synchronization must still update that matching port. Updating the row a
	// second time after a successful migration is harmless and keeps its counters.
	newPortScript := syncInboundSQL(inbound, false)
	if updateAt := strings.Index(newPortScript, "UPDATE inbounds SET "); updateAt >= 0 {
		if updateEnd := strings.Index(newPortScript[updateAt:], ";\n"); updateEnd >= 0 {
			body += newPortScript[updateAt:updateAt+updateEnd+1] + "\n"
		}
	}
	if full {
		fallback := syncInboundSQL(inbound, false, true)
		insertAt := strings.Index(fallback, "INSERT INTO inbounds ")
		if insertAt >= 0 {
			insertEnd := strings.Index(fallback[insertAt:], ";\n")
			if insertEnd >= 0 {
				body += fallback[insertAt:insertAt+insertEnd+1] + "\n"
			}
		}
	}
	return prefix + body + suffix
}

func (s *ServerManagementService) RemoteInbound(port int) (map[string]interface{}, error) {
	v, err := s.GetSetting()
	if err != nil {
		return nil, err
	}
	if !v.SyncAccounts || v.Host == "" {
		return nil, nil
	}
	cfg, err := remoteSSHClientConfig(v)
	if err != nil {
		return nil, err
	}
	client, err := dialRemoteSSH("tcp", fmt.Sprintf("%s:%d", v.Host, v.Port), cfg)
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

// Summary 汇总各入站的流量。inboundId > 0 时只汇总该入站（受限登录专用），
// 在查询阶段就限定范围，避免其他端口的数据进入内存或被带出去。
func (s *ServerManagementService) Summary(inboundId int) ([]*entity.TrafficSummary, error) {
	remote, err := s.GetTrafficCache()
	if err != nil {
		return nil, err
	}
	by := map[int]*entity.ServerTraffic{}
	for _, v := range remote {
		by[v.Port] = v
	}
	var local []model.Inbound
	query := database.GetDB()
	if inboundId > 0 {
		query = query.Where("id = ?", inboundId)
	}
	if err = query.Find(&local).Error; err != nil {
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
		enable := in.Enable && re
		overlimit := TrafficOverlimit(lu, ru, in.Total)
		out = append(out, &entity.TrafficSummary{
			InboundId:    in.Id,
			Username:     in.Remark,
			Port:         in.Port,
			Local:        lu,
			Remote:       ru,
			Total:        lu + ru,
			Limit:        in.Total,
			Enable:       enable,
			MonthlyReset: in.MonthlyReset,
			Overlimit:    overlimit,
			Status:       TrafficStatusOf(enable, overlimit),
			LocalText:    FormatTrafficSize(lu),
			RemoteText:   FormatTrafficSize(ru),
			TotalText:    FormatTrafficSize(lu + ru),
			LimitText:    FormatTrafficLimit(in.Total),
		})
	}
	return out, nil
}

// forceHeartbeat 强制执行一次远端流量心跳（拉取远程流量、审计禁用并刷新缓存）。
// 独立成变量是为了在测试中替换，避免真实 SSH 连接。定时心跳的节流状态
// （lastHeartbeat）保存在 controller 层，这里不触碰，因此不影响定时心跳节奏。
var forceHeartbeat = func() {
	if _, err := new(ServerManagementService).Traffic(); err != nil {
		logger.Warning("删除账号后的强制流量心跳失败: ", err)
	}
}

// ResetPortTrafficCache 删除账号后调用：清掉本地缓存里该端口的远程流量条目，
// 并强制触发一次心跳。否则端口被新账号复用时，会继承旧账号的缓存用量，
// 在下一次心跳前被自动禁用（新账号上传、下载均为 0 仍被停用）。
//
// 缓存清理同步完成并持有 serverStateMu，与 Traffic() 的写回串行，防止并发
// 心跳把旧条目又写回来；心跳本身异步执行，SSH 慢时不会拖住删除请求。
func (s *ServerManagementService) ResetPortTrafficCache(port int) {
	s.ResetPortsTrafficCache(port)
}

func (s *ServerManagementService) ResetPortsTrafficCache(ports ...int) {
	portSet := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		portSet[port] = struct{}{}
	}
	serverStateMu.Lock()
	cache, err := s.GetTrafficCache()
	if err == nil && len(cache) > 0 {
		kept := make([]*entity.ServerTraffic, 0, len(cache))
		changed := false
		for _, v := range cache {
			if _, reset := portSet[v.Port]; reset {
				changed = true
				continue
			}
			kept = append(kept, v)
		}
		if changed {
			err = s.SaveTrafficCache(kept)
		}
	}
	serverStateMu.Unlock()
	if err != nil {
		logger.Warning("删除账号后清理端口流量缓存失败: ", err)
	}
	go forceHeartbeat()
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
	if v.AuthMode == "" {
		v.AuthMode = "password"
	}
	v.PrivateKeyConfigured = strings.TrimSpace(v.PrivateKey) != ""
	return &v, nil
}
func (s *ServerManagementService) SaveSetting(v *entity.ServerSetting) error {
	serverStateMu.Lock()
	defer serverStateMu.Unlock()
	return s.saveSettingLocked(v)
}

// saveSettingLocked 保存服务器设置，调用方必须已持有 serverStateMu。
// 内部校验与落库逻辑与 SaveSetting 一致，供需要复用已持锁上下文的内部方法使用，
// 避免在同一 goroutine 中对不可重入的 serverStateMu 二次加锁造成死锁。
func (s *ServerManagementService) saveSettingLocked(v *entity.ServerSetting) error {
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
	if v.PrivateKey == "" {
		v.PrivateKey = old.PrivateKey
	}
	if v.PrivateKeyPassword == "" && !v.PrivateKeyPasswordChanged {
		v.PrivateKeyPassword = old.PrivateKeyPassword
	}
	v.PrivateKeyPasswordChanged = false
	if v.AuthMode == "" {
		v.AuthMode = "password"
	}
	if v.AuthMode == "privateKey" {
		if _, err := remoteSSHClientConfig(v); err != nil {
			return err
		}
	} else if v.AuthMode != "password" {
		return fmt.Errorf("SSH认证方式无效")
	}
	v.PrivateKeyConfigured = false
	b, _ := json.Marshal(v)
	return database.GetDB().Transaction(func(db *gorm.DB) error {
		row := &model.Setting{}
		err := db.Where("key = ?", serverManagementSettingKey).First(row).Error
		if database.IsNotFound(err) {
			if err = db.Create(&model.Setting{Key: serverManagementSettingKey, Value: string(b)}).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			row.Value = string(b)
			if err = db.Save(row).Error; err != nil {
				return err
			}
		}
		if old.Host != v.Host || old.Port != v.Port || old.Username != v.Username {
			if err := db.Where("key = ?", trafficCacheKey).Delete(&model.Setting{}).Error; err != nil {
				return err
			}
			st, err := readTrafficResetState(db)
			if err != nil {
				return err
			}
			if st.LocalDone && !st.RemoteDone {
				st.RemoteDone = true
				if err := saveTrafficResetStateTx(db, st); err != nil {
					return err
				}
			}
			return nil
		}
		return nil
	})
}

// AddPendingDelete 将端口添加到待删除队列，用于同步失败时的手动处理提醒。
func (s *ServerManagementService) AddPendingDelete(port int) error {
	serverStateMu.Lock()
	defer serverStateMu.Unlock()
	v, err := s.GetSetting()
	if err != nil {
		return err
	}
	// 检查端口是否已存在
	for _, p := range v.PendingDeletes {
		if p == port {
			return nil
		}
	}
	v.PendingDeletes = append(v.PendingDeletes, port)
	return s.saveSettingLocked(v)
}

// ClearPendingDelete 从待删除队列中移除指定端口。
func (s *ServerManagementService) ClearPendingDelete(port int) {
	serverStateMu.Lock()
	defer serverStateMu.Unlock()
	v, err := s.GetSetting()
	if err != nil {
		return
	}
	filtered := make([]int, 0, len(v.PendingDeletes))
	for _, p := range v.PendingDeletes {
		if p != port {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) != len(v.PendingDeletes) {
		v.PendingDeletes = filtered
		s.saveSettingLocked(v)
	}
}

// GetPendingDeletes 返回需要手动处理的待删除端口列表。
func (s *ServerManagementService) GetPendingDeletes() []int {
	v, err := s.GetSetting()
	if err != nil {
		return nil
	}
	return v.PendingDeletes
}

func (s *ServerManagementService) Traffic() ([]*entity.ServerTraffic, error) {
	// 只锁内读取状态快照（st、v），随即可释放锁；
	// SSH 拨号与读取远程流量的长网络 IO 移到锁外执行，
	// 否则单次可阻塞 serverStateMu 数十秒，拖垮删除/心跳/清零等所有依赖该锁的操作。
	serverStateMu.Lock()
	st, err := s.getTrafficResetState()
	if err != nil {
		serverStateMu.Unlock()
		return nil, err
	}
	v, err := s.GetSetting()
	serverStateMu.Unlock()
	if err != nil {
		return nil, err
	}
	if v.Host == "" {
		return nil, fmt.Errorf("请先配置第三方服务器")
	}
	if st.PendingMonth != 0 && st.LocalDone && !st.RemoteDone {
		return nil, fmt.Errorf("月度远程清零尚未完成，等待重试")
	}

	// ---- 以下为锁外的 SSH 网络 IO：拨号、reload、读取远程流量 ----
	cfg, err := remoteSSHClientConfig(v)
	if err != nil {
		return nil, err
	}
	client, err := dialRemoteSSH("tcp", fmt.Sprintf("%s:%d", v.Host, v.Port), cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH连接失败: %w", err)
	}
	defer client.Close()
	retry, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	err = retry.Run(syncReloadCommand)
	retry.Close()
	if err != nil {
		return nil, fmt.Errorf("重试远程账号配置重启失败: %w", err)
	}

	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	out, err := sess.Output("sqlite3 -batch -noheader -separator '|' /etc/x-ui/x-ui.db '" + remoteTrafficSQL + "'")
	if err != nil {
		return nil, fmt.Errorf("读取远程流量失败: %w", err)
	}
	result, err := parseRemoteTraffic(out)
	if err != nil {
		return nil, err
	}

	// ---- 本地状态修改与写回重新加锁，与 DisableInvalidInbounds 等互斥 ----
	serverStateMu.Lock()
	defer serverStateMu.Unlock()
	currentSetting, err := s.GetSetting()
	if err != nil {
		return nil, err
	}
	currentResetState, err := s.getTrafficResetState()
	if err != nil {
		return nil, err
	}
	if err := validateTrafficSnapshot(v, currentSetting, st, currentResetState); err != nil {
		return nil, err
	}
	if currentSetting.AutoDisable {
		var localInbounds []model.Inbound
		if err := database.GetDB().Find(&localInbounds).Error; err != nil {
			return nil, err
		}
		localByPort := map[int]model.Inbound{}
		for _, in := range localInbounds {
			localByPort[in.Port] = in
		}
		localChanged := false
		// Apply successful local changes even if a later remote operation fails.
		defer func() {
			if localChanged {
				new(XrayService).SetToNeedRestart()
			}
		}()
		remotePorts := make([]string, 0)
		for _, item := range result {
			local, exists := localByPort[item.Port]
			if !exists || local.Total <= 0 || local.Up+local.Down+item.Used < local.Total {
				continue
			}
			if local.Enable {
				changed, err := s.disableLocal(item.Port)
				if err != nil {
					return nil, fmt.Errorf("禁用本地端口 %d 失败: %w", item.Port, err)
				}
				localChanged = localChanged || changed
			}
			if item.Enable {
				remotePorts = append(remotePorts, strconv.Itoa(item.Port))
			}
		}
		// Run the remote command on every heartbeat so a pending restart is
		// retried even after the database row already shows enable=0.
		{
			if len(remotePorts) == 0 {
				remotePorts = append(remotePorts, "0")
			}
			// Check affected rows on the remote database, not only the earlier snapshot.
			cmd := remoteDisableCommand(strings.Join(remotePorts, ","))
			r, e := client.NewSession()
			if e != nil {
				return nil, e
			}
			e = r.Run(cmd)
			r.Close()
			if e != nil {
				return nil, fmt.Errorf("批量禁用远程端口失败: %w", e)
			}
			for _, item := range result {
				local, exists := localByPort[item.Port]
				if exists && local.Total > 0 && local.Up+local.Down+item.Used >= local.Total {
					item.Enable = false
				}
			}
		}
	}
	if err := s.SaveTrafficCache(result); err != nil {
		return nil, fmt.Errorf("保存流量缓存失败: %w", err)
	}
	return result, nil
}

func validateTrafficSnapshot(snapshotSetting, currentSetting *entity.ServerSetting, snapshotState, currentState *trafficResetState) error {
	if remoteIdentity(snapshotSetting) != remoteIdentity(currentSetting) {
		return fmt.Errorf("第三方服务器配置在心跳期间已更改，丢弃旧服务器读取结果")
	}
	if !reflect.DeepEqual(snapshotState, currentState) {
		return fmt.Errorf("流量周期在心跳期间已更改，丢弃旧周期读取结果")
	}
	return nil
}

const remoteTrafficSQL = "SELECT port, COALESCE(up,0), COALESCE(down,0), COALESCE(total,0), COALESCE(enable,0) FROM inbounds ORDER BY port;"

func parseRemoteTraffic(out []byte) ([]*entity.ServerTraffic, error) {
	result := make([]*entity.ServerTraffic, 0)
	content := strings.TrimSpace(string(out))
	if content == "" {
		return result, nil
	}
	for i, line := range strings.Split(content, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 5 {
			return nil, fmt.Errorf("远程流量第 %d 行格式错误：应有 5 个字段", i+1)
		}
		port, e1 := strconv.Atoi(f[0])
		up, e2 := strconv.ParseInt(f[1], 10, 64)
		down, e3 := strconv.ParseInt(f[2], 10, 64)
		total, e4 := strconv.ParseInt(f[3], 10, 64)
		en, e5 := strconv.Atoi(f[4])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || port < 1 || port > 65535 || up < 0 || down < 0 || total < 0 || (en != 0 && en != 1) || up > int64(1<<63-1)-down {
			return nil, fmt.Errorf("远程流量第 %d 行数值无效", i+1)
		}
		result = append(result, &entity.ServerTraffic{Port: port, Up: up, Down: down, Used: up + down, Total: total, Enable: en == 1})
	}
	return result, nil
}

func (s *ServerManagementService) disableLocal(port int) (bool, error) {
	result := database.GetDB().Model(&model.Inbound{}).Where("port = ? AND enable = ?", port, true).
		Updates(map[string]interface{}{"enable": false, "disabled_by": "limit"})
	return result.RowsAffected > 0, result.Error
}

func remoteDisableCommand(ports string) string {
	return fmt.Sprintf(`sqlite3 -bail /etc/x-ui/x-ui.db ".timeout 5000" "BEGIN IMMEDIATE; UPDATE inbounds SET enable=0,disabled_by='limit' WHERE enable=1 AND port IN (%s); INSERT INTO settings(key,value) SELECT 'remoteReloadPending','1' WHERE changes()>0 AND NOT EXISTS (SELECT 1 FROM settings WHERE key='remoteReloadPending'); COMMIT;" || exit $?
pending=$(sqlite3 -batch -noheader /etc/x-ui/x-ui.db "SELECT count(*) FROM settings WHERE key='remoteReloadPending';") || exit $?
if [ "$pending" -gt 0 ]; then
 systemctl restart x-ui || exit $?
 sqlite3 -bail /etc/x-ui/x-ui.db "DELETE FROM settings WHERE key='remoteReloadPending';" || exit $?
fi`, ports)
}

// Persist restart intent in the same transaction as account changes.
func withSyncReloadIntent(sql string) string {
	// changes() must be read immediately after the account write. Reading it
	// after SELECT changes() itself returns zero and loses the reload intent.
	return strings.Replace(sql, "SELECT changes();\nCOMMIT;", "INSERT INTO settings(key,value) SELECT 'syncReloadPending','1' WHERE changes()>0 AND NOT EXISTS (SELECT 1 FROM settings WHERE key='syncReloadPending');\nSELECT changes();\nCOMMIT;", 1)
}

const syncReloadCommand = `pending=$(sqlite3 -batch -noheader /etc/x-ui/x-ui.db "SELECT count(*) FROM settings WHERE key='syncReloadPending';") || exit $?
if [ "$pending" -gt 0 ]; then
 systemctl restart x-ui || exit $?
 sqlite3 -bail /etc/x-ui/x-ui.db "DELETE FROM settings WHERE key='syncReloadPending';" || exit $?
fi`
