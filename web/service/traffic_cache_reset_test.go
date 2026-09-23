package service

import (
	"path/filepath"
	"testing"
	"x-ui/database"
	"x-ui/web/entity"
)

// 删除账号后必须清掉该端口的流量缓存条目，否则端口被新账号复用时
// 会继承旧账号的缓存用量，在下一次心跳前被自动禁用。
func TestResetPortTrafficCacheRemovesPortEntry(t *testing.T) {
	forceHeartbeat = func() {} // 测试中不真正执行 SSH 心跳
	if err := database.InitDB(filepath.Join(t.TempDir(), "cache-reset.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	old := []*entity.ServerTraffic{
		{Port: 1000, Up: 100, Down: 200, Used: 300, Total: 500, Enable: true},
		{Port: 2000, Up: 1, Down: 2, Used: 3, Total: 0, Enable: true},
	}
	if err := s.SaveTrafficCache(old); err != nil {
		t.Fatal(err)
	}

	s.ResetPortTrafficCache(1000)

	cache, err := s.GetTrafficCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(cache) != 1 || cache[0].Port != 2000 {
		t.Fatalf("端口 1000 的缓存条目应被清除，实际: %+v", cache)
	}

	// 重复清理应幂等，不影响其他端口
	s.ResetPortTrafficCache(1000)
	cache, err = s.GetTrafficCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(cache) != 1 || cache[0].Port != 2000 {
		t.Fatalf("重复清理后缓存应保持不变，实际: %+v", cache)
	}
}

// 缓存为空时清理应无副作用。
func TestResetPortTrafficCacheEmptyCache(t *testing.T) {
	forceHeartbeat = func() {}
	if err := database.InitDB(filepath.Join(t.TempDir(), "cache-reset-empty.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	s.ResetPortTrafficCache(3000)
	cache, err := s.GetTrafficCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(cache) != 0 {
		t.Fatalf("空缓存清理后应仍为空，实际: %+v", cache)
	}
}

func TestResetPortsTrafficCacheRemovesAllMovedPortEntries(t *testing.T) {
	forceHeartbeat = func() {}
	if err := database.InitDB(filepath.Join(t.TempDir(), "cache-reset-multiple.db")); err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	if err := s.SaveTrafficCache([]*entity.ServerTraffic{
		{Port: 1001, Used: 10}, {Port: 1002, Used: 20}, {Port: 1003, Used: 30},
	}); err != nil {
		t.Fatal(err)
	}
	s.ResetPortsTrafficCache(1001, 1003)
	cache, err := s.GetTrafficCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(cache) != 1 || cache[0].Port != 1002 {
		t.Fatalf("old and new port cache entries should both be reset: %+v", cache)
	}
}
