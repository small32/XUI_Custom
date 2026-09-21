package service

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/web/entity"
)

// 超限判定走汇总口径（本地 + 远程），三态划分互斥且完备。
func TestTrafficOverlimitAndStatus(t *testing.T) {
	cases := []struct {
		local, remote, limit int64
		want                 bool
	}{
		{0, 0, 0, false},       // 不限量
		{1 << 30, 0, 0, false}, // 不限量时用量再大也不算超限
		{99, 0, 100, false},
		{100, 0, 100, true}, // 正好到上限即算超限
		{101, 0, 100, true},
		{50, 50, 100, true}, // 本地不够、远程补上
		{0, 100, 100, true}, // 只有远程用量
	}
	for _, c := range cases {
		if got := TrafficOverlimit(c.local, c.remote, c.limit); got != c.want {
			t.Errorf("TrafficOverlimit(%d, %d, %d) = %v, 期望 %v", c.local, c.remote, c.limit, got, c.want)
		}
	}

	// 启用优先：仍在启用的一律归 enabled，哪怕当下已超限（等停用后才进 overlimit）。
	pairs := []struct {
		enable, overlimit bool
		want              string
	}{
		{true, true, TrafficStatusEnabled},
		{true, false, TrafficStatusEnabled},
		{false, true, TrafficStatusOverlimit},
		{false, false, TrafficStatusDisabled},
	}
	for _, p := range pairs {
		if got := TrafficStatusOf(p.enable, p.overlimit); got != p.want {
			t.Errorf("TrafficStatusOf(%v, %v) = %q, 期望 %q", p.enable, p.overlimit, got, p.want)
		}
	}
}

// 汇总接口要为每个账号给出超限标记与三态取值，页面直接按 status 过滤。
func TestSummaryCarriesOverlimitStatus(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	db := database.GetDB()
	const limit = 1000
	seeds := []struct {
		port   int
		up     int64
		total  int64
		enable bool
		want   string
	}{
		{9101, 100, limit, true, TrafficStatusEnabled},    // 启用、未超限
		{9102, 1000, limit, true, TrafficStatusEnabled},   // 已到上限但还启用着：仍归启用
		{9103, 100, limit, false, TrafficStatusDisabled},  // 手动停用、未超限
		{9104, 600, limit, false, TrafficStatusOverlimit}, // 停用、靠远程补足到上限
		{9105, 1 << 20, 0, false, TrafficStatusDisabled},  // 不限量，永远不会超限
	}
	for _, v := range seeds {
		in := &model.Inbound{
			MonthlyReset: true, Port: v.port, Up: v.up, Total: v.total, Enable: v.enable,
			Tag: fmt.Sprintf("inbound-%d", v.port), Remark: fmt.Sprintf("客户%d", v.port),
		}
		if err := db.Create(in).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveTrafficCache([]*entity.ServerTraffic{{Port: 9104, Up: 500, Used: 500, Total: limit, Enable: true}}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Summary(0)
	if err != nil {
		t.Fatal(err)
	}
	byPort := make(map[int]*entity.TrafficSummary, len(rows))
	for _, r := range rows {
		byPort[r.Port] = r
	}
	for _, v := range seeds {
		r := byPort[v.port]
		if r == nil {
			t.Fatalf("端口 %d 没有汇总行", v.port)
		}
		if r.Status != v.want {
			t.Errorf("端口 %d 状态 = %q，期望 %q（overlimit=%v used=%d limit=%d）",
				v.port, r.Status, v.want, r.Overlimit, r.Total, r.Limit)
		}
	}
	// 超限必须按汇总算：9104 本地 600 单独看没到 1000。
	if got := byPort[9104]; got.Total != 1100 || !got.Overlimit {
		t.Fatalf("9104 应按本地 600 + 远程 500 判超限: %+v", got)
	}
	// 已到上限但仍在启用的账号，超限标记与状态各司其职，不能互相污染。
	if got := byPort[9102]; !got.Overlimit || got.Status != TrafficStatusEnabled {
		t.Fatalf("9102 应 overlimit=true 且 status=enabled: %+v", got)
	}
}

// 自动停用改按汇总口径：本地没到上限、加上远程到了，同样要停用。
func TestDisableInvalidInboundsUsesAggregateUsed(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	ib := &InboundService{}
	db := database.GetDB()
	const total = 1000
	if err := db.Create(&model.Inbound{Port: 9201, Up: 600, Total: total, Enable: true, Tag: "inbound-9201"}).Error; err != nil {
		t.Fatal(err)
	}
	// 另有一个本地就已超限的账号，改造前后的行为必须一致。
	if err := db.Create(&model.Inbound{Port: 9202, Up: 1500, Total: total, Enable: true, Tag: "inbound-9202"}).Error; err != nil {
		t.Fatal(err)
	}
	// 健康账号：无论本地还是汇总都没到上限。
	if err := db.Create(&model.Inbound{Port: 9203, Up: 100, Total: total, Enable: true, Tag: "inbound-9203"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTrafficCache([]*entity.ServerTraffic{{Port: 9201, Up: 400, Used: 400, Total: total, Enable: true}}); err != nil {
		t.Fatal(err)
	}

	count, err := ib.DisableInvalidInbounds()
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("应停用 2 条（本地超限 + 汇总超限），得到 %d", count)
	}
	if inboundBy(t, 9201).Enable {
		t.Error("汇总超限的 9201 没有被停用")
	}
	if inboundBy(t, 9202).Enable {
		t.Error("本地超限的 9202 没有被停用")
	}
	if !inboundBy(t, 9203).Enable {
		t.Error("健康账号 9203 被误停用")
	}
	// 停用是幂等的：第二轮不应再产生变更。
	if count, err = ib.DisableInvalidInbounds(); err != nil || count != 0 {
		t.Fatalf("重复执行应无变更，得到 count=%d err=%v", count, err)
	}
}

// 月初清零要把上个自然月因超限被停用的账号恢复启用，
// 同时不能误伤管理员手动停用的、也不能放出已到期的。
func TestMonthlyResetReactivatesOverlimitAccounts(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	db := database.GetDB()
	const total = 1000
	// 清零发生在 2026-10-01，这个到期时间已经过去。
	expired := time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local).Unix() * 1000
	seeds := []struct {
		port   int
		up     int64
		enable bool
		expiry int64
		want   bool
		why    string
	}{
		{9301, 1500, false, 0, true, "本地已超限，上月被自动停用"},
		{9302, 100, false, 0, false, "管理员手动停用，未超限"},
		{9303, 1500, false, expired, false, "已到期，不能因超限被放出来"},
		{9304, 1500, true, 0, true, "本来就处于启用状态"},
		{9305, 0, false, 0, false, "零用量，未超限"},
		{9306, 500, false, 0, true, "本地没超，远程补足到上限"},
		{9307, 500, true, 0, true, "远程补足超限，但仍启用着"},
	}
	for _, v := range seeds {
		in := &model.Inbound{
			MonthlyReset: true,
			Port:         v.port, Up: v.up, Total: total, Enable: v.enable, ExpiryTime: v.expiry,
			Tag: fmt.Sprintf("inbound-%d", v.port), Remark: fmt.Sprintf("客户%d", v.port),
		}
		if err := db.Create(in).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveTrafficCache([]*entity.ServerTraffic{
		{Port: 9306, Up: 600, Used: 600, Total: total, Enable: true},
		{Port: 9307, Up: 600, Used: 600, Total: total, Enable: true},
	}); err != nil {
		t.Fatal(err)
	}

	// 首次运行只登记月份，不改动任何数据。
	if err := s.maybeMonthlyResetAt(time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	if inboundBy(t, 9301).Enable {
		t.Fatal("首次登记阶段不应改动入站状态")
	}

	if err := s.maybeMonthlyResetAt(time.Date(2026, 10, 1, 0, 0, 1, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	for _, v := range seeds {
		in := inboundBy(t, v.port)
		if in.Enable != v.want {
			t.Errorf("端口 %d（%s）清零后 enable = %v，期望 %v", v.port, v.why, in.Enable, v.want)
		}
		if in.Up != 0 || in.Down != 0 {
			t.Errorf("端口 %d 用量未清零: up=%d down=%d", v.port, in.Up, in.Down)
		}
		if in.Total != total {
			t.Errorf("端口 %d 的套餐上限被改动: %d", v.port, in.Total)
		}
	}

	// 留档记的仍是清零前的用量，不能因为顺带恢复了启用就把账抹成 0。
	rows, err := s.TrafficResetSnapshots(0)
	if err != nil {
		t.Fatal(err)
	}
	byPort := make(map[int]*entity.TrafficSnapshot, len(rows))
	for _, r := range rows {
		byPort[r.Port] = r
	}
	if got := byPort[9301]; got == nil || got.Local != 1500 || got.Used != 1500 {
		t.Fatalf("9301 的留档应为清零前的 1500: %+v", got)
	}
	if got := byPort[9306]; got == nil || got.Local != 500 || got.Remote != 600 || got.Used != 1100 {
		t.Fatalf("9306 的留档应含远程用量: %+v", got)
	}
	// 流程跑完后不再需要这份待启用名单。
	st, err := s.getTrafficResetState()
	if err != nil {
		t.Fatal(err)
	}
	if st.ConfirmedMonth != 202610 || !st.LocalDone || !st.RemoteDone {
		t.Fatalf("清零进度异常: %+v", st)
	}
	if len(st.ReactivatePorts) != 0 {
		t.Fatalf("流程结束后应清空待启用名单: %v", st.ReactivatePorts)
	}
}

// 待恢复启用的端口必须与进度一起落盘：远程清零失败要重试时，
// 本地用量已经归零，再也推不出该启用谁，只能靠这份记录。
func TestResetStatePersistsReactivatePorts(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	db := database.GetDB()
	if err := db.Create(&model.Inbound{MonthlyReset: true, Port: 9401, Up: 2000, Total: 1000, Enable: false, Tag: "inbound-9401"}).Error; err != nil {
		t.Fatal(err)
	}
	// 手动停用的账号不能被记进待启用名单。
	if err := db.Create(&model.Inbound{MonthlyReset: true, Port: 9402, Up: 10, Total: 1000, Enable: false, Tag: "inbound-9402"}).Error; err != nil {
		t.Fatal(err)
	}
	// 先登记首次运行（只存进度，不清零），再跨月触发真正的清零。
	if err := s.maybeMonthlyResetAt(time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	st := &trafficResetState{ConfirmedMonth: 202609, PendingMonth: 202610}
	if err := s.snapshotAndResetLocal(st, 202609, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	if len(st.ReactivatePorts) != 1 || st.ReactivatePorts[0] != 9401 {
		t.Fatalf("待启用名单异常: %v", st.ReactivatePorts)
	}
	if !st.LocalDone {
		t.Fatal("本地阶段完成后 LocalDone 应为真")
	}
	stored, err := s.getTrafficResetState()
	if err != nil {
		t.Fatal(err)
	}
	if !stored.LocalDone || len(stored.ReactivatePorts) != 1 || stored.ReactivatePorts[0] != 9401 {
		t.Fatalf("清零进度未与数据一起落盘: %+v", stored)
	}
}

// 远程脚本必须“先恢复启用、再清零”：清零后用量归零，超限条件就不再成立了。
// 清零按端口名单而不是全表——未勾选“按月计算”的账号要一直累计下去，不能被抹掉。
func TestRemoteMonthlyResetSQL(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 1, 0, time.Local)
	// 按月端口 9001/9301/9306，其中 9301、9306 因超限被停用需要恢复启用。
	sql := remoteMonthlyResetSQL([]int{9001, 9301, 9306}, []int{9301, 9306}, now)
	enableAt := strings.Index(sql, "SET enable=1")
	resetAt := strings.Index(sql, "SET up=0, down=0")
	if enableAt < 0 || resetAt < 0 {
		t.Fatalf("远程脚本缺少启用或清零语句:\n%s", sql)
	}
	if enableAt > resetAt {
		t.Fatalf("启用语句必须排在清零之前:\n%s", sql)
	}
	for _, want := range []string{
		"SET enable=1 WHERE enable=0 AND (expiry_time=0 OR expiry_time>" + strconv.FormatInt(now.Unix()*1000, 10) + ") AND port IN (9301,9306)",
		"SET up=0, down=0 WHERE port IN (9001,9301,9306)",
		"BEGIN IMMEDIATE",
		"COMMIT",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("远程脚本缺少 %q:\n%s", want, sql)
		}
	}
	// 反向确认没有退化成全表清零。
	if strings.Contains(sql, "down=0;\nSELECT") {
		t.Errorf("清零语句缺少端口条件，等于全表清零:\n%s", sql)
	}
	// 非法端口号被剔除，不拼进 SQL；只剩非法值时退化成不匹配任何行。
	got := remoteMonthlyResetSQL([]int{0, -1, 70000}, []int{0, -1, 70000}, now)
	if strings.Contains(got, "SET enable=1") {
		t.Errorf("非法端口不应生成启用语句:\n%s", got)
	}
	if !strings.Contains(got, "SET up=0, down=0 WHERE 0;\nSELECT changes();") {
		t.Errorf("按月名单为空时应退化成不匹配任何行:\n%s", got)
	}
	// 没有待启用端口时只清零。
	empty := remoteMonthlyResetSQL([]int{9001}, nil, now)
	if strings.Contains(empty, "SET enable=1") {
		t.Errorf("无待启用端口时不应有启用语句:\n%s", empty)
	}
	// 调用方靠 SELECT changes() 校验清零行数，它必须紧跟清零语句。
	if !strings.Contains(empty, "SET up=0, down=0 WHERE port IN (9001);\nSELECT changes();") {
		t.Errorf("changes() 未紧跟清零语句:\n%s", empty)
	}
}

// 老版本写下的清零进度没有 reactivatePorts 字段，读出来应当是该字段为空而非报错。
func TestTrafficResetStateBackwardCompatible(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(&model.Setting{
		Key:   trafficResetStateKey,
		Value: `{"confirmedMonth":202609,"pendingMonth":202610,"localDone":false,"remoteDone":false}`,
	}).Error; err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	st, err := s.getTrafficResetState()
	if err != nil {
		t.Fatalf("旧格式进度应能正常解析: %v", err)
	}
	if st.ConfirmedMonth != 202609 || st.PendingMonth != 202610 {
		t.Fatalf("旧格式进度解析异常: %+v", st)
	}
	if len(st.ReactivatePorts) != 0 {
		t.Fatalf("旧格式没有该字段时应为空: %v", st.ReactivatePorts)
	}
}
