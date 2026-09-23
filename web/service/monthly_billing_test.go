package service

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/web/entity"
)

// 流量上限为 0 表示不限制，展示上不能写成 “0 B”，否则会被当成“一点流量都没有”。
func TestFormatTrafficLimit(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "无限制"},
		{-1, "无限制"},
		{1, "1 B"},
		{1024, "1.00 KB"},
		{1 << 30, "1.00 GB"},
	}
	for _, c := range cases {
		if got := FormatTrafficLimit(c.in); got != c.want {
			t.Errorf("FormatTrafficLimit(%d) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// 远程清零按“按月端口名单”进行，名单必须与本地被清零的那批端口完全一致，
// 且不能把未勾选“按月计算”的账号带进去。
func TestResetStatePersistsMonthlyPorts(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	db := database.GetDB()
	for _, v := range []struct {
		port    int
		monthly bool
	}{{9601, true}, {9602, false}, {9603, true}} {
		in := &model.Inbound{
			MonthlyReset: v.monthly, Port: v.port, Up: 100, Total: 1000, Enable: true,
			Tag: fmt.Sprintf("inbound-%d", v.port), Remark: fmt.Sprintf("客户%d", v.port),
		}
		if err := db.Create(in).Error; err != nil {
			t.Fatal(err)
		}
	}

	// 先登记首次运行（只存进度，不清零），再跨月触发真正的清零。
	if err := s.maybeMonthlyResetAt(time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	st := &trafficResetState{ConfirmedMonth: 202609, PendingMonth: 202610}
	if err := s.snapshotAndResetLocal(st, 202609, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	if len(st.MonthlyPorts) != 2 || st.MonthlyPorts[0] != 9601 || st.MonthlyPorts[1] != 9603 {
		t.Fatalf("按月端口名单应只含勾选账号: %v", st.MonthlyPorts)
	}
	if len(st.ReactivatePorts) != 0 {
		t.Fatalf("没有超限停用的账号，不该有待启用端口: %v", st.ReactivatePorts)
	}
	stored, err := s.getTrafficResetState()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.MonthlyPorts) != 2 || stored.MonthlyPorts[1] != 9603 {
		t.Fatalf("按月端口名单未与进度一起落盘: %+v", stored)
	}

	if got := inboundUsed(t, 9601); got != 0 {
		t.Errorf("按月账号未清零: %d", got)
	}
	if got := inboundUsed(t, 9602); got != 100 {
		t.Errorf("未勾选“按月计算”的账号被清零了: %d", got)
	}
	rows, err := s.TrafficResetSnapshots(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("留档应只覆盖 2 个按月账号: %+v", rows)
	}
	for _, r := range rows {
		if r.Port == 9602 {
			t.Fatalf("未按月账号不应有留档: %+v", r)
		}
	}
}

// “按月计算”只作用于勾选的账号：未勾选的用量累计到底，
// 月初既不清零也不恢复启用（流量用完即止）。
func TestTrafficResetOnlyAffectsMonthlyInbounds(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	db := database.GetDB()
	const total = 1000
	seeds := []struct {
		port       int
		monthly    bool
		up         int64
		enable     bool
		disabledBy string
		remark     string
	}{
		{9501, true, 1500, false, "limit", "按月：已超限被停用"},
		{9502, false, 1500, false, "limit", "累计：已超限被停用"},
		{9503, false, 400, true, "", "累计：在用"},
	}
	for _, v := range seeds {
		in := &model.Inbound{
			MonthlyReset: v.monthly, Port: v.port, Up: v.up, Total: total, Enable: v.enable, DisabledBy: v.disabledBy,
			Tag: fmt.Sprintf("inbound-%d", v.port), Remark: v.remark,
		}
		if err := db.Create(in).Error; err != nil {
			t.Fatal(err)
		}
	}

	// 首次运行只登记月份，不动数据。
	if err := s.maybeMonthlyResetAt(time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	// 跨月清零。
	if err := s.maybeMonthlyResetAt(time.Date(2026, 10, 1, 0, 0, 1, 0, time.Local)); err != nil {
		t.Fatal(err)
	}

	// 按月账号：清零、因超限被停用的恢复启用，套餐上限不动。
	if in := inboundBy(t, 9501); in.Up != 0 || in.Down != 0 || !in.Enable || in.Total != total {
		t.Errorf("按月账号应清零并恢复启用: %+v", in)
	}
	// 累计账号：一个字节都不能动；超限停用的也不放出来——流量用完即止。
	if in := inboundBy(t, 9502); in.Up != 1500 || in.Enable {
		t.Errorf("累计账号超限停用后不应被月初清零或恢复启用: %+v", in)
	}
	if in := inboundBy(t, 9503); in.Up != 400 || !in.Enable {
		t.Errorf("累计在用账号的用量不应被月度清零: %+v", in)
	}

	// 留档只覆盖按月账号，且记的是清零前的值。
	rows, err := s.TrafficResetSnapshots(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Port != 9501 || rows[0].Local != 1500 {
		t.Fatalf("留档应只覆盖按月账号且记清零前的值: %+v", rows)
	}

	// 流程跑完后两份名单都不该残留（已随进度落盘、供远程阶段用完后清空）。
	st, err := s.getTrafficResetState()
	if err != nil {
		t.Fatal(err)
	}
	if st.ConfirmedMonth != 202610 || !st.LocalDone || !st.RemoteDone {
		t.Fatalf("清零进度异常: %+v", st)
	}
	if len(st.MonthlyPorts) != 0 || len(st.ReactivatePorts) != 0 {
		t.Fatalf("流程结束后应清空端口名单: monthly=%v reactivate=%v", st.MonthlyPorts, st.ReactivatePorts)
	}
}

// 一个按月账号都没有时，远程阶段不必连 SSH，也不能因此卡住清零进度。
func TestMonthlyResetWithoutMonthlyAccountsSkipsRemote(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	// 故意配上一个连不通的远程地址：只要远程阶段真去连了，这条测试就会失败。
	if err := s.SaveSetting(&entity.ServerSetting{
		Name: "第三方服务器", Host: "127.0.0.1", Port: 1, Username: "root", Password: "x",
		SyncStrategy: "normal", HeartbeatMinutes: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(&model.Inbound{
		Port: 9701, Up: 100, Total: 1000, Enable: true, Tag: "inbound-9701",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.maybeMonthlyResetAt(time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	if err := s.maybeMonthlyResetAt(time.Date(2026, 10, 1, 0, 0, 1, 0, time.Local)); err != nil {
		t.Fatalf("没有按月账号时不该去连远程: %v", err)
	}
	if got := inboundUsed(t, 9701); got != 100 {
		t.Fatalf("未按月账号的用量不应被清零: %d", got)
	}
	st, err := s.getTrafficResetState()
	if err != nil {
		t.Fatal(err)
	}
	if st.ConfirmedMonth != 202610 || !st.RemoteDone {
		t.Fatalf("清零进度未确认: %+v", st)
	}
	// 首次登记会先存一次设置读取结果，这里确认没有留下任何待办名单。
	if len(st.MonthlyPorts) != 0 || len(st.ReactivatePorts) != 0 {
		t.Fatalf("不该有待处理端口: monthly=%v reactivate=%v", st.MonthlyPorts, st.ReactivatePorts)
	}
}
