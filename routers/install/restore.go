// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package install

import (
	stdctx "context"
	"fmt"
	"net/http"
	"time"

	"code.gitea.io/gitea/modules/graceful"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/storage"
	"code.gitea.io/gitea/modules/templates"
	backup_svc "code.gitea.io/gitea/services/backup"
	"code.gitea.io/gitea/services/context"
)

const tplRestore templates.TplName = "restore"

// RestorePage 显示恢复进度页面
func RestorePage(ctx *context.Context) {
	ctx.Data["title"] = ctx.Tr("install.restore_title")
	ctx.HTML(http.StatusOK, tplRestore)
}

// RestoreStatus 返回恢复进度的 JSON
func RestoreStatus(ctx *context.Context) {
	progress := backup_svc.GetRestoreProgress()
	ctx.JSON(http.StatusOK, progress)
}

// StartRestore 开始从备份恢复
func StartRestore(ctx *context.Context) {
	progress := backup_svc.GetRestoreProgress()
	if progress.Status == backup_svc.RestoreStatusRunning {
		ctx.JSON(http.StatusConflict, map[string]string{"error": "restore already in progress"})
		return
	}

	// 初始化备份存储
	backupStorage, err := initBackupStorage()
	if err != nil {
		log.Error("Failed to init backup storage: %v", err)
		ctx.JSON(http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("failed to init backup storage: %v", err)})
		return
	}

	// 获取最新备份
	backupInfo, err := backup_svc.GetLatestBackupInfo(ctx.Req.Context(), backupStorage)
	if err != nil || backupInfo == nil {
		ctx.JSON(http.StatusNotFound, map[string]string{"error": "no backup found"})
		return
	}

	// 在后台 goroutine 中执行恢复
	go func() {
		bgCtx := stdctx.Background()
		if err := backup_svc.RestoreFromBackup(bgCtx, backupStorage, backupInfo.FileName); err != nil {
			log.Error("Restore from backup failed: %v", err)
		}
	}()

	ctx.JSON(http.StatusOK, map[string]string{"status": "started"})
}

// SkipRestore 跳过恢复，正常完成安装
func SkipRestore(ctx *context.Context) {
	// 设置 INSTALL_LOCK=true
	cfg, err := setting.NewConfigProviderFromFile(setting.CustomConf)
	if err == nil {
		cfg.Section("security").Key("INSTALL_LOCK").SetValue("true")
		cfg.SaveTo(setting.CustomConf)
	}

	setting.ClearEnvConfigKeys()
	log.Info("Installation finished (restore skipped)")
	InstallDone(ctx)

	go func() {
		time.Sleep(3 * time.Second)
		graceful.GetManager().DoGracefulShutdown()
	}()
}

// RestoreFinish 恢复完成后的收尾工作
func RestoreFinish(ctx *context.Context) {
	// 保存 INSTALL_LOCK
	cfg, err := setting.NewConfigProviderFromFile(setting.CustomConf)
	if err == nil {
		cfg.Section("security").Key("INSTALL_LOCK").SetValue("true")
		cfg.SaveTo(setting.CustomConf)
	}

	setting.InstallLock = true
	setting.ClearEnvConfigKeys()
	log.Info("Restore from backup completed!")
	InstallDone(ctx)

	go func() {
		time.Sleep(3 * time.Second)
		graceful.GetManager().DoGracefulShutdown()
	}()
}

// initBackupStorage 根据当前配置初始化备份存储连接
func initBackupStorage() (storage.ObjectStorage, error) {
	if setting.Backup.WebDAVStorage == nil {
		return nil, fmt.Errorf("backup storage not configured")
	}
	return storage.NewStorage(setting.Backup.WebDAVStorage.Type, setting.Backup.WebDAVStorage)
}

// CheckBackupOnInstall 在安装提交后检查是否有可恢复的备份
func CheckBackupOnInstall() (*backup_svc.BackupInfo, error) {
	if setting.Backup.WebDAVStorage == nil {
		return nil, nil
	}

	backupStorage, err := initBackupStorage()
	if err != nil {
		return nil, fmt.Errorf("failed to init backup storage: %w", err)
	}

	ctx := stdctx.Background()
	return backup_svc.GetLatestBackupInfo(ctx, backupStorage)
}
