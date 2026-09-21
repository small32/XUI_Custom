package service

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/web/entity"
)

func TestRemoteDisableMarkerFailureRollsBack(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 required")
	}
	path := filepath.Join(t.TempDir(), "remote.db")
	setup := "CREATE TABLE inbounds(port INTEGER,enable INTEGER); INSERT INTO inbounds VALUES(1234,1); CREATE TABLE settings(key TEXT,value TEXT); CREATE TRIGGER fail_marker BEFORE INSERT ON settings BEGIN SELECT RAISE(ABORT,'marker failed'); END;"
	if out, err := exec.Command("sqlite3", path, setup).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	command := strings.ReplaceAll(remoteDisableCommand("1234"), "/etc/x-ui/x-ui.db", path)
	if _, err := exec.Command("sh", "-c", command).CombinedOutput(); err == nil {
		t.Fatal("expected marker failure")
	}
	out, err := exec.Command("sqlite3", path, "SELECT enable FROM inbounds;").Output()
	if err != nil || string(out) != "1\n" {
		t.Fatalf("disable committed without marker: %s %v", out, err)
	}
}

func TestMonthlyResetClearsCachedCounters(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "local.db")); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	if err := db.Create(&model.Setting{Key: serverManagementSettingKey, Value: `{"autoDisable":true}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Inbound{Port: 1234, Tag: "monthly", MonthlyReset: true, Total: 100, Enable: false}).Error; err != nil {
		t.Fatal(err)
	}
	s := new(ServerManagementService)
	if err := s.SaveTrafficCache([]*entity.ServerTraffic{{Port: 1234, Up: 100, Used: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := s.snapshotAndResetLocal(&trafficResetState{}, 202609, time.Now()); err != nil {
		t.Fatal(err)
	}
	if count, err := new(InboundService).DisableInvalidInbounds(); err != nil || count != 0 {
		t.Fatalf("restored account disabled: %d %v", count, err)
	}
	if !inboundBy(t, 1234).Enable {
		t.Fatal("account not restored")
	}
}

func TestServerChangeRollsBackIfCacheCannotBeCleared(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "local.db")); err != nil {
		t.Fatal(err)
	}
	s := new(ServerManagementService)
	setting := &entity.ServerSetting{Host: "old.example", Username: "root", Port: 22, HeartbeatMinutes: 10}
	if err := s.SaveSetting(setting); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTrafficCache([]*entity.ServerTraffic{{Port: 1234, Used: 100}}); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_cache_delete BEFORE DELETE ON settings WHEN OLD.key='serverTrafficCache' BEGIN SELECT RAISE(ABORT,'test failure'); END;").Error; err != nil {
		t.Fatal(err)
	}
	setting.Host = "new.example"
	if err := s.SaveSetting(setting); err == nil {
		t.Fatal("cache deletion error ignored")
	}
	got, err := s.GetSetting()
	if err != nil || got.Host != "old.example" {
		t.Fatalf("setting committed with stale cache: %+v %v", got, err)
	}
	if err := db.Exec("DROP TRIGGER reject_cache_delete").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSetting(setting); err != nil {
		t.Fatal(err)
	}
	cache, err := s.GetTrafficCache()
	if err != nil || len(cache) != 0 {
		t.Fatalf("old cache retained: %+v %v", cache, err)
	}
}
