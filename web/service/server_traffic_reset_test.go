package service

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/web/entity"
)

func TestFormatTrafficSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1024*1024 - 1, "1024.00 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024*1024*1024 - 1, "1024.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
		{3 * 1024 * 1024 * 1024 / 2, "1.50 GB"},
		{-5, "0 B"},
	}
	for _, c := range cases {
		if got := FormatTrafficSize(c.in); got != c.want {
			t.Errorf("FormatTrafficSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMonthlyTrafficReset(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(&model.Inbound{MonthlyReset: true, Port: 1001, Up: 500, Down: 600, Total: 10000, Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	at := func(y int, m time.Month, d, h, mi, sec int) time.Time {
		return time.Date(y, m, d, h, mi, sec, 0, time.Local)
	}

	// 首次运行只登记当前周期，不能清掉部署前已有的流量。
	if err := s.maybeMonthlyResetAt(at(2026, 9, 15, 10, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if got := inboundUsed(t, 1001); got != 1100 {
		t.Fatalf("首次登记不应清零，得到 %d", got)
	}
	// 同一周期内重复运行无副作用。
	if err := s.maybeMonthlyResetAt(at(2026, 9, 30, 23, 59, 0)); err != nil {
		t.Fatal(err)
	}
	if got := inboundUsed(t, 1001); got != 1100 {
		t.Fatalf("同月不应清零，得到 %d", got)
	}

	// 跨月后清零，且必须保留 total 流量上限。
	if err := s.maybeMonthlyResetAt(at(2026, 10, 1, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	in := inboundBy(t, 1001)
	if in.Up != 0 || in.Down != 0 {
		t.Fatalf("跨月未清零: up=%d down=%d", in.Up, in.Down)
	}
	if in.Total != 10000 {
		t.Fatalf("流量上限被改动: %d", in.Total)
	}

	// 幂等：已确认的周期不再重复清零。
	if err := database.GetDB().Model(&model.Inbound{}).Where("port = ?", 1001).Update("up", 7).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.maybeMonthlyResetAt(at(2026, 10, 20, 8, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if got := inboundUsed(t, 1001); got != 7 {
		t.Fatalf("已完成的周期被重复清零，得到 %d", got)
	}

	// 留档：记下清零前的旧值与清零时刻。月份落在被清掉的那批流量所属的 9 月，
	// 而不是清零发生的 10 月。
	snaps, err := s.TrafficResetSnapshots(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("留档条数异常: %+v", snaps)
	}
	row := snaps[0]
	if row.Yyyymm != 202609 || row.Period != "2026-09" {
		t.Fatalf("留档月份异常: %+v", row)
	}
	if row.Port != 1001 || row.Local != 1100 || row.LocalText != "1.07 KB" {
		t.Fatalf("留档内容异常: %+v", row)
	}
	if row.Limit != 10000 || row.LimitText != "9.77 KB" {
		t.Fatalf("留档未保留套餐上限: %+v", row)
	}
	if row.ResetAt != at(2026, 10, 1, 0, 0, 1).Unix() {
		t.Fatalf("留档时间异常: %d", row.ResetAt)
	}
	// 留档落在该账号自己的表里，表名带账号（入站）id。
	var inboundId int
	if err = database.GetDB().Model(&model.Inbound{}).Where("port = ?", 1001).
		Pluck("id", &inboundId).Error; err != nil {
		t.Fatal(err)
	}
	if row.InboundId != inboundId {
		t.Fatalf("留档未关联账号: %+v", row)
	}
	var stored model.TrafficSnapshot
	if err = database.GetDB().Where("inbound_id = ? AND yyyymm = ?", inboundId, 202609).
		First(&stored).Error; err != nil {
		t.Fatalf("账号 %d 没有 202609 的留档: %v", inboundId, err)
	}
	if stored.LocalUp != 500 || stored.LocalDown != 600 {
		t.Fatalf("库内留档异常: %+v", stored)
	}

	// 跨年同样生效。这里 11、12 月都没有运行过，相当于停机跨月：
	// 留档记在这批流量的累计起点（10 月），不会把多个月的用量伪造成一个月。
	if err := database.GetDB().Model(&model.Inbound{}).Where("port = ?", 1001).Update("up", 99).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.maybeMonthlyResetAt(at(2027, 1, 1, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if got := inboundUsed(t, 1001); got != 0 {
		t.Fatalf("跨年未清零，得到 %d", got)
	}
	if snaps, err = s.TrafficResetSnapshots(0); err != nil || len(snaps) != 2 {
		t.Fatalf("跨年留档未追加: %+v %v", snaps, err)
	} else if snaps[0].Yyyymm != 202610 || snaps[1].Yyyymm != 202609 {
		t.Fatalf("跨年留档月份异常: %+v", snaps)
	}
}

// 留档落库：所有账号共用一张表，靠 inbound_id 区分，表内用 yyyymm 区分月份，
// 远程用量按端口并入同一账号。
func TestTrafficSnapshotSingleTable(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	db := database.GetDB()
	for _, v := range []struct {
		port     int
		up, down int64
	}{
		{5001, 1536, 0},
		{5002, 0, 2048},
	} {
		in := &model.Inbound{MonthlyReset: true, Port: v.port, Up: v.up, Down: v.down, Total: 1 << 30, Tag: fmt.Sprintf("inbound-%d", v.port), Remark: fmt.Sprintf("客户%d", v.port), Enable: true}
		if err := db.Create(in).Error; err != nil {
			t.Fatal(err)
		}
	}
	// 远程用量取自流量审计缓存，按端口并入对应账号的留档。
	if err := s.SaveTrafficCache([]*entity.ServerTraffic{{Port: 5002, Up: 512, Down: 1024, Used: 1536, Total: 1 << 30, Enable: true}}); err != nil {
		t.Fatal(err)
	}
	at := func(y int, m time.Month, d, h, mi, sec int) time.Time {
		return time.Date(y, m, d, h, mi, sec, 0, time.Local)
	}
	if err := s.maybeMonthlyResetAt(at(2026, 9, 10, 8, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.maybeMonthlyResetAt(at(2026, 10, 1, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}

	// 单表存全部账号：不应存在按账号拆出来的表，两个账号同月各占一行。
	var names []string
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type = 'table'").Scan(&names).Error; err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == "traffic_snapshots" {
			found = true
			continue
		}
		if strings.HasPrefix(n, "traffic_snapshot_") {
			t.Fatalf("不应再按账号分表，出现了 %s", n)
		}
	}
	if !found {
		t.Fatalf("留档单表 traffic_snapshots 不存在: %v", names)
	}

	var locals []model.Inbound
	if err := db.Order("port").Find(&locals).Error; err != nil {
		t.Fatal(err)
	}
	rows, err := s.TrafficResetSnapshots(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("期望 2 条留档，得到 %+v", rows)
	}
	for i, in := range locals {
		row := rows[i] // 同月按端口升序
		if row.InboundId != in.Id || row.Port != in.Port || row.Yyyymm != 202609 {
			t.Fatalf("留档归属异常: %+v", row)
		}
		var n int64
		if err = db.Model(&model.TrafficSnapshot{}).
			Where("inbound_id = ? AND yyyymm = ?", in.Id, 202609).Count(&n).Error; err != nil {
			t.Fatalf("账号 %d 的留档不可读: %v", in.Id, err)
		}
		if n != 1 {
			t.Fatalf("账号 %d 应有 1 条当月留档，得到 %d", in.Id, n)
		}
	}
	// (inbound_id, yyyymm) 复合唯一索引：最左列就是账号，既保证同一账号同月只有一行，
	// 又直接支撑按账号查询，因此不必再单独建一个 inbound_id 索引。
	var cols []struct {
		Seqno int    `gorm:"column:seqno"`
		Name  string `gorm:"column:name"`
	}
	if err = db.Raw("PRAGMA index_info(idx_traffic_snapshot_account_month)").Scan(&cols).Error; err != nil {
		t.Fatal(err)
	}
	if len(cols) != 2 || cols[0].Name != "inbound_id" || cols[1].Name != "yyyymm" {
		t.Fatalf("复合唯一索引列顺序异常: %+v", cols)
	}
	// 远程用量落在自己账号那一行，不会串到别的账号。
	if rows[1].Port != 5002 || rows[1].Local != 2048 || rows[1].Remote != 1536 || rows[1].Used != 3584 {
		t.Fatalf("远程用量未并入对应账号: %+v", rows[1])
	}
	if rows[1].RemoteText != "1.50 KB" || rows[0].Remote != 0 || rows[0].LocalText != "1.50 KB" {
		t.Fatalf("留档展示文本异常: %+v", rows)
	}
}

// 同账号同月重复写入只留一行；每个账号的留档表按 24 个月滚动保留。
func TestTrafficSnapshotIdempotentAndRetention(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	db := database.GetDB()
	in := &model.Inbound{MonthlyReset: true, Port: 6001, Up: 1024, Total: 1 << 30, Tag: "inbound-6001", Remark: "客户", Enable: true}
	if err := db.Create(in).Error; err != nil {
		t.Fatal(err)
	}
	table := model.TrafficSnapshot{}.TableName()

	// 清零重试会重复写同一个月，必须覆盖原行而不是追加。
	for i := 0; i < 3; i++ {
		if err := db.Model(&model.Inbound{}).Where("id = ?", in.Id).
			Update("up", int64(1024*(i+1))).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.snapshotAndResetLocal(&trafficResetState{}, 202609, time.Unix(int64(1700000000+i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	if err := db.Table(table).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("同月重复写入产生了 %d 行", n)
	}
	var snap model.TrafficSnapshot
	if err := db.Table(table).Where("yyyymm = ?", 202609).First(&snap).Error; err != nil {
		t.Fatal(err)
	}
	if snap.LocalUp != 3072 || snap.ResetAt != 1700000002 {
		t.Fatalf("同月重复写入未覆盖原行: %+v", snap)
	}

	// 连续 40 个月只保留最近 36 期，最旧的按月淘汰。
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < 40; i++ {
		at := base.AddDate(0, i, 0)
		if err := db.Model(&model.Inbound{}).Where("id = ?", in.Id).Update("up", 1024).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.snapshotAndResetLocal(&trafficResetState{}, TrafficYyyymm(at), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Table(table).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != trafficResetKeepPeriods {
		t.Fatalf("期望保留 %d 期，实际 %d 行", trafficResetKeepPeriods, n)
	}
	var oldest model.TrafficSnapshot
	if err := db.Table(table).Order("yyyymm").First(&oldest).Error; err != nil {
		t.Fatal(err)
	}
	if want := TrafficYyyymm(base.AddDate(0, 4, 0)); oldest.Yyyymm != want {
		t.Fatalf("淘汰的不是最旧的月份：%d，期望 %d", oldest.Yyyymm, want)
	}

	// 月份编号与展示形式。
	if YyyymmPeriod(202609) != "2026-09" || YyyymmPeriod(202712) != "2027-12" {
		t.Fatal("月份展示格式异常")
	}
	// 进度缺失时回退到上一个自然月，跨年不出错。
	if got := archivedMonth(&trafficResetState{}, time.Date(2026, 3, 1, 0, 0, 1, 0, time.Local)); got != 202602 {
		t.Fatalf("空进度应回退到上个月，得到 %d", got)
	}
	if got := archivedMonth(&trafficResetState{}, time.Date(2026, 1, 1, 0, 0, 1, 0, time.Local)); got != 202512 {
		t.Fatalf("跨年回退异常，得到 %d", got)
	}
}

// 零用量的账号当月也要留档（各项记为 0）：留档是完整账目，缺月会让人分不清
// "当月没用"和"当月没记录"。套餐上限仍要照实保留。
func TestTrafficSnapshotKeepsZeroUsage(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	db := database.GetDB()
	in := &model.Inbound{MonthlyReset: true, Port: 7001, Total: 1 << 30, Tag: "inbound-7001", Remark: "零用量客户", Enable: false}
	if err := db.Create(in).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.snapshotAndResetLocal(&trafficResetState{}, 202609, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.TrafficResetSnapshots(in.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("零用量账号也应有 1 条留档，得到 %+v", rows)
	}
	row := rows[0]
	if row.InboundId != in.Id || row.Port != 7001 || row.Yyyymm != 202609 {
		t.Fatalf("零用量留档归属异常: %+v", row)
	}
	if row.Local != 0 || row.Remote != 0 || row.Used != 0 || row.LocalText != "0 B" || row.UsedText != "0 B" {
		t.Fatalf("零用量留档应记为 0: %+v", row)
	}
	if row.Limit != 1<<30 || row.LimitText != "1.00 GB" {
		t.Fatalf("零用量留档仍应保留套餐上限: %+v", row)
	}
}

// 按账号取留档：只返回该账号的行，且按 yyyymm 从最近往最前排序；
// 传入不存在的账号返回空，而不是退化成全量。
func TestTrafficSnapshotFilterByInbound(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	db := database.GetDB()
	ids := make([]int, 0, 2)
	for _, port := range []int{8001, 8002} {
		in := &model.Inbound{MonthlyReset: true, Port: port, Up: 1024, Total: 1 << 30, Tag: fmt.Sprintf("inbound-%d", port), Remark: fmt.Sprintf("客户%d", port), Enable: true}
		if err := db.Create(in).Error; err != nil {
			t.Fatal(err)
		}
		ids = append(ids, in.Id)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < 3; i++ {
		at := base.AddDate(0, i, 0)
		if err := db.Model(&model.Inbound{}).Where("id IN ?", ids).Update("up", int64(1024*(i+1))).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.snapshotAndResetLocal(&trafficResetState{}, TrafficYyyymm(at), at); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.TrafficResetSnapshots(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("期望 6 条留档（2 账号 × 3 个月），得到 %d", len(all))
	}
	// 全局视图：最新月份在前，同月按端口升序。
	if all[0].Yyyymm != 202603 || all[1].Yyyymm != 202603 || all[0].Port != 8001 || all[1].Port != 8002 {
		t.Fatalf("全局排序异常: %+v", all[:2])
	}

	one, err := s.TrafficResetSnapshots(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 3 {
		t.Fatalf("按账号过滤后应有 3 条，得到 %+v", one)
	}
	for _, row := range one {
		if row.InboundId != ids[1] || row.Port != 8002 {
			t.Fatalf("过滤后混入了其他账号: %+v", row)
		}
	}
	for i, want := range []int{202603, 202602, 202601} {
		if one[i].Yyyymm != want {
			t.Fatalf("第 %d 条应为 %d，得到 %+v", i, want, one)
		}
	}
	if none, err := s.TrafficResetSnapshots(999999); err != nil || len(none) != 0 {
		t.Fatalf("不存在的账号不应返回留档: %+v %v", none, err)
	}
}

// 汇总接口必须同时给出格式化文本，页面直接渲染，避免前端各算一套口径。
func TestSummaryTrafficText(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(&model.Inbound{MonthlyReset: true, Port: 2001, Up: 1 << 20, Down: 1 << 19, Total: 1 << 30, Remark: "客户A", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	// 不限量账号：总流量为 0，上限列不能显示成 "0 B"。
	if err := database.GetDB().Create(&model.Inbound{MonthlyReset: true, Port: 2002, Up: 1024, Total: 0, Remark: "客户B", Enable: true, Tag: "inbound-2002"}).Error; err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	rows, err := s.Summary(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("期望 2 行，得到 %d", len(rows))
	}
	byPort := map[int]*entity.TrafficSummary{}
	for _, r := range rows {
		byPort[r.Port] = r
	}
	row := byPort[2001]
	if row == nil {
		t.Fatalf("缺少端口 2001 的汇总行: %+v", rows)
	}
	if row.LocalText != "1.50 MB" || row.RemoteText != "0 B" || row.TotalText != "1.50 MB" || row.LimitText != "1.00 GB" {
		t.Fatalf("展示文本异常: %+v", row)
	}
	// 计费方式要带出去，页面靠它区分“按月／累计”，也靠它决定快照弹窗的提示。
	if !row.MonthlyReset {
		t.Fatalf("汇总未带出按月标记: %+v", row)
	}
	if unlimited := byPort[2002]; unlimited == nil || unlimited.Limit != 0 || unlimited.LimitText != "无限制" {
		t.Fatalf("不限量账号的上限文案异常: %+v", unlimited)
	}
	// 页面直接按 data-index 取这几列，并靠 inboundId 拉取该账号的月度留档，
	// 锁住 JSON 字段名防止前后端脱节。
	if row.InboundId == 0 {
		t.Fatalf("汇总缺少入站主键，页面无法按账号拉取留档: %+v", row)
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"inboundId", "localText", "remoteText", "totalText", "limitText", "status", "overlimit", "monthlyReset"} {
		if !strings.Contains(string(b), `"`+key+`"`) {
			t.Fatalf("JSON 缺少字段 %s: %s", key, b)
		}
	}
	// 受限登录携带的入站 id 不存在时不能返回任何行。
	if rows, err = s.Summary(9999); err != nil || len(rows) != 0 {
		t.Fatalf("越权入站不应返回数据: %+v %v", rows, err)
	}
}

func inboundBy(t *testing.T, port int) model.Inbound {
	t.Helper()
	var in model.Inbound
	if err := database.GetDB().Where("port = ?", port).First(&in).Error; err != nil {
		t.Fatal(err)
	}
	return in
}

func inboundUsed(t *testing.T, port int) int64 {
	t.Helper()
	in := inboundBy(t, port)
	return in.Up + in.Down
}
