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

func TestPendingResetNotTransferredToNewServer(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "local.db")); err != nil {
		t.Fatal(err)
	}
	s := new(ServerManagementService)
	v := &entity.ServerSetting{Host: "old.example", Username: "root", Port: 22, HeartbeatMinutes: 10}
	if err := s.SaveSetting(v); err != nil {
		t.Fatal(err)
	}
	st := &trafficResetState{ConfirmedMonth: 202609, PendingMonth: 202610, LocalDone: true, MonthlyPorts: []int{1234}, ReactivatePorts: []int{1234}, RemoteIdentity: remoteIdentity(v)}
	if err := s.saveTrafficResetState(st); err != nil {
		t.Fatal(err)
	}
	v.Host = "new.example"
	if err := s.SaveSetting(v); err != nil {
		t.Fatal(err)
	}
	st, err := s.getTrafficResetState()
	if err != nil || !st.RemoteDone {
		t.Fatalf("old server task still active: %+v %v", st, err)
	}
	if err := s.maybeMonthlyResetAt(time.Date(2026, 10, 2, 0, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	st, err = s.getTrafficResetState()
	if err != nil || st.ConfirmedMonth != 202610 || len(st.MonthlyPorts) != 0 {
		t.Fatalf("task not finalized: %+v %v", st, err)
	}
}

func TestPendingResetAcrossMonthsPreservesArchive(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "local.db")); err != nil {
		t.Fatal(err)
	}
	s := new(ServerManagementService)
	in := &model.Inbound{Port: 1234, Tag: "monthly", MonthlyReset: true, Enable: true, Up: 100, Total: 1000}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatal(err)
	}
	st := &trafficResetState{ConfirmedMonth: 202609, PendingMonth: 202610}
	if err := s.snapshotAndResetLocal(st, 202609, time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(in).Update("up", 23).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.maybeMonthlyResetAt(time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	var snapshots []model.TrafficSnapshot
	if err := database.GetDB().Order("yyyymm").Find(&snapshots).Error; err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].Yyyymm != 202609 || snapshots[0].LocalUp != 100 || snapshots[1].Yyyymm != 202610 || snapshots[1].LocalUp != 23 {
		t.Fatalf("archives corrupted: %+v", snapshots)
	}
}

func TestSyncDeletionRestartRetry(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 required")
	}
	db := filepath.Join(t.TempDir(), "remote.db")
	sql := "CREATE TABLE settings(key TEXT,value TEXT); CREATE TABLE inbounds(port INTEGER); INSERT INTO inbounds VALUES(1234);"
	if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	apply := func() {
		t.Helper()
		cmd := exec.Command("sqlite3", "-bail", db)
		cmd.Stdin = strings.NewReader(withSyncReloadIntent(deleteSyncedInboundSQL(1234)))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	apply()
	// First restart fails; repeat deletion is a no-op but must preserve intent.
	command := strings.ReplaceAll(syncReloadCommand, "/etc/x-ui/x-ui.db", db)
	command = strings.ReplaceAll(command, "systemctl restart x-ui", "false")
	if err := exec.Command("sh", "-c", command).Run(); err == nil {
		t.Fatal("expected restart failure")
	}
	apply()
	out, err := exec.Command("sqlite3", db, "SELECT count(*) FROM settings WHERE key='syncReloadPending';").Output()
	if err != nil || string(out) != "1\n" {
		t.Fatalf("retry intent lost: %s %v", out, err)
	}
	command = strings.ReplaceAll(command, "false || exit $?", "true || exit $?")
	if err := exec.Command("sh", "-c", command).Run(); err != nil {
		t.Fatal(err)
	}
	out, err = exec.Command("sqlite3", db, "SELECT count(*) FROM settings WHERE key='syncReloadPending';").Output()
	if err != nil || string(out) != "0\n" {
		t.Fatalf("retry not acknowledged: %s %v", out, err)
	}
}
