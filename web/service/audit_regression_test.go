package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/web/entity"
)

func TestUpdateInboundDoesNotOverwriteConcurrentTraffic(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "inbound-update.db")); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	in := &model.Inbound{Port: 12000, Tag: "inbound-12000", Protocol: model.Trojan, Settings: `{}`, StreamSettings: `{}`, Sniffing: `{}`, Enable: true, Up: 100, Down: 200}
	if err := db.Create(in).Error; err != nil {
		t.Fatal(err)
	}
	const callbackName = "test:inject_traffic_before_edit_save"
	injected := false
	if err := db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if injected {
			return
		}
		loaded, ok := tx.Statement.Dest.(*model.Inbound)
		if !ok || loaded.Id == 0 {
			return
		}
		injected = true
		if err := tx.Exec("UPDATE inbounds SET up=up+50 WHERE id=?", loaded.Id).Error; err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove(callbackName)

	edit := *in
	edit.Remark = "edited"
	if err := new(InboundService).UpdateInbound(&edit); err != nil {
		t.Fatal(err)
	}
	var got model.Inbound
	if err := db.Unscoped().First(&got, in.Id).Error; err != nil {
		t.Fatal(err)
	}
	if !injected {
		t.Fatal("concurrent traffic update was not injected")
	}
	if got.Up != 150 || got.Down != 200 {
		t.Fatalf("edit overwrote concurrent traffic: up=%d down=%d", got.Up, got.Down)
	}
}

func TestTrafficSnapshotRejectsChangedServerAndMonth(t *testing.T) {
	server := &entity.ServerSetting{Host: "a.example", Port: 22, Username: "root"}
	state := &trafficResetState{ConfirmedMonth: 202609}
	if err := validateTrafficSnapshot(server, server, state, &trafficResetState{ConfirmedMonth: 202609}); err != nil {
		t.Fatalf("unchanged snapshot rejected: %v", err)
	}
	changedServer := *server
	changedServer.Host = "b.example"
	if err := validateTrafficSnapshot(server, &changedServer, state, state); err == nil {
		t.Fatal("old server result was accepted after server replacement")
	}
	if err := validateTrafficSnapshot(server, server, state, &trafficResetState{ConfirmedMonth: 202610}); err == nil {
		t.Fatal("old month result was accepted after reset state changed")
	}
	changedSetting := *server
	changedSetting.AutoDisable = false
	if err := validateTrafficSnapshot(server, &changedSetting, state, state); err != nil {
		t.Fatalf("toggling auto-disable should keep same-server read usable: %v", err)
	}
}

func TestRemoteSSHAuthModes(t *testing.T) {
	passwordCfg, err := remoteSSHClientConfig(&entity.ServerSetting{Username: "root", Password: "secret"})
	if err != nil || len(passwordCfg.Auth) != 1 {
		t.Fatalf("default password authentication failed: cfg=%v err=%v", passwordCfg, err)
	}
	if _, err := remoteSSHClientConfig(&entity.ServerSetting{Username: "root", AuthMode: "privateKey"}); err == nil {
		t.Fatal("private-key mode accepted an absent key")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	keyCfg, err := remoteSSHClientConfig(&entity.ServerSetting{Username: "root", AuthMode: "privateKey", PrivateKey: privateKey})
	if err != nil || len(keyCfg.Auth) != 1 {
		t.Fatalf("PEM private-key authentication failed: cfg=%v err=%v", keyCfg, err)
	}
	if _, err := remoteSSHClientConfig(&entity.ServerSetting{Username: "root", AuthMode: "privateKey", PrivateKey: "invalid"}); err == nil {
		t.Fatal("invalid private key was accepted")
	}
}
