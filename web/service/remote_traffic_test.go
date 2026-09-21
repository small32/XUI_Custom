package service

import (
	"os/exec"
	"testing"
)

func TestRemoteTrafficSQLiteOutput(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 required")
	}
	sql := "CREATE TABLE inbounds(port INTEGER, up INTEGER, down INTEGER, total INTEGER, enable INTEGER); INSERT INTO inbounds VALUES(39420,4294967296,20,5000000000,1),(39421,NULL,NULL,NULL,0);" + remoteTrafficSQL
	out, err := exec.Command("sqlite3", "-batch", "-noheader", "-separator", "|", ":memory:", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite: %v: %s", err, out)
	}
	rows, err := parseRemoteTraffic(out)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].Port != 39420 || rows[0].Used != 4294967316 || !rows[0].Enable || rows[1].Used != 0 || rows[1].Enable {
		t.Fatalf("unexpected traffic: %+v / %+v", rows[0], rows[1])
	}
}

func TestRemoteTrafficRejectsMalformedRows(t *testing.T) {
	for _, input := range []string{`39420\t10\t20\t100\t1`, "39420|10|20|100", "39420|bad|20|100|1", "39420|10|20|100|2", "39420|10|20|100|1\ninvalid"} {
		if rows, err := parseRemoteTraffic([]byte(input)); err == nil || rows != nil {
			t.Fatalf("must reject entire response: %q", input)
		}
	}
	rows, err := parseRemoteTraffic(nil)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty database: %v %v", rows, err)
	}
	if rows, err := parseRemoteTraffic([]byte("39420|10|20|100|1\r\n")); err != nil || len(rows) != 1 {
		t.Fatalf("CRLF: %v %v", rows, err)
	}
}
