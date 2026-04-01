// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cron

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"code.gitea.io/gitea/models/db"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/dump"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/storage"
	"code.gitea.io/gitea/modules/timeutil"
)

type BackupConfig struct {
	BaseConfig
	// Retention 配置：保留的备份文件数量（0 表示不限制）
	Retention int
}

func registerBackupTask() {
	RegisterTaskFatal("backup_repos", &BackupConfig{
		BaseConfig: BaseConfig{
			Enabled:         false,
			RunAtStart:      false,
			Schedule:        "@midnight",
			NoticeOnSuccess: false,
		},
		Retention: 0, // 默认保留所有备份
	}, func(ctx context.Context, _ *user_model.User, config Config) error {
		backupConfig := config.(*BackupConfig)
		return runBackupTask(ctx, backupConfig)
	})
}

func runBackupTask(ctx context.Context, cfg *BackupConfig) error {
	// 检查备份存储是否已初始化（未配置或初始化失败）
	if storage.IsDiscardStorage(storage.Backup) {
		log.Error("Backup storage is not configured")
		return fmt.Errorf("backup storage is not configured")
	}

	// Create temporary directory for backup
	tmpDir := filepath.Join(setting.AppDataPath, "tmp")
	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create tmp dir: %w", err)
	}

	// Determine backup format
	format := setting.Backup.Format
	if format == "" {
		format = "zip"
	}

	// Validate format
	validFormat := false
	for _, f := range dump.SupportedOutputTypes {
		if f == format {
			validFormat = true
			break
		}
	}
	if !validFormat {
		return fmt.Errorf("unsupported backup format: %s", format)
	}

	// 生成备份文件名，格式：gitea-backup-{timestamp}.{format}
	fileName := fmt.Sprintf("gitea-backup-%d.%s", timeutil.TimeStampNow(), format)
	tmpFile := filepath.Join(tmpDir, fileName)

	// 确保临时文件在函数结束时被清理
	var tmpFileCreated bool
	defer func() {
		if tmpFileCreated {
			os.Remove(tmpFile)
		}
	}()

	// Create backup file
	file, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to create backup file: %w", err)
	}
	tmpFileCreated = true

	// Create dumper
	dumper, err := dump.NewDumper(ctx, format, file)
	if err != nil {
		file.Close()
		return fmt.Errorf("failed to create dumper: %w", err)
	}
	dumper.Verbose = true

	// 使用 defer 确保 dumper 和 file 都被正确关闭
	var dumperClosed, fileClosed bool
	defer func() {
		if !dumperClosed {
			dumper.Close()
		}
		if !fileClosed {
			file.Close()
		}
	}()

	// Add repositories directory
	log.Info("Adding repositories to backup: %s", setting.RepoRootPath)
	if err := dumper.AddRecursiveExclude("repos", setting.RepoRootPath, nil); err != nil {
		return fmt.Errorf("failed to add repos: %w", err)
	}

	// Add LFS data
	if !setting.Backup.SkipLFS && setting.LFS.StartServer {
		log.Info("Adding LFS data to backup")
		if err := storage.LFS.IterateObjects("", func(objPath string, obj storage.Object) error {
			info, err := obj.Stat()
			if err != nil {
				return err
			}
			return dumper.AddFileByReader(obj, info, filepath.Join("data", "lfs", objPath))
		}); err != nil {
			return fmt.Errorf("failed to add LFS: %w", err)
		}
	}

	// Add Attachments
	if !setting.Backup.SkipAttachments {
		log.Info("Adding attachments to backup")
		if err := storage.Attachments.IterateObjects("", func(objPath string, obj storage.Object) error {
			info, err := obj.Stat()
			if err != nil {
				return err
			}
			return dumper.AddFileByReader(obj, info, filepath.Join("data", "attachments", objPath))
		}); err != nil {
			return fmt.Errorf("failed to add attachments: %w", err)
		}
	}

	// Add Packages
	if !setting.Backup.SkipPackages && setting.Packages.Enabled {
		log.Info("Adding packages to backup")
		if err := storage.Packages.IterateObjects("", func(objPath string, obj storage.Object) error {
			info, err := obj.Stat()
			if err != nil {
				return err
			}
			return dumper.AddFileByReader(obj, info, filepath.Join("data", "packages", objPath))
		}); err != nil {
			return fmt.Errorf("failed to add packages: %w", err)
		}
	}

	// Add database dump
	if !setting.Backup.SkipDB {
		log.Info("Adding database to backup")
		dbFile := filepath.Join(tmpDir, "gitea-db.sql")
		if err := db.DumpDatabase(dbFile, ""); err != nil {
			return fmt.Errorf("failed to dump database: %w", err)
		}

		if err := dumper.AddFileByPath("gitea-db.sql", dbFile); err != nil {
			os.Remove(dbFile)
			return fmt.Errorf("failed to add db dump: %w", err)
		}
		os.Remove(dbFile) // db dump 已归档，删除临时文件
	}

	// 添加配置文件 app.ini
	log.Info("Adding configuration file from %s", setting.CustomConf)
	if err := dumper.AddFileByPath("app.ini", setting.CustomConf); err != nil {
		log.Warn("Failed to include app.ini: %v", err)
	}

	// 添加自定义目录 custom/
	customDir, err := os.Stat(setting.CustomPath)
	if err == nil && customDir.IsDir() {
		if is, _ := dump.IsSubdir(setting.AppDataPath, setting.CustomPath); !is {
			log.Info("Adding custom directory from %s", setting.CustomPath)
			if err := dumper.AddRecursiveExclude("custom", setting.CustomPath, nil); err != nil {
				log.Warn("Failed to include custom: %v", err)
			}
		} else {
			log.Info("Custom dir %s is inside data dir %s, skipped", setting.CustomPath, setting.AppDataPath)
		}
	} else {
		log.Info("Custom dir %s doesn't exist, skipped", setting.CustomPath)
	}

	// 添加数据目录 data/（排除已单独备份的子目录）
	if _, err := os.Stat(setting.AppDataPath); err == nil {
		log.Info("Adding data directory from %s", setting.AppDataPath)
		var excludes []string
		excludes = append(excludes, setting.RepoRootPath)
		excludes = append(excludes, setting.LFS.Storage.Path)
		excludes = append(excludes, setting.Attachment.Storage.Path)
		excludes = append(excludes, setting.Packages.Storage.Path)
		excludes = append(excludes, setting.RepoArchive.Storage.Path)
		excludes = append(excludes, setting.Log.RootPath)
		if err := dumper.AddRecursiveExclude("data", setting.AppDataPath, excludes); err != nil {
			log.Warn("Failed to include data directory: %v", err)
		}
	}

	// Close dumper and finalize archive
	if err := dumper.Close(); err != nil {
		return fmt.Errorf("failed to close dumper: %w", err)
	}
	dumperClosed = true

	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close backup file: %w", err)
	}
	fileClosed = true

	// Open backup file for upload
	backupFile, err := os.Open(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to open backup file: %w", err)
	}

	// Upload to WebDAV storage using the globally initialized storage.Backup
	log.Info("Uploading backup to storage: %s", fileName)
	stat, err := backupFile.Stat()
	if err != nil {
		backupFile.Close()
		return fmt.Errorf("failed to stat backup file: %w", err)
	}

	if _, err := storage.Backup.Save(fileName, backupFile, stat.Size()); err != nil {
		backupFile.Close()
		return fmt.Errorf("failed to upload backup: %w", err)
	}
	backupFile.Close()

	log.Info("Backup uploaded successfully: %s", fileName)

	// 备份轮转：按保留数量清理旧备份
	if cfg.Retention > 0 {
		if err := rotateBackups(cfg.Retention, fileName); err != nil {
			// 轮转失败不影响本次备份成功状态，只记录警告
			log.Warn("Backup rotation failed: %v", err)
		}
	}

	log.Info("Backup completed successfully: %s", fileName)
	return nil
}

// rotateBackups 列出备份存储中的所有 gitea-backup-* 文件，
// 按修改时间倒序，删除超出 retention 数量的旧备份。
func rotateBackups(retention int, currentFileName string) error {
	// List all backup files in storage
	var files []string

	iterErr := storage.Backup.IterateObjects("", func(path string, obj storage.Object) error {
		if strings.HasPrefix(path, "gitea-backup-") {
			files = append(files, path)
		}
		return nil
	})

	if iterErr != nil {
		return fmt.Errorf("failed to iterate backup files: %w", iterErr)
	}

	if len(files) <= retention {
		return nil
	}

	// 收集文件及其修改时间，按时间倒序
	type fileWithTime struct {
		path    string
		modTime time.Time
	}
	var fileInfos []fileWithTime
	for _, f := range files {
		info, err := storage.Backup.Stat(f)
		if err != nil {
			continue
		}
		fileInfos = append(fileInfos, fileWithTime{path: f, modTime: info.ModTime()})
	}

	sort.Slice(fileInfos, func(i, j int) bool {
		return fileInfos[i].modTime.After(fileInfos[j].modTime)
	})

	// 删除超出保留数量的旧备份（跳过当前刚上传的文件）
	deleted := 0
	for i := retention; i < len(fileInfos); i++ {
		if fileInfos[i].path == currentFileName {
			continue
		}
		if err := storage.Backup.Delete(fileInfos[i].path); err != nil {
			log.Warn("Failed to delete old backup %s: %v", fileInfos[i].path, err)
			continue
		}
		deleted++
		log.Info("Deleted old backup: %s", fileInfos[i].path)
	}

	if deleted > 0 {
		log.Info("Backup rotation: deleted %d old backup(s), kept %d", deleted, retention)
	}

	return nil
}
