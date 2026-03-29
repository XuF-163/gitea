// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cron

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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
	SkipLFS         bool
	SkipAttachments bool
	SkipPackages    bool
	SkipDB          bool
}

func registerBackupTask() {
	RegisterTaskFatal("backup_repos", &BackupConfig{
		BaseConfig: BaseConfig{
			Enabled:         false,
			RunAtStart:      false,
			Schedule:        "@midnight",
			NoticeOnSuccess:  false,
		},
	}, func(ctx context.Context, _ *user_model.User, config Config) error {
		backupConfig := config.(*BackupConfig)
		return runBackupTask(ctx, backupConfig)
	})
}

func runBackupTask(ctx context.Context, cfg *BackupConfig) error {
	// Check if backup storage is configured
	if setting.Backup.WebDevStorage == nil {
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

	// Generate backup filename
	fileName := fmt.Sprintf("gitea-backup-%d.%s", timeutil.TimeStampNow(), format)
	tmpFile := filepath.Join(tmpDir, fileName)

	// Create backup file
	file, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to create backup file: %w", err)
	}

	// Create dumper
	dumper, err := dump.NewDumper(ctx, format, file)
	if err != nil {
		file.Close()
		return fmt.Errorf("failed to create dumper: %w", err)
	}
	dumper.Verbose = true

	// Add repositories directory
	log.Info("Adding repositories to backup: %s", setting.RepoRootPath)
	if err := dumper.AddRecursiveExclude("repos", setting.RepoRootPath, nil); err != nil {
		dumper.Close()
		file.Close()
		return fmt.Errorf("failed to add repos: %w", err)
	}

	// Add LFS data
	if !cfg.SkipLFS && setting.LFS.StartServer {
		log.Info("Adding LFS data to backup")
		if err := storage.LFS.IterateObjects("", func(objPath string, obj storage.Object) error {
			info, err := obj.Stat()
			if err != nil {
				return err
			}
			return dumper.AddFileByReader(obj, info, filepath.Join("data", "lfs", objPath))
		}); err != nil {
			dumper.Close()
			file.Close()
			return fmt.Errorf("failed to add LFS: %w", err)
		}
	}

	// Add Attachments
	if !cfg.SkipAttachments {
		log.Info("Adding attachments to backup")
		if err := storage.Attachments.IterateObjects("", func(objPath string, obj storage.Object) error {
			info, err := obj.Stat()
			if err != nil {
				return err
			}
			return dumper.AddFileByReader(obj, info, filepath.Join("data", "attachments", objPath))
		}); err != nil {
			dumper.Close()
			file.Close()
			return fmt.Errorf("failed to add attachments: %w", err)
		}
	}

	// Add Packages
	if !cfg.SkipPackages && setting.Packages.Enabled {
		log.Info("Adding packages to backup")
		if err := storage.Packages.IterateObjects("", func(objPath string, obj storage.Object) error {
			info, err := obj.Stat()
			if err != nil {
				return err
			}
			return dumper.AddFileByReader(obj, info, filepath.Join("data", "packages", objPath))
		}); err != nil {
			dumper.Close()
			file.Close()
			return fmt.Errorf("failed to add packages: %w", err)
		}
	}

	// Add database dump
	if !cfg.SkipDB {
		log.Info("Adding database to backup")
		dbFile := filepath.Join(tmpDir, "gitea-db.sql")
		if err := db.DumpDatabase(dbFile, ""); err != nil {
			dumper.Close()
			file.Close()
			return fmt.Errorf("failed to dump database: %w", err)
		}

		if err := dumper.AddFileByPath("gitea-db.sql", dbFile); err != nil {
			dumper.Close()
			file.Close()
			os.Remove(dbFile)
			return fmt.Errorf("failed to add db dump: %w", err)
		}
		os.Remove(dbFile)
	}

	// Close dumper and finalize archive
	if err := dumper.Close(); err != nil {
		file.Close()
		return fmt.Errorf("failed to close dumper: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close backup file: %w", err)
	}

	// Upload to WebDAV storage
	log.Info("Uploading backup to WebDAV storage: %s", fileName)
	backupStorage, err := storage.NewStorage(setting.Backup.WebDevStorage.Type, setting.Backup.WebDevStorage)
	if err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to create backup storage: %w", err)
	}

	backupFile, err := os.Open(tmpFile)
	if err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to open backup file: %w", err)
	}

	stat, err := backupFile.Stat()
	if err != nil {
		backupFile.Close()
		os.Remove(tmpFile)
		return fmt.Errorf("failed to stat backup file: %w", err)
	}

	if _, err := backupStorage.Save(fileName, backupFile, stat.Size()); err != nil {
		backupFile.Close()
		os.Remove(tmpFile)
		return fmt.Errorf("failed to upload backup: %w", err)
	}
	backupFile.Close()
	os.Remove(tmpFile)

	log.Info("Backup completed successfully: %s", fileName)
	return nil
}
