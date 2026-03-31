// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package backup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/storage"

	"github.com/mholt/archives"
)

// BackupInfo 备份文件信息
type BackupInfo struct {
	FileName string
	Size     int64
	ModTime  time.Time
}

// GetLatestBackupInfo 获取最新的备份文件信息
func GetLatestBackupInfo(ctx context.Context, backupStorage storage.ObjectStorage) (*BackupInfo, error) {
	var backups []BackupInfo

	err := backupStorage.IterateObjects("", func(path string, obj storage.Object) error {
		fileName := filepath.Base(path)
		if !strings.HasPrefix(fileName, "gitea-backup-") {
			return nil
		}
		info, err := obj.Stat()
		if err != nil {
			return nil
		}
		backups = append(backups, BackupInfo{
			FileName: path,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list backup files: %w", err)
	}

	if len(backups) == 0 {
		return nil, nil
	}

	// 按修改时间降序排列，取最新的
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].ModTime.After(backups[j].ModTime)
	})

	return &backups[0], nil
}

// RestoreFromBackup 从备份存储恢复数据
// 备份归档格式（与 cron task 一致）：repos/ + data/ + custom/ + app.ini + gitea-db.sql
func RestoreFromBackup(ctx context.Context, backupStorage storage.ObjectStorage, backupPath string) error {
	ResetRestoreProgress()
	UpdateRestoreProgress("downloading backup", 5)

	// 创建临时目录
	tmpDir := filepath.Join(setting.AppDataPath, "tmp", "restore")
	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		SetRestoreFailed(fmt.Sprintf("failed to create temp dir: %v", err))
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// 下载备份文件
	backupFile := filepath.Join(tmpDir, filepath.Base(backupPath))
	if err := downloadBackup(ctx, backupStorage, backupPath, backupFile); err != nil {
		SetRestoreFailed(fmt.Sprintf("failed to download backup: %v", err))
		return fmt.Errorf("failed to download backup: %w", err)
	}
	UpdateRestoreProgress("download complete, extracting archive", 25)

	// 将备份归档当作文件系统读取
	archiveFS, err := archives.FileSystem(ctx, backupFile, nil)
	if err != nil {
		SetRestoreFailed(fmt.Sprintf("failed to open archive: %v", err))
		return fmt.Errorf("failed to open archive: %w", err)
	}

	// 恢复配置文件 app.ini
	UpdateRestoreProgress("restoring configuration", 30)
	if err := restoreSingleFile(archiveFS, "app.ini", setting.CustomConf); err != nil {
		log.Error("Failed to restore app.ini: %v", err)
	}

	// 恢复自定义目录 custom/
	UpdateRestoreProgress("restoring custom directory", 35)
	if err := restoreFromArchiveFS(archiveFS, "custom", setting.CustomPath); err != nil {
		log.Error("Failed to restore custom: %v", err)
	}

	// 恢复仓库
	UpdateRestoreProgress("restoring repositories", 45)
	if err := restoreFromArchiveFS(archiveFS, "repos", setting.RepoRootPath); err != nil {
		log.Error("Failed to restore repos: %v", err)
	}

	// 恢复 LFS 数据
	UpdateRestoreProgress("restoring LFS data", 55)
	lfsPath := setting.LFS.Storage.Path
	if lfsPath == "" {
		lfsPath = filepath.Join(setting.AppDataPath, "lfs")
	}
	if err := restoreFromArchiveFS(archiveFS, "data/lfs", lfsPath); err != nil {
		log.Error("Failed to restore LFS: %v", err)
	}

	// 恢复附件
	UpdateRestoreProgress("restoring attachments", 65)
	attachPath := setting.Attachment.Storage.Path
	if attachPath == "" {
		attachPath = filepath.Join(setting.AppDataPath, "attachments")
	}
	if err := restoreFromArchiveFS(archiveFS, "data/attachments", attachPath); err != nil {
		log.Error("Failed to restore attachments: %v", err)
	}

	// 恢复包
	UpdateRestoreProgress("restoring packages", 72)
	pkgPath := setting.Packages.Storage.Path
	if pkgPath == "" {
		pkgPath = filepath.Join(setting.AppDataPath, "packages")
	}
	if err := restoreFromArchiveFS(archiveFS, "data/packages", pkgPath); err != nil {
		log.Error("Failed to restore packages: %v", err)
	}

	// 恢复其他 data/ 子目录（indexers, queues 等，排除已恢复的目录）
	UpdateRestoreProgress("restoring data directory", 80)
	if err := restoreDataDirExcludingKnown(archiveFS); err != nil {
		log.Error("Failed to restore data directory: %v", err)
	}

	// 恢复数据库 SQL dump
	UpdateRestoreProgress("restoring database", 90)
	if err := restoreDatabaseFromArchive(archiveFS); err != nil {
		log.Error("Failed to restore database: %v", err)
	}

	UpdateRestoreProgress("restore completed", 100)
	SetRestoreDone()
	return nil
}

// downloadBackup 从备份存储下载文件到本地
func downloadBackup(ctx context.Context, backupStorage storage.ObjectStorage, remotePath, localPath string) error {
	obj, err := backupStorage.Open(remotePath)
	if err != nil {
		return fmt.Errorf("failed to open backup object: %w", err)
	}
	defer obj.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, obj); err != nil {
		return fmt.Errorf("failed to download backup: %w", err)
	}
	return nil
}

// restoreFromArchiveFS 从归档文件系统中恢复指定子目录到目标路径
func restoreFromArchiveFS(archiveFS fs.FS, subDir, destPath string) error {
	// 检查归档中是否存在该子目录
	if _, err := fs.ReadDir(archiveFS, subDir); err != nil {
		log.Info("Directory %s not found in backup, skipping", subDir)
		return nil
	}

	if err := os.MkdirAll(destPath, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create %s: %w", destPath, err)
	}

	// 遍历并提取文件
	return fs.WalkDir(archiveFS, subDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// 计算目标路径（去掉 subDir 前缀）
		relPath := strings.TrimPrefix(path, subDir+"/")
		if relPath == subDir {
			return nil // 跳过根目录自身
		}
		destFile := filepath.Join(destPath, filepath.FromSlash(relPath))

		if d.IsDir() {
			return os.MkdirAll(destFile, os.ModePerm)
		}

		// 打开归档中的文件
		src, err := archiveFS.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer src.Close()

		// 创建目标文件
		if err := os.MkdirAll(filepath.Dir(destFile), os.ModePerm); err != nil {
			return err
		}
		dst, err := os.Create(destFile)
		if err != nil {
			return err
		}
		defer dst.Close()

		_, err = io.Copy(dst, src)
		return err
	})
}

// restoreSingleFile 从归档中恢复单个文件到目标路径
func restoreSingleFile(archiveFS fs.FS, srcName, destPath string) error {
	src, err := archiveFS.Open(srcName)
	if err != nil {
		log.Info("File %s not found in backup, skipping", srcName)
		return nil
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), os.ModePerm); err != nil {
		return fmt.Errorf("failed to create parent dir for %s: %w", destPath, err)
	}

	dst, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", destPath, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("failed to copy %s: %w", srcName, err)
	}

	log.Info("Restored %s to %s", srcName, destPath)
	return nil
}

// restoreDataDirExcludingKnown 从归档的 data/ 目录恢复文件，
// 但跳过已单独恢复的子目录（lfs, attachments, packages）
func restoreDataDirExcludingKnown(archiveFS fs.FS) error {
	// 检查归档中是否存在 data 目录
	if _, err := fs.ReadDir(archiveFS, "data"); err != nil {
		log.Info("Directory data not found in backup, skipping")
		return nil
	}

	// 已单独恢复的子目录，跳过
	alreadyRestored := map[string]bool{
		"lfs":         true,
		"attachments": true,
		"packages":    true,
	}

	return fs.WalkDir(archiveFS, "data", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// 计算相对于 data/ 的路径
		relPath := strings.TrimPrefix(path, "data/")
		if relPath == "data" {
			return nil // 跳过根目录自身
		}

		// 跳过已单独恢复的顶层子目录
		parts := strings.SplitN(relPath, "/", 2)
		if alreadyRestored[parts[0]] {
			if len(parts) == 1 && d.IsDir() {
				return fs.SkipDir // 跳过整个子目录
			}
			return nil
		}

		destFile := filepath.Join(setting.AppDataPath, filepath.FromSlash(relPath))

		if d.IsDir() {
			return os.MkdirAll(destFile, os.ModePerm)
		}

		// 打开归档中的文件
		src, err := archiveFS.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer src.Close()

		if err := os.MkdirAll(filepath.Dir(destFile), os.ModePerm); err != nil {
			return err
		}
		dst, err := os.Create(destFile)
		if err != nil {
			return err
		}
		defer dst.Close()

		_, err = io.Copy(dst, src)
		return err
	})
}

// restoreDatabaseFromArchive 从归档中恢复数据库 SQL dump
func restoreDatabaseFromArchive(archiveFS fs.FS) error {
	// 尝试打开 gitea-db.sql
	src, err := archiveFS.Open("gitea-db.sql")
	if err != nil {
		log.Info("No database dump found in backup")
		return nil
	}
	defer src.Close()

	// 将 SQL dump 保存到数据目录
	destPath := filepath.Join(setting.AppDataPath, "gitea-db-restore.sql")
	dst, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create restore sql file: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("failed to copy database dump: %w", err)
	}

	log.Info("Database SQL dump saved to %s", destPath)
	return nil
}
