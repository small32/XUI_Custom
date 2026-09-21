package database

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"io/fs"
	"os"
	"path"
	"x-ui/config"
	"x-ui/database/model"
)

var db *gorm.DB

func initUser() error {
	err := db.AutoMigrate(&model.User{})
	if err != nil {
		return err
	}
	var count int64
	err = db.Model(&model.User{}).Count(&count).Error
	if err != nil {
		return err
	}
	if count == 0 {
		user := &model.User{
			Username: "admin",
			Password: "admin",
		}
		return db.Create(user).Error
	}
	return nil
}

// initInbound 建入站表，并处理“按月计算”（monthly_reset）这个新增列的老库迁移。
//
// monthly_reset 是后加的列，AutoMigrate 会给老库补上，但补出来的默认值是 0（不按月），
// 而升级前所有入站都在按月清零。若不回填，老客户会在升级后静默丢掉月度周期，
// 流量一路累计到上限就永久停用。因此这里在补列的同时把老数据回填为 1，保持原有行为；
// 之后要不要改成累计计费，由管理员在入站设置里逐条取消勾选。
//
// 全新安装（表还不存在）没有老数据，回填不会影响任何行。
func initInbound() error {
	backfill := db.Migrator().HasTable(&model.Inbound{}) &&
		!db.Migrator().HasColumn(&model.Inbound{}, "monthly_reset")
	if err := db.AutoMigrate(&model.Inbound{}); err != nil {
		return err
	}
	if !backfill {
		return nil
	}
	// GORM 默认拒绝无条件更新，这里用一个恒真条件显式放开。
	return db.Model(&model.Inbound{}).Where("1 = 1").Update("monthly_reset", true).Error
}

func initSetting() error {
	return db.AutoMigrate(&model.Setting{})
}

func initTrafficSnapshot() error {
	return db.AutoMigrate(&model.TrafficSnapshot{})
}

func InitDB(dbPath string) error {
	dir := path.Dir(dbPath)
	err := os.MkdirAll(dir, fs.ModeDir)
	if err != nil {
		return err
	}

	var gormLogger logger.Interface

	if config.IsDebug() {
		gormLogger = logger.Default
	} else {
		gormLogger = logger.Discard
	}

	c := &gorm.Config{
		Logger: gormLogger,
	}
	db, err = gorm.Open(sqlite.Open(dbPath), c)
	if err != nil {
		return err
	}

	err = initUser()
	if err != nil {
		return err
	}
	err = initInbound()
	if err != nil {
		return err
	}
	err = initSetting()
	if err != nil {
		return err
	}
	err = initTrafficSnapshot()
	if err != nil {
		return err
	}

	return nil
}

func GetDB() *gorm.DB {
	return db
}

func IsNotFound(err error) bool {
	return err == gorm.ErrRecordNotFound
}
