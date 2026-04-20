// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cron

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"code.gitea.io/gitea/models/db"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/storage"
)

type BackupConfig struct {
	BaseConfig
}

func registerBackupTask() {
	RegisterTaskFatal("backup_repos", &BackupConfig{
		BaseConfig: BaseConfig{
			Enabled:         false,
			RunAtStart:      false,
			Schedule:        "@midnight",
			NoticeOnSuccess: false,
		},
	}, func(ctx context.Context, _ *user_model.User, config Config) error {
		return runBackupTask(ctx)
	})
}

func runBackupTask(ctx context.Context) error {
	if storage.IsDiscardStorage(storage.Backup) {
		log.Error("Backup storage is not configured")
		return fmt.Errorf("backup storage is not configured")
	}

	// 生成日期文件夹名 YYYY-MM-DD
	dateDir := time.Now().UTC().Format("2006-01-02")
	remoteBase := dateDir

	log.Info("Starting daily backup to: %s/", remoteBase)

	// 创建远程日期目录
	if err := storage.Backup.MkdirAll(remoteBase); err != nil {
		return fmt.Errorf("failed to create remote directory %s: %w", remoteBase, err)
	}

	// 上传仓库目录（单个文件失败不中断整体备份）
	log.Info("Uploading repositories from: %s", setting.RepoRootPath)
	uploadDirectoryWithTolerance(remoteBase+"/repos", setting.RepoRootPath)

	// 上传 LFS 数据
	if !setting.Backup.SkipLFS && setting.LFS.StartServer {
		log.Info("Uploading LFS data")
		uploadStorageObjectsWithTolerance(remoteBase+"/data/lfs", storage.LFS)
	}

	// 上传附件
	if !setting.Backup.SkipAttachments {
		log.Info("Uploading attachments")
		uploadStorageObjectsWithTolerance(remoteBase+"/data/attachments", storage.Attachments)
	}

	// 上传包
	if !setting.Backup.SkipPackages && setting.Packages.Enabled {
		log.Info("Uploading packages")
		uploadStorageObjectsWithTolerance(remoteBase+"/data/packages", storage.Packages)
	}

	// 上传数据库 dump（关键数据，失败则整体报错）
	log.Info("Uploading database dump")
	tmpDir := filepath.Join(setting.AppDataPath, "tmp")
	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create tmp dir: %w", err)
	}
	dbFile := filepath.Join(tmpDir, "gitea-db.sql")
	if err := db.DumpDatabase(dbFile, ""); err != nil {
		return fmt.Errorf("failed to dump database: %w", err)
	}
	if err := uploadFile(remoteBase+"/gitea-db.sql", dbFile); err != nil {
		os.Remove(dbFile)
		return fmt.Errorf("failed to upload database dump: %w", err)
	}
	os.Remove(dbFile)

	// 上传配置文件（关键数据，失败则整体报错）
	log.Info("Uploading configuration file: %s", setting.CustomConf)
	if err := uploadFile(remoteBase+"/app.ini", setting.CustomConf); err != nil {
		return fmt.Errorf("failed to upload app.ini: %w", err)
	}

	// 上传 custom/ 目录（非关键，失败仅警告）
	customDir, err := os.Stat(setting.CustomPath)
	if err == nil && customDir.IsDir() {
		log.Info("Uploading custom directory from: %s", setting.CustomPath)
		uploadDirectoryWithTolerance(remoteBase+"/custom", setting.CustomPath)
	}

	// 上传 data/ 目录（排除已单独上传的子目录，非关键）
	if _, err := os.Stat(setting.AppDataPath); err == nil {
		log.Info("Uploading data directory from: %s", setting.AppDataPath)
		excludes := map[string]bool{
			setting.RepoRootPath:             true,
			setting.LFS.Storage.Path:         true,
			setting.Attachment.Storage.Path:  true,
			setting.Packages.Storage.Path:    true,
			setting.RepoArchive.Storage.Path: true,
			setting.Log.RootPath:             true,
		}
		uploadDirectoryExcludingWithTolerance(remoteBase+"/data", setting.AppDataPath, excludes)
	}

	log.Info("Backup uploaded successfully to: %s/", remoteBase)

	// 备份轮转：删除超过保留天数的日期文件夹
	retentionDays := setting.Backup.RetentionDays
	if retentionDays > 0 {
		if err := rotateDailyBackups(retentionDays, dateDir); err != nil {
			log.Warn("Backup rotation failed: %v", err)
		}
	}

	log.Info("Backup completed successfully: %s/", remoteBase)
	return nil
}

// uploadFile 上传单个本地文件到远程存储
func uploadFile(remotePath, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", localPath, err)
	}

	if _, err := storage.Backup.Save(remotePath, f, stat.Size()); err != nil {
		return fmt.Errorf("upload %s: %w", remotePath, err)
	}
	return nil
}

// uploadDirectoryWithTolerance 递归上传目录，单个文件失败时记录警告并继续
func uploadDirectoryWithTolerance(remoteBase, localBase string) {
	filepath.WalkDir(localBase, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Warn("Backup: skip inaccessible path %s: %v", path, err)
			return nil
		}

		relPath, relErr := filepath.Rel(localBase, path)
		if relErr != nil {
			return nil
		}
		if relPath == "." {
			return nil
		}

		remotePath := remoteBase + "/" + filepath.ToSlash(relPath)

		if d.IsDir() {
			if mkErr := storage.Backup.MkdirAll(remotePath); mkErr != nil {
				log.Warn("Backup: failed to create remote dir %s: %v", remotePath, mkErr)
			}
			return nil
		}

		if upErr := uploadFile(remotePath, path); upErr != nil {
			log.Warn("Backup: failed to upload %s: %v", remotePath, upErr)
		}
		return nil
	})
}

// uploadDirectoryExcludingWithTolerance 递归上传目录，跳过 excludes 路径，单个文件失败时记录警告并继续
func uploadDirectoryExcludingWithTolerance(remoteBase, localBase string, excludes map[string]bool) {
	filepath.WalkDir(localBase, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Warn("Backup: skip inaccessible path %s: %v", path, err)
			return nil
		}

		if excludes[path] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		relPath, relErr := filepath.Rel(localBase, path)
		if relErr != nil {
			return nil
		}
		if relPath == "." {
			return nil
		}

		remotePath := remoteBase + "/" + filepath.ToSlash(relPath)

		if d.IsDir() {
			if mkErr := storage.Backup.MkdirAll(remotePath); mkErr != nil {
				log.Warn("Backup: failed to create remote dir %s: %v", remotePath, mkErr)
			}
			return nil
		}

		if upErr := uploadFile(remotePath, path); upErr != nil {
			log.Warn("Backup: failed to upload %s: %v", remotePath, upErr)
		}
		return nil
	})
}

// uploadStorageObjectsWithTolerance 从 ObjectStorage 逐对象上传，单个失败时记录警告并继续
func uploadStorageObjectsWithTolerance(remoteBase string, srcStore storage.ObjectStorage) {
	srcStore.IterateObjects("", func(objPath string, obj storage.Object) error {
		remotePath := remoteBase + "/" + objPath
		info, statErr := obj.Stat()
		if statErr != nil {
			log.Warn("Backup: failed to stat object %s: %v", objPath, statErr)
			return nil
		}
		if _, saveErr := storage.Backup.Save(remotePath, obj, info.Size()); saveErr != nil {
			log.Warn("Backup: failed to upload object %s: %v", remotePath, saveErr)
		}
		return nil
	})
}

// rotateDailyBackups 删除超过保留天数的日期文件夹
func rotateDailyBackups(retentionDays int, currentDateDir string) error {
	type datedDir struct {
		name string
		date time.Time
	}

	var dirs []datedDir

	// 遍历备份存储根目录，收集所有日期文件夹
	err := storage.Backup.IterateObjects("", func(path string, obj storage.Object) error {
		// 仅处理顶层路径（日期文件夹中的文件）
		parts := strings.SplitN(path, "/", 2)
		dirName := parts[0]

		// 检查是否符合 YYYY-MM-DD 格式
		t, err := time.Parse("2006-01-02", dirName)
		if err != nil {
			return nil
		}

		// 去重：同一个日期文件夹可能有多个文件
		for _, d := range dirs {
			if d.name == dirName {
				return nil
			}
		}

		dirs = append(dirs, datedDir{name: dirName, date: t})
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to iterate backup directories: %w", err)
	}

	if len(dirs) == 0 {
		return nil
	}

	// 按日期升序排列
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].date.Before(dirs[j].date)
	})

	// 计算截止日期：保留最近 retentionDays 天
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)

	deleted := 0
	for _, d := range dirs {
		// 跳过当前正在备份的日期文件夹
		if d.name == currentDateDir {
			continue
		}
		// 只删除早于截止日期的文件夹
		if !d.date.Before(cutoff) {
			continue
		}

		// 删除该日期文件夹下的所有文件
		if err := deleteRemoteDirectory(d.name); err != nil {
			log.Warn("Failed to delete old backup directory %s: %v", d.name, err)
			continue
		}
		deleted++
		log.Info("Deleted old backup directory: %s", d.name)
	}

	if deleted > 0 {
		log.Info("Backup rotation: deleted %d old backup directory(ies), retention: %d days", deleted, retentionDays)
	}

	return nil
}

// deleteRemoteDirectory 递归删除远程存储中指定前缀下的所有文件
func deleteRemoteDirectory(prefix string) error {
	var paths []string
	err := storage.Backup.IterateObjects(prefix, func(path string, obj storage.Object) error {
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}

	for _, p := range paths {
		if err := storage.Backup.Delete(p); err != nil {
			log.Warn("Failed to delete %s: %v", p, err)
		}
	}
	return nil
}
