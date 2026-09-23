package service

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"x-ui/database/model"
)

func TestSyncInboundVisibility(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 required")
	}
	db := filepath.Join(t.TempDir(), "remote.db")
	run := func(sql string) (string, error) {
		c := exec.Command("sqlite3", "-bail", db)
		c.Stdin = strings.NewReader(sql)
		b, e := c.CombinedOutput()
		return strings.TrimSpace(string(b)), e
	}
	if out, err := run(`CREATE TABLE users(id INTEGER PRIMARY KEY); INSERT INTO users VALUES(7); CREATE TABLE inbounds(user_id INTEGER,port INTEGER UNIQUE,protocol TEXT,settings TEXT,stream_settings TEXT,tag TEXT UNIQUE,sniffing TEXT,remark TEXT,listen TEXT,enable INTEGER,expiry_time INTEGER,total INTEGER,up INTEGER,down INTEGER);`); err != nil {
		t.Fatal(out, err)
	}
	in := &model.Inbound{Port: 12345, Protocol: model.Trojan, Settings: `{"password":"a'b"}`, Remark: "O'Brien", Listen: "0.0.0.0", Enable: true}
	for _, setup := range []string{"", "UPDATE inbounds SET user_id=NULL,up=10,down=20;"} {
		if _, err := run(setup + "\n" + syncInboundSQL(in, setup == "")); err != nil {
			t.Fatal(err)
		}
		out, err := run("SELECT user_id,remark,listen,tag FROM inbounds WHERE user_id=7;")
		if err != nil || out != "7|O'Brien|0.0.0.0|inbound-12345" {
			t.Fatalf("not visible: %q %v", out, err)
		}
	}
	out, _ := run("SELECT count(*),up,down FROM inbounds;")
	if out != "1|10|20" {
		t.Fatal(out)
	}
	// Existing owner remains valid even when multiple panel users exist.
	run("INSERT INTO users VALUES(8);")
	if out, err := run(syncInboundSQL(in, false)); err != nil {
		t.Fatal(out, err)
	}
	// Updating a missing port is a no-op even with multiple remote users.
	in.Port = 23456
	if out, err := run(syncInboundSQL(in, false)); err != nil || out != "0" {
		t.Fatalf("missing update: %q %v", out, err)
	}
	in.Port = 12345
	if _, err := run(syncInboundSQL(in, true)); err == nil {
		t.Fatal("creation must not overwrite an existing port")
	}
	// An ambiguous new owner must fail, never silently create an invisible account.
	in.Port = 23456
	if _, err := run(syncInboundSQL(in, true)); err == nil {
		t.Fatal("expected ambiguous owner failure")
	}
	out, _ = run("SELECT count(*) FROM inbounds;")
	if out != "1" {
		t.Fatal(out)
	}
	// Full synchronization creates a missing port and updates without resetting traffic.
	if out, err := run("DELETE FROM users WHERE id=8;"); err != nil {
		t.Fatal(out, err)
	}
	if out, err := run(syncInboundSQL(in, false, true)); err != nil || out != "1" {
		t.Fatalf("full insert: %q %v", out, err)
	}
	if out, err := run("UPDATE inbounds SET up=42 WHERE port=23456;"); err != nil {
		t.Fatal(out, err)
	}
	in.Remark = "changed"
	if out, err := run(syncInboundSQL(in, false, true)); err != nil || out != "1" {
		t.Fatalf("full update: %q %v", out, err)
	}
	out, err := run("SELECT user_id,remark,up FROM inbounds WHERE port=23456;")
	if err != nil || out != "7|changed|42" {
		t.Fatalf("full state: %q %v", out, err)
	}
	for _, want := range []string{"1", "0"} {
		if out, err := run(deleteSyncedInboundSQL(23456)); err != nil || out != want {
			t.Fatalf("delete: %q %v", out, err)
		}
	}
	out, err = run("SELECT count(*) FROM inbounds WHERE port=12345;")
	if err != nil || out != "1" {
		t.Fatal("deleted unrelated port", out, err)
	}

	if out, err := run("CREATE TABLE settings(key TEXT,value TEXT); INSERT INTO settings VALUES ('webCertFile','/remote/cert.pem'),('webKeyFile','/remote/key.pem');"); err != nil {
		t.Fatal(out, err)
	}
	in.Port = 12345
	in.StreamSettings = `{"security":"tls","tlsSettings":{"certificates":[{"certificateFile":"/local/cert.pem","keyFile":"/local/key.pem"}]}}`
	if out, err := run(syncInboundSQL(in, false)); err != nil {
		t.Fatal(out, err)
	}
	out, err = run("SELECT json_extract(stream_settings,'$.tlsSettings.certificates[0].certificateFile'),json_extract(stream_settings,'$.tlsSettings.certificates[0].keyFile') FROM inbounds WHERE port=12345;")
	if err != nil || out != "/remote/cert.pem|/remote/key.pem" {
		t.Fatalf("remote certificates: %q %v", out, err)
	}
	if _, err := run("DELETE FROM settings;"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(syncInboundSQL(in, false)); err == nil {
		t.Fatal("missing remote certificate should fail")
	}
	in.Port = 55555
	if out, err := run(syncInboundSQL(in, false)); err != nil || out != "0" {
		t.Fatalf("missing port must skip: %q %v", out, err)
	}

}

// 改端口后，远端旧端口账号不能被残留：syncInboundSQLWithOldPort 应删除旧端口，
// 并在新端口写入账号，避免孤儿账号继续可用。
func TestSyncInboundMovesPortRemovesOrphan(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 required")
	}
	db := filepath.Join(t.TempDir(), "remote.db")
	run := func(sql string) (string, error) {
		c := exec.Command("sqlite3", "-bail", db)
		c.Stdin = strings.NewReader(sql)
		b, e := c.CombinedOutput()
		return strings.TrimSpace(string(b)), e
	}
	if out, err := run(`CREATE TABLE users(id INTEGER PRIMARY KEY); INSERT INTO users VALUES(7); CREATE TABLE inbounds(user_id INTEGER,port INTEGER UNIQUE,protocol TEXT,settings TEXT,stream_settings TEXT,tag TEXT UNIQUE,sniffing TEXT,remark TEXT,listen TEXT,enable INTEGER,expiry_time INTEGER,total INTEGER,up INTEGER,down INTEGER);`); err != nil {
		t.Fatal(out, err)
	}
	in := &model.Inbound{Port: 12000, Protocol: model.Trojan, Settings: `{"password":"a"}`, Enable: true}
	if out, err := run(syncInboundSQL(in, true)); err != nil {
		t.Fatal(out, err)
	}
	if out, err := run("UPDATE inbounds SET up=900,down=200 WHERE port=12000;"); err != nil {
		t.Fatal(out, err)
	}

	// 改端口：旧 12000 → 新 12001，必须迁移而非新增残留。
	in.Port = 12001
	if out, err := run(syncInboundSQLWithOldPort(in, 12000, true)); err != nil {
		t.Fatal(out, err)
	}
	// 新端口存在。
	out, err := run("SELECT count(*) FROM inbounds WHERE port=12001;")
	if err != nil || out != "1" {
		t.Fatalf("new port not written: %q %v", out, err)
	}
	// 旧端口不复存在（孤儿被清除）。
	out, err = run("SELECT count(*) FROM inbounds WHERE port=12000;")
	if err != nil || out != "0" {
		t.Fatalf("old port orphan still present: %q %v", out, err)
	}
	if out, err = run("SELECT up,down FROM inbounds WHERE port=12001;"); err != nil || out != "900|200" {
		t.Fatalf("port migration lost remote traffic: %q %v", out, err)
	}

	in.Port = 12002
	if out, err = run(syncInboundSQLWithOldPort(in, 12000, false)); err != nil {
		t.Fatalf("normal sync of missing old port should be a no-op: %q %v", out, err)
	}
	if out, err = run("SELECT count(*) FROM inbounds WHERE port=12002;"); err != nil || out != "0" {
		t.Fatalf("normal sync created a missing remote port: %q %v", out, err)
	}
	if out, err = run(syncInboundSQLWithOldPort(in, 12000, true)); err != nil {
		t.Fatalf("full sync should create a missing new port: %q %v", out, err)
	}
	if out, err = run("SELECT count(*) FROM inbounds WHERE port=12002;"); err != nil || out != "1" {
		t.Fatalf("full sync did not create a missing remote port: %q %v", out, err)
	}
	if out, err = run("UPDATE inbounds SET up=70,down=30 WHERE port=12002;"); err != nil {
		t.Fatal(out, err)
	}
	in.Remark = "normal updates a matching new port"
	if out, err = run(syncInboundSQLWithOldPort(in, 12000, false)); err != nil {
		t.Fatalf("normal sync should update an existing new port: %q %v", out, err)
	}
	if out, err = run("SELECT remark,up,down FROM inbounds WHERE port=12002;"); err != nil || out != "normal updates a matching new port|70|30" {
		t.Fatalf("normal sync did not update target or preserve its counters: %q %v", out, err)
	}
}
