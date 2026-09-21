package service

import (
	"path/filepath"
	"testing"
	"x-ui/database"
	"x-ui/database/model"
)

func TestDisableLocalOnlyReportsActualChanges(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "audit.db")); err != nil {
		t.Fatal(err)
	}
	in := &model.Inbound{Port: 32123, Tag: "audit-test", Enable: true}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatal(err)
	}
	s := &ServerManagementService{}
	for _, want := range []bool{true, false, false} {
		changed, err := s.disableLocal(in.Port)
		if err != nil || changed != want {
			t.Fatalf("changed=%v want=%v err=%v", changed, want, err)
		}
	}
	if err := database.GetDB().Model(in).Update("enable", true).Error; err != nil {
		t.Fatal(err)
	}
	if changed, err := s.disableLocal(in.Port); err != nil || !changed {
		t.Fatalf("re-enabled inbound must be disabled again: %v %v", changed, err)
	}
}
