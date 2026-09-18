package service

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRemoteInboundQuery(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 required")
	}
	for _, remark := range []string{"", "节点\t39420\n第二行", "quote'中文"} {
		query := `CREATE TABLE inbounds(protocol TEXT, settings TEXT, stream_settings TEXT, sniffing TEXT, remark TEXT, port INTEGER); INSERT INTO inbounds VALUES('trojan','{"clients":[]}','{}','','` + strings.ReplaceAll(remark, "'", "''") + `',39420);` + remoteInboundSQL(39420)
		cmd := exec.Command("sqlite3", "-batch", "-noheader", ":memory:")
		cmd.Stdin = strings.NewReader(query)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		node, err := parseRemoteInbound(out, 39420, "remote.example")
		if err != nil {
			t.Fatal(err)
		}
		if node["remark"] != remark || node["port"] != 39420 || node["settings"] != `{"clients":[]}` {
			t.Fatal(node)
		}
	}
	_, err := parseRemoteInbound([]byte("bad data"), 39420, "host")
	if err == nil || !strings.Contains(err.Error(), "解析失败") {
		t.Fatal(err)
	}
	_, err = parseRemoteInbound(nil, 39420, "host")
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatal(err)
	}
}
