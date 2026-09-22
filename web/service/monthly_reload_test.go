package service

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"x-ui/database"
	"x-ui/database/model"
)

func TestMonthlyResetRequestsLocalReload(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "local.db")); err != nil {
		t.Fatal(err)
	}
	x := new(XrayService)
	x.IsNeedRestartAndSetFalse()
	defer x.IsNeedRestartAndSetFalse()
	if err := database.GetDB().Create(&model.Inbound{Port: 12345, Tag: "test", MonthlyReset: true, Enable: false, DisabledBy: "limit", Up: 100, Total: 100}).Error; err != nil {
		t.Fatal(err)
	}
	s := new(ServerManagementService)
	if err := s.snapshotAndResetLocal(&trafficResetState{}, 202609, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !x.IsNeedRestartAndSetFalse() {
		t.Fatal("reactivation must reload Xray")
	}
	if err := s.snapshotAndResetLocal(&trafficResetState{}, 202610, time.Now()); err != nil {
		t.Fatal(err)
	}
	if x.IsNeedRestartAndSetFalse() {
		t.Fatal("traffic reset alone must not reload")
	}
}

func TestRemoteMonthlyReloadIntent(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 required")
	}
	db := filepath.Join(t.TempDir(), "remote.db")
	run := func(sql string) string {
		t.Helper()
		cmd := exec.Command("sqlite3", "-bail", db)
		cmd.Stdin = strings.NewReader(sql)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	run("CREATE TABLE settings(key TEXT,value TEXT); CREATE TABLE inbounds(port INTEGER,enable INTEGER,expiry_time INTEGER,up INTEGER,down INTEGER); INSERT INTO inbounds VALUES(12345,0,0,100,0);")
	for i := 0; i < 2; i++ {
		run(remoteMonthlyResetSQL([]int{12345}, []int{12345, 23456}, time.Now()))
		if got := run("SELECT count(*) FROM settings WHERE key='monthlyResetReloadPending';"); got != "1\n" {
			t.Fatal("missing or duplicated retry marker", got)
		}
	}
	run("UPDATE inbounds SET up=37;")
	run(remoteMonthlyResetSQL([]int{12345}, []int{12345}, time.Now()))
	if got := run("SELECT up FROM inbounds;"); got != "37\n" {
		t.Fatal("retry erased new traffic", got)
	}
	run("DELETE FROM settings WHERE key='monthlyResetReloadPending';")
	run(remoteMonthlyResetSQL([]int{12345}, []int{12345, 23456}, time.Now()))
	if got := run("SELECT count(*) FROM settings WHERE key='monthlyResetReloadPending';"); got != "0\n" {
		t.Fatal("unchanged ports must not reload", got)
	}
	if got := run("SELECT count(*) FROM inbounds;"); got != "1\n" {
		t.Fatal("deleted port recreated", got)
	}
}
