package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strconv"
	"strings"
	"time"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/util/common"
	"x-ui/web/entity"
)

const (
	// trafficResetStateKey 记录月度清零进度，保证服务重启后既不漏做也不重复做。
	trafficResetStateKey = "serverTrafficMonthlyReset"
	// trafficResetKeepPeriods 每个账号最多保留的月度留档数（近 36 个月），
	// 更早的按月淘汰，避免留档无限增长。
	trafficResetKeepPeriods = 36
)

// trafficResetState 是月度清零的进度。ConfirmedMonth 为已完成的月份，
// PendingMonth 为正在处理的月份：远程清零失败时会停在这一步等待下一轮重试。
// 月份用 yyyymm 编号（如 202609），与留档表的字段保持同一口径。
type trafficResetState struct {
	RemoteIdentity string `json:"remoteIdentity"`
	ConfirmedMonth int    `json:"confirmedMonth"`
	PendingMonth   int    `json:"pendingMonth"`
	LocalDone      bool   `json:"localDone"`
	RemoteDone     bool   `json:"remoteDone"`
	// ReactivatePorts 是本次清零前“因超限被停用”的端口，供远程阶段恢复启用。
	// 本地清零会把用量归零、超限状态随之消失，所以只能在这一步留存；
	// 否则远程清零失败重试时，就再也推不出该启用哪些端口了。
	ReactivatePorts []int `json:"reactivatePorts"`
	// MonthlyPorts 是本次清零涉及的端口（即勾选了“按月计算”的入站）。
	// 远程侧据此按端口清零：远程库未必有 monthly_reset 列，而且非按月账号的用量
	// 本就不能被清掉，所以不能像以前那样全表清零。与 ReactivatePorts 同理，
	// 这必须与本地清零的那批端口完全一致，故在本地事务里一并留存。
	MonthlyPorts []int `json:"monthlyPorts"`
}

// TrafficYyyymm 把时间转成月份编号，如 2026 年 9 月得到 202609。
func TrafficYyyymm(t time.Time) int {
	return t.Year()*100 + int(t.Month())
}

// YyyymmPeriod 把月份编号还原成 2026-09 形式，供页面展示。
func YyyymmPeriod(yyyymm int) string {
	return fmt.Sprintf("%04d-%02d", yyyymm/100, yyyymm%100)
}

// MaybeMonthlyReset 在跨月后把本地与远程入站的已用流量清零，并按账号留档。
// 只处理勾选了“按月计算”的入站：未勾选的账号用量一直累计、流量用完即止，
// 既不进月度留档，也不会在月初被自动恢复启用。
// 只清 up/down，保留 total 流量上限，因此流量上限会随自然月滚动。
// 幂等：进度记录在 settings 表，重复调用或服务重启都不会重复清零。
func (s *ServerManagementService) MaybeMonthlyReset() error {
	// 统一按上海时区计算"当前月份"：cron 也以东八区触发，
	// 保证触发判定与这里面的月份推算使用同一时区，避免系统时区不同导致错位。
	return s.maybeMonthlyResetAt(nowCN())
}

func nowCN() time.Time {
	return time.Now().In(common.ShanghaiLocation)
}

func (s *ServerManagementService) maybeMonthlyResetAt(now time.Time) error {
	serverStateMu.Lock()
	defer serverStateMu.Unlock()
	month := TrafficYyyymm(now)
	st, err := s.getTrafficResetState()
	if err != nil {
		return err
	}
	if st.ConfirmedMonth == 0 && st.PendingMonth == 0 {
		// 首次启用只登记当前月份，从下个自然月才开始清零，避免部署当天误清已有流量。
		st.ConfirmedMonth = month
		return s.saveTrafficResetState(st)
	}
	if st.ConfirmedMonth == month {
		return nil
	}
	if st.PendingMonth != month {
		// Local counters already belong to the pending month, even if remote
		// completion failed. Archive them under that month, not the older one.
		if st.LocalDone && st.PendingMonth > 0 {
			st.ConfirmedMonth = st.PendingMonth
		}
		v, err := s.GetSetting()
		if err != nil {
			return err
		}
		st.RemoteIdentity = remoteIdentity(v)
		st.PendingMonth = month
		st.LocalDone = false
		st.RemoteDone = false
	}
	if !st.LocalDone {
		// 本地清零与进度（含待恢复启用的端口）在同一事务内提交，
		// 因此这里返回即为已落盘，st.LocalDone 也已被置为真。
		if err = s.snapshotAndResetLocal(st, archivedMonth(st, now), now); err != nil {
			return err
		}
	}
	if !st.RemoteDone {
		v, e := s.GetSetting()
		if e != nil {
			return e
		}
		if (st.RemoteIdentity != "" && st.RemoteIdentity != remoteIdentity(v)) || v.Host == "" || v.Password == "" {
			// 未配置远程服务器时视为已完成，不影响本地清零的确认。
			st.RemoteDone = true
		} else if len(st.MonthlyPorts) == 0 && len(st.ReactivatePorts) == 0 {
			// 没有任何“按月计算”的账号，远程既无需清零也无需恢复启用，跳过 SSH。
			st.RemoteDone = true
		} else if e = s.resetRemoteTraffic(v, st.MonthlyPorts, st.ReactivatePorts, now); e != nil {
			// 保留进度等待重试；本地已清零，下一轮不会再动本地数据。
			if saveErr := s.saveTrafficResetState(st); saveErr != nil {
				return fmt.Errorf("%v（保存清零进度失败: %w）", e, saveErr)
			}
			return e
		} else {
			st.RemoteDone = true
		}
	}
	st.ReactivatePorts = nil
	st.MonthlyPorts = nil
	st.ConfirmedMonth = month
	return s.saveTrafficResetState(st)
}

// archivedMonth 是本次清零要留档的月份：上一轮确认过的月份，也就是这批流量的累计起点。
// 正常情况下等于上一个自然月；服务停机跨月时沿用起点月份，不会把多个月的用量误记为一个月。
func archivedMonth(st *trafficResetState, now time.Time) int {
	if st.ConfirmedMonth > 0 {
		return st.ConfirmedMonth
	}
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return TrafficYyyymm(firstOfMonth.AddDate(0, 0, -1))
}

// TrafficResetSnapshots 汇总月度流量留档，最新月份在前，同月按端口升序。
// inboundId > 0 时只返回该账号的留档（受限登录专用），在查询阶段就限定范围，
// 避免其他账号的留档进入内存或被带出去。
func (s *ServerManagementService) TrafficResetSnapshots(inboundId int) ([]*entity.TrafficSnapshot, error) {
	stored, err := readAllTrafficSnapshots(database.GetDB(), inboundId)
	if err != nil {
		return nil, err
	}
	out := make([]*entity.TrafficSnapshot, 0, len(stored))
	for _, v := range stored {
		local := v.LocalUp + v.LocalDown
		remote := v.RemoteUp + v.RemoteDown
		out = append(out, &entity.TrafficSnapshot{
			Yyyymm:     v.Yyyymm,
			Period:     YyyymmPeriod(v.Yyyymm),
			InboundId:  v.InboundId,
			Port:       v.Port,
			Remark:     v.Remark,
			Local:      local,
			Remote:     remote,
			Used:       local + remote,
			Limit:      v.Total,
			LocalText:  FormatTrafficSize(local),
			RemoteText: FormatTrafficSize(remote),
			UsedText:   FormatTrafficSize(local + remote),
			LimitText:  FormatTrafficLimit(v.Total),
			ResetAt:    v.ResetAt,
		})
	}
	return out, nil
}

func (s *ServerManagementService) getTrafficResetState() (*trafficResetState, error) {
	return readTrafficResetState(database.GetDB())
}

func readTrafficResetState(db *gorm.DB) (*trafficResetState, error) {
	row := &model.Setting{}
	err := db.Where("key = ?", trafficResetStateKey).First(row).Error
	if database.IsNotFound(err) {
		return &trafficResetState{}, nil
	}
	if err != nil {
		return nil, err
	}
	st := &trafficResetState{}
	if err = json.Unmarshal([]byte(row.Value), st); err != nil {
		return nil, fmt.Errorf("解析流量清零进度失败: %w", err)
	}
	return st, nil
}

func (s *ServerManagementService) saveTrafficResetState(st *trafficResetState) error {
	return saveTrafficResetStateTx(database.GetDB(), st)
}

// saveTrafficResetStateTx 在给定事务里落盘清零进度。
// 本地清零与进度写入必须在同一事务：否则一旦进度写入失败，下一轮会把已经清零的
// 用量当作旧值再留一次档，反而把正确的留档覆盖成全 0。
func saveTrafficResetStateTx(tx *gorm.DB, st *trafficResetState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	row := &model.Setting{}
	err = tx.Where("key = ?", trafficResetStateKey).First(row).Error
	if database.IsNotFound(err) {
		return tx.Create(&model.Setting{Key: trafficResetStateKey, Value: string(b)}).Error
	}
	if err != nil {
		return err
	}
	row.Value = string(b)
	return tx.Save(row).Error
}

// snapshotAndResetLocal 在同一个事务里读取旧值、按账号写入留档表、清零本地已用流量，
// 并把“上个月因超限被停用的端口”恢复启用，最后一起落盘清零进度。
// 四件事同事务是为了保证不会出现“清零了却没留档”或“清零了却没记进度”的中间态。
//
// 只处理勾选了“按月计算”的入站：只有它们才有“某个月用了多少”这回事。
// 未勾选的账号既不进留档也不清零，用量一路累计、流量用完即止。
//
// 零用量的按月账号同样留档（记为 0）：留档是“这个账号某个月用了多少”的完整账目，
// 缺月会让对账时分不清“当月没用”和“当月没记录”。
//
// 恢复启用只针对按月账号里因超限停用的那些：管理员手动停用的（用量没到上限）不在其中，
// 已过期的即使超限也保持停用，避免月初把到期账号又放出来；
// 非按月账号根本不会进这个名单，所以“用完即止”的停用不会被月初解除。
func (s *ServerManagementService) snapshotAndResetLocal(st *trafficResetState, yyyymm int, now time.Time) error {
	changed := false
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		var locals []model.Inbound
		if err := tx.Order("port").Find(&locals).Error; err != nil {
			return err
		}
		remote, err := readTrafficCache(tx)
		if err != nil {
			return err
		}
		byPort := make(map[int]*entity.ServerTraffic, len(remote))
		for _, r := range remote {
			byPort[r.Port] = r
		}
		nowMs := now.Unix() * 1000
		monthlyPorts := make([]int, 0, len(locals))
		// 恢复启用的名单必须在清零前判定：用量一旦归零，就再也推不出谁是因为超限被停用的。
		reactivatePorts := make([]int, 0)
		for _, in := range locals {
			if !in.MonthlyReset {
				continue
			}
			monthlyPorts = append(monthlyPorts, in.Port)
			snap := &model.TrafficSnapshot{
				Yyyymm:    yyyymm,
				InboundId: in.Id,
				Port:      in.Port,
				Remark:    in.Remark,
				LocalUp:   in.Up,
				LocalDown: in.Down,
				Total:     in.Total,
				ResetAt:   now.Unix(),
			}
			if r := byPort[in.Port]; r != nil {
				snap.RemoteUp, snap.RemoteDown = r.Up, r.Down
			}
			if err = saveTrafficSnapshot(tx, snap); err != nil {
				return err
			}
			// 只恢复因超限被自动停用的账号（DisabledBy=="limit"），
			// 且未过期。管理员手动停用的账号（DisabledBy=="manual"）即使用量恰好
			// 超限也不在这里恢复，避免月初误复活。
			if in.DisabledBy == "limit" && !expiredAt(&in, nowMs) &&
				TrafficOverlimit(in.Up+in.Down, snap.RemoteUp+snap.RemoteDown, in.Total) {
				reactivatePorts = append(reactivatePorts, in.Port)
			}
		}
		// 只清按月账号的已用流量，逐个端口对齐，远程阶段据此清同一批端口；total 是套餐上限，必须保留。
		if len(monthlyPorts) > 0 {
			if err = tx.Model(&model.Inbound{}).Where("port in ?", monthlyPorts).
				UpdateColumns(map[string]interface{}{"up": 0, "down": 0}).Error; err != nil {
				return err
			}
		}
		// 新月份从零开始计数，上个月因超限停用的按月账号随之恢复启用，并清除停用来源。
		if len(reactivatePorts) > 0 {
			result := tx.Model(&model.Inbound{}).Where("port in ? AND enable = ?", reactivatePorts, false).
				Updates(map[string]interface{}{"enable": true, "disabled_by": ""})
			if result.Error != nil {
				return result.Error
			}
			changed = result.RowsAffected > 0
		}
		// Remove prior-month counters before restored accounts can be checked.
		for _, r := range remote {
			for _, port := range monthlyPorts {
				if r.Port == port {
					r.Up = 0
					r.Down = 0
					r.Used = 0
				}
			}
		}
		cache, err := json.Marshal(remote)
		if err != nil {
			return err
		}
		if err = tx.Model(&model.Setting{}).Where("key = ?", trafficCacheKey).Update("value", string(cache)).Error; err != nil {
			return err
		}
		st.LocalDone = true
		st.ReactivatePorts = reactivatePorts
		st.MonthlyPorts = monthlyPorts
		return saveTrafficResetStateTx(tx, st)
	})
	if err == nil && changed {
		new(XrayService).SetToNeedRestart()
	}
	return err
}

// saveTrafficSnapshot 写入一条月度留档。同账号同月重复写入会覆盖原行，
// 因此清零重试不会写出重复记录；写入后按账号做一次保留期裁剪。
func saveTrafficSnapshot(tx *gorm.DB, snap *model.TrafficSnapshot) error {
	err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "inbound_id"}, {Name: "yyyymm"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"port", "remark", "local_up", "local_down",
			"remote_up", "remote_down", "total", "reset_at",
		}),
	}).Create(snap).Error
	if err != nil {
		return fmt.Errorf("写入账号 %d 的 %d 月度留档失败: %w", snap.InboundId, snap.Yyyymm, err)
	}
	return trimTrafficSnapshot(tx, snap.InboundId)
}

// trimTrafficSnapshot 每个账号只保留最近若干个月的留档，最旧的按月淘汰。
func trimTrafficSnapshot(tx *gorm.DB, inboundId int) error {
	months := make([]int, 0, trafficResetKeepPeriods)
	if err := tx.Model(&model.TrafficSnapshot{}).Where("inbound_id = ?", inboundId).
		Order("yyyymm DESC").Limit(trafficResetKeepPeriods).
		Pluck("yyyymm", &months).Error; err != nil {
		return err
	}
	if len(months) < trafficResetKeepPeriods {
		return nil
	}
	return tx.Where("inbound_id = ? AND yyyymm < ?", inboundId, months[len(months)-1]).
		Delete(&model.TrafficSnapshot{}).Error
}

// readAllTrafficSnapshots 读取月度留档：最新月份在前，同月按端口升序。
// inboundId > 0 时只读该账号，靠 (inbound_id, yyyymm) 复合索引的最左列直接命中。
func readAllTrafficSnapshots(tx *gorm.DB, inboundId int) ([]*model.TrafficSnapshot, error) {
	all := make([]*model.TrafficSnapshot, 0)
	query := tx.Order("yyyymm DESC, port ASC")
	if inboundId > 0 {
		query = query.Where("inbound_id = ?", inboundId)
	}
	if err := query.Find(&all).Error; err != nil {
		return nil, fmt.Errorf("读取月度流量留档失败: %w", err)
	}
	return all, nil
}

// resetRemoteTraffic 通过 SSH 清零第三方面板里指定端口的已用流量，并恢复月初应启用的端口。
// 仅清零流量不需重启；恢复启用后需要重新加载远程运行配置。
func (s *ServerManagementService) resetRemoteTraffic(v *entity.ServerSetting, monthlyPorts, reactivatePorts []int, now time.Time) error {
	cfg := &ssh.ClientConfig{User: v.Username, Auth: []ssh.AuthMethod{ssh.Password(v.Password)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
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
	sess.Stdin = strings.NewReader(remoteMonthlyResetSQL(monthlyPorts, reactivatePorts, now))
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	out, err := sess.Output(remoteMonthlyResetCommand)
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("清零远程流量失败: %s: %w", msg, err)
		}
		return fmt.Errorf("清零远程流量失败: %w", err)
	}
	if n, e := strconv.Atoi(strings.TrimSpace(string(out))); e != nil || n < 0 {
		return fmt.Errorf("清零远程流量返回异常: %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// remoteMonthlyResetSQL 生成远程库的月初重置脚本：先恢复启用，再清零指定端口的用量。
//
// 恢复启用的语句必须排在清零之前——清零之后用量归零，超限条件就不再成立了。
// 端口号来自本地判定结果（本地已按“本地 + 远程”的汇总口径确认它们因超限被停用），
// 这里再要求 enable=0 且未过期，避免把远程手动停用的或已到期的账号一并放出来。
//
// 清零按端口名单而不是全表：非按月账号的用量累计到底，不能被清掉；
// 同时也不去依赖远程库有 monthly_reset 列（第三方面板未必是这一版）。
// 名单为空时用 WHERE 0 兜底，保证末尾 SELECT changes() 读到的始终是清零影响的行数。
func remoteMonthlyResetSQL(monthlyPorts, reactivatePorts []int, now time.Time) string {
	var b strings.Builder
	b.WriteString(".timeout 5000\nBEGIN IMMEDIATE;\n")
	b.WriteString(fmt.Sprintf("CREATE TEMP TABLE reset_guard AS SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM settings WHERE key='remoteMonthlyResetApplied' AND value='%d');\n", TrafficYyyymm(now)))
	if ports := validPortList(reactivatePorts); len(ports) > 0 {
		b.WriteString(fmt.Sprintf(
			"UPDATE inbounds SET enable=1 WHERE EXISTS (SELECT 1 FROM reset_guard) AND enable=0 AND (expiry_time=0 OR expiry_time>%d) AND port IN (%s);\n",
			now.Unix()*1000, strings.Join(ports, ",")))
		// Persist reload intent together with the enable change, so a failed
		// restart is retried even when the next UPDATE changes no rows.
		b.WriteString("INSERT INTO settings (key,value) SELECT 'monthlyResetReloadPending','1' WHERE changes()>0 AND NOT EXISTS (SELECT 1 FROM settings WHERE key='monthlyResetReloadPending');\n")
	}
	clearCond := "0"
	if ports := validPortList(monthlyPorts); len(ports) > 0 {
		clearCond = "port IN (" + strings.Join(ports, ",") + ")"
	}
	b.WriteString("UPDATE inbounds SET up=0, down=0 WHERE " + clearCond + " AND EXISTS (SELECT 1 FROM reset_guard);\nSELECT changes();\n")
	b.WriteString(fmt.Sprintf("DELETE FROM settings WHERE key='remoteMonthlyResetApplied'; INSERT INTO settings(key,value) VALUES('remoteMonthlyResetApplied','%d'); COMMIT;\n", TrafficYyyymm(now)))
	return b.String()
}

const remoteMonthlyResetCommand = `test -f /etc/x-ui/x-ui.db || exit 1
sqlite3 -batch -bail -noheader /etc/x-ui/x-ui.db || exit $?
pending=$(sqlite3 -batch -noheader /etc/x-ui/x-ui.db "SELECT count(*) FROM settings WHERE key='monthlyResetReloadPending';") || exit $?
if [ "$pending" -gt 0 ]; then
  systemctl restart x-ui >&2 || exit $?
  sqlite3 -bail /etc/x-ui/x-ui.db "DELETE FROM settings WHERE key='monthlyResetReloadPending';" || exit $?
fi`

// validPortList 过滤出合法端口并转成字符串，非法值直接丢弃：
// 这些数字会被拼进 SQL，不能放任何来路不明的内容进去。
func validPortList(ports []int) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		if p > 0 && p <= 65535 {
			out = append(out, strconv.Itoa(p))
		}
	}
	return out
}

func readTrafficCache(tx *gorm.DB) ([]*entity.ServerTraffic, error) {
	row := &model.Setting{}
	err := tx.Where("key = ?", trafficCacheKey).First(row).Error
	if database.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v []*entity.ServerTraffic
	if err = json.Unmarshal([]byte(row.Value), &v); err != nil {
		return nil, fmt.Errorf("解析流量缓存失败: %w", err)
	}
	return v, nil
}

func remoteIdentity(v *entity.ServerSetting) string {
	b, _ := json.Marshal([]interface{}{v.Host, v.Port, v.Username})
	return string(b)
}
