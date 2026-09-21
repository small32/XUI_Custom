package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"

	"x-ui/database"
	"x-ui/database/model"
	"x-ui/web/entity"
	"x-ui/web/session"
)

// 定时表达式写错会让任务静默失效，因此按真实解析器校验触发时刻。
func TestMonthlyResetCronSpec(t *testing.T) {
	parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	schedule, err := parser.Parse(monthlyResetCronSpec)
	if err != nil {
		t.Fatalf("表达式 %q 无法解析: %v", monthlyResetCronSpec, err)
	}
	cases := []struct {
		from time.Time
		want time.Time
	}{
		{time.Date(2026, 9, 21, 12, 0, 0, 0, time.Local), time.Date(2026, 10, 1, 0, 0, 1, 0, time.Local)},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.Local), time.Date(2027, 1, 1, 0, 0, 1, 0, time.Local)},
		{time.Date(2026, 10, 1, 0, 0, 1, 0, time.Local), time.Date(2026, 11, 1, 0, 0, 1, 0, time.Local)},
	}
	for _, c := range cases {
		if next := schedule.Next(c.from); !next.Equal(c.want) {
			t.Errorf("从 %v 触发时间 = %v, 期望 %v", c.from, next, c.want)
		}
	}
	if _, err = parser.Parse(monthlyResetFallbackSpec); err != nil {
		t.Fatalf("兜底表达式 %q 无法解析: %v", monthlyResetFallbackSpec, err)
	}
}

// 走真实 HTTP 编解码，确认页面拿到的 JSON 字段与列绑定一致。
func TestTrafficSummaryHTTP(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(&model.Inbound{MonthlyReset: true, Port: 3001, Up: 1 << 20, Down: 1 << 19, Total: 1 << 30, Remark: "客户B", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(sessions.Sessions("session", cookie.NewStore([]byte("test-secret"))))
	engine.POST("/xui/traffic-summary/list", (&ServerManagementController{}).summary)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/xui/traffic-summary/list", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", w.Code, w.Body.String())
	}
	var msg struct {
		Success bool                     `json:"success"`
		Obj     []*entity.TrafficSummary `json:"obj"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("响应解析失败: %v: %s", err, w.Body.String())
	}
	if !msg.Success || len(msg.Obj) != 1 {
		t.Fatalf("响应异常: %s", w.Body.String())
	}
	row := msg.Obj[0]
	if row.Port != 3001 || row.LocalText != "1.50 MB" || row.LimitText != "1.00 GB" {
		t.Fatalf("字段异常: %+v", row)
	}
	// 计费方式要带出去：页面靠它显示“按月／累计”，也靠它决定快照弹窗的提示。
	if !row.MonthlyReset {
		t.Fatalf("汇总未带出按月标记: %+v", row)
	}
	if body := w.Body.String(); !strings.Contains(body, `"localText":"1.50 MB"`) ||
		!strings.Contains(body, `"totalText":"1.50 MB"`) ||
		!strings.Contains(body, `"monthlyReset":true`) {
		t.Fatalf("JSON 字段名与页面不一致: %s", body)
	}
}

// 受限登录只能看到绑定入站那一行，且清零记录接口不会暴露给受限登录。
func TestTrafficSummaryHTTPRestricted(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	boundId := 0
	for _, port := range []int{4001, 4002} {
		in := &model.Inbound{MonthlyReset: true, Port: port, Up: 2048, Total: 1 << 20, Tag: fmt.Sprintf("inbound-%d", port), Remark: "客户", Enable: true}
		if err := database.GetDB().Create(in).Error; err != nil {
			t.Fatal(err)
		}
		// 受限登录绑定的是入站主键 id，不是端口号。
		if port == 4002 {
			boundId = in.Id
		}
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(sessions.Sessions("session", cookie.NewStore([]byte("test-secret"))))
	engine.Use(func(c *gin.Context) {
		if err := session.SetLoginInboundId(c, boundId); err != nil {
			t.Fatal(err)
		}
		c.Next()
	})
	engine.POST("/xui/traffic-summary/list", (&ServerManagementController{}).summary)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/xui/traffic-summary/list", nil))
	var msg struct {
		Success bool                     `json:"success"`
		Obj     []*entity.TrafficSummary `json:"obj"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("响应解析失败: %v: %s", err, w.Body.String())
	}
	if len(msg.Obj) != 1 || msg.Obj[0].Port != 4002 {
		t.Fatalf("受限登录看到了非绑定入站: %s", w.Body.String())
	}
	if msg.Obj[0].LocalText != "2.00 KB" {
		t.Fatalf("展示文本异常: %+v", msg.Obj[0])
	}
}

// 留档接口：受限登录无论传什么 inboundId，都只能拿到自己账号的记录；
// 管理员可以按账号取单条，也可以不传取全量。
func TestTrafficSnapshotsHTTP(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	var boundId, otherId int
	for _, port := range []int{4101, 4102} {
		in := &model.Inbound{MonthlyReset: true, Port: port, Up: 1024, Total: 1 << 20, Tag: fmt.Sprintf("inbound-%d", port), Remark: "客户", Enable: true}
		if err := database.GetDB().Create(in).Error; err != nil {
			t.Fatal(err)
		}
		if port == 4101 {
			otherId = in.Id
		} else {
			boundId = in.Id
		}
	}
	for _, v := range []struct {
		id   int
		port int
	}{{boundId, 4102}, {otherId, 4101}} {
		if err := database.GetDB().Create(&model.TrafficSnapshot{
			Yyyymm: 202609, InboundId: v.id, Port: v.port, Remark: "客户",
			LocalUp: 1024, Total: 1 << 20, ResetAt: 1700000000,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	gin.SetMode(gin.TestMode)

	// 管理员：按账号取单条，不传则取全量。
	// 会话中间件必须装上——取登录绑定入站时会话不存在会直接 panic。
	admin := gin.New()
	admin.Use(sessions.Sessions("session", cookie.NewStore([]byte("test-secret"))))
	admin.POST("/xui/traffic-summary/snapshots", (&ServerManagementController{}).resetSnapshots)
	rows := postSnapshots(t, admin, "inboundId="+strconv.Itoa(otherId))
	if len(rows) != 1 || rows[0].InboundId != otherId || rows[0].Port != 4101 || rows[0].Yyyymm != 202609 {
		t.Fatalf("管理员按账号取留档异常: %+v", rows)
	}
	if rows = postSnapshots(t, admin, ""); len(rows) != 2 {
		t.Fatalf("管理员应看到全部留档，得到 %+v", rows)
	}

	// 受限登录：即使请求体里塞别人的 id，也只返回自己账号那一条。
	restricted := gin.New()
	restricted.Use(sessions.Sessions("session", cookie.NewStore([]byte("test-secret"))))
	restricted.Use(func(c *gin.Context) {
		if err := session.SetLoginInboundId(c, boundId); err != nil {
			t.Fatal(err)
		}
		c.Next()
	})
	restricted.POST("/xui/traffic-summary/snapshots", (&ServerManagementController{}).resetSnapshots)
	for _, body := range []string{"", "inboundId=" + strconv.Itoa(otherId)} {
		rows = postSnapshots(t, restricted, body)
		if len(rows) != 1 || rows[0].InboundId != boundId || rows[0].Port != 4102 {
			t.Fatalf("受限登录越权拿到了他人留档（body=%q）: %+v", body, rows)
		}
	}
}

// postSnapshots 走真实 HTTP 编解码调一次留档接口。
func postSnapshots(t *testing.T, engine *gin.Engine, body string) []*entity.TrafficSnapshot {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/xui/traffic-summary/snapshots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	var msg struct {
		Success bool                      `json:"success"`
		Obj     []*entity.TrafficSnapshot `json:"obj"`
	}
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("响应解析失败: %v: %s", err, raw)
	}
	if !msg.Success {
		t.Fatalf("请求失败: %s", raw)
	}
	return msg.Obj
}
