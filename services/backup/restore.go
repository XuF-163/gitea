// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package backup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"code.gitea.io/gitea/models/db"
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
	// IsDailyBackup 标识是否为目录结构的新格式每日备份
	IsDailyBackup bool
}

// GetLatestBackupInfo 获取最新的备份信息（同时支持旧格式 zip 和新格式日期文件夹）
func GetLatestBackupInfo(ctx context.Context, backupStorage storage.ObjectStorage) (*BackupInfo, error) {
	var backups []BackupInfo

	err := backupStorage.IterateObjects("", func(path string, obj storage.Object) error {
		fileName := filepath.Base(path)

		// 旧格式：gitea-backup-*.zip
		if strings.HasPrefix(fileName, "gitea-backup-") {
			info, err := obj.Stat()
			if err != nil {
				return nil
			}
			modTime := info.ModTime()
			if ts, ok := parseBackupTimestamp(fileName); ok {
				modTime = time.Unix(ts, 0)
			}
			backups = append(backups, BackupInfo{
				FileName: path,
				Size:     info.Size(),
				ModTime:  modTime,
			})
			return nil
		}

		// 新格式：YYYY-MM-DD/ 日期文件夹中的文件
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 2 {
			dirName := parts[0]
			t, err := time.Parse("2006-01-02", dirName)
			if err != nil {
				return nil
			}
			info, err := obj.Stat()
			if err != nil {
				return nil
			}
			backups = append(backups, BackupInfo{
				FileName:      dirName,
				Size:          info.Size(),
				ModTime:       t,
				IsDailyBackup: true,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list backup files: %w", err)
	}

	if len(backups) == 0 {
		return nil, nil
	}

	// 去重：同一天可能有多个文件
	seen := map[string]bool{}
	var unique []BackupInfo
	for _, b := range backups {
		key := b.FileName
		if b.IsDailyBackup {
			key = "daily:" + b.FileName
		}
		if !seen[key] {
			seen[key] = true
			unique = append(unique, b)
		}
	}

	// 按修改时间降序排列，取最新的
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].ModTime.After(unique[j].ModTime)
	})

	return &unique[0], nil
}

func parseBackupTimestamp(fileName string) (int64, bool) {
	rest := strings.TrimPrefix(fileName, "gitea-backup-")
	tsStr, _, hasDot := strings.Cut(rest, ".")
	if !hasDot || tsStr == "" {
		return 0, false
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	return ts, err == nil
}

// RestoreFromBackup 从备份存储恢复数据（自动检测格式）
func RestoreFromBackup(ctx context.Context, backupStorage storage.ObjectStorage, backupPath string) error {
	// 检测是否为新格式日期文件夹
	if t, err := time.Parse("2006-01-02", backupPath); err == nil && !t.IsZero() {
		return RestoreFromDailyBackup(ctx, backupStorage, backupPath)
	}

	// 旧格式 zip 归档恢复
	return RestoreFromZipBackup(ctx, backupStorage, backupPath)
}

// RestoreFromDailyBackup 从目录结构的每日备份恢复数据
func RestoreFromDailyBackup(ctx context.Context, backupStorage storage.ObjectStorage, dateDir string) error {
	ResetRestoreProgress()
	UpdateRestoreProgress("preparing daily backup restore", 5)

	// 恢复配置文件 app.ini
	UpdateRestoreProgress("restoring configuration", 10)
	if err := restoreDailyFile(backupStorage, dateDir+"/app.ini", setting.CustomConf); err != nil {
		log.Error("Failed to restore app.ini: %v", err)
	}

	// 恢复自定义目录 custom/
	UpdateRestoreProgress("restoring custom directory", 20)
	if err := restoreDailyDirectory(backupStorage, dateDir+"/custom", setting.CustomPath); err != nil {
		log.Error("Failed to restore custom: %v", err)
	}

	// 恢复仓库
	UpdateRestoreProgress("restoring repositories", 30)
	if err := restoreDailyDirectory(backupStorage, dateDir+"/repos", setting.RepoRootPath); err != nil {
		log.Error("Failed to restore repos: %v", err)
	}

	// 恢复 LFS 数据
	UpdateRestoreProgress("restoring LFS data", 45)
	lfsPath := setting.LFS.Storage.Path
	if lfsPath == "" {
		lfsPath = filepath.Join(setting.AppDataPath, "lfs")
	}
	if err := restoreDailyDirectory(backupStorage, dateDir+"/data/lfs", lfsPath); err != nil {
		log.Error("Failed to restore LFS: %v", err)
	}

	// 恢复附件
	UpdateRestoreProgress("restoring attachments", 55)
	attachPath := setting.Attachment.Storage.Path
	if attachPath == "" {
		attachPath = filepath.Join(setting.AppDataPath, "attachments")
	}
	if err := restoreDailyDirectory(backupStorage, dateDir+"/data/attachments", attachPath); err != nil {
		log.Error("Failed to restore attachments: %v", err)
	}

	// 恢复包
	UpdateRestoreProgress("restoring packages", 65)
	pkgPath := setting.Packages.Storage.Path
	if pkgPath == "" {
		pkgPath = filepath.Join(setting.AppDataPath, "packages")
	}
	if err := restoreDailyDirectory(backupStorage, dateDir+"/data/packages", pkgPath); err != nil {
		log.Error("Failed to restore packages: %v", err)
	}

	// 恢复其他 data/ 子目录
	UpdateRestoreProgress("restoring data directory", 75)
	if err := restoreDailyDataDirExcludingKnown(backupStorage, dateDir); err != nil {
		log.Error("Failed to restore data directory: %v", err)
	}

	// 恢复数据库
	UpdateRestoreProgress("restoring database", 85)
	if err := restoreDailyDatabase(ctx, backupStorage, dateDir); err != nil {
		SetRestoreFailed(fmt.Sprintf("failed to restore database: %v", err))
		return err
	}

	UpdateRestoreProgress("restore completed", 100)
	SetRestoreDone()
	return nil
}

// restoreDailyFile 从每日备份恢复单个文件
func restoreDailyFile(backupStorage storage.ObjectStorage, remotePath, destPath string) error {
	obj, err := backupStorage.Open(remotePath)
	if err != nil {
		log.Info("File %s not found in backup, skipping", remotePath)
		return nil
	}
	defer obj.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), os.ModePerm); err != nil {
		return fmt.Errorf("failed to create parent dir for %s: %w", destPath, err)
	}

	dst, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", destPath, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, obj); err != nil {
		return fmt.Errorf("failed to copy %s: %w", remotePath, err)
	}

	log.Info("Restored %s to %s", remotePath, destPath)
	return nil
}

// restoreDailyDirectory 从每日备份恢复整个目录
func restoreDailyDirectory(backupStorage storage.ObjectStorage, remotePrefix, destPath string) error {
	var files []string
	err := backupStorage.IterateObjects(remotePrefix, func(path string, obj storage.Object) error {
		files = append(files, path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to list %s: %w", remotePrefix, err)
	}
	if len(files) == 0 {
		log.Info("Directory %s not found in backup, skipping", remotePrefix)
		return nil
	}

	for _, remotePath := range files {
		relPath := strings.TrimPrefix(remotePath, remotePrefix+"/")
		localPath := filepath.Join(destPath, filepath.FromSlash(relPath))

		if err := os.MkdirAll(filepath.Dir(localPath), os.ModePerm); err != nil {
			return err
		}

		obj, err := backupStorage.Open(remotePath)
		if err != nil {
			return fmt.Errorf("open %s: %w", remotePath, err)
		}

		dst, err := os.Create(localPath)
		if err != nil {
			obj.Close()
			return err
		}

		_, err = io.Copy(dst, obj)
		obj.Close()
		dst.Close()
		if err != nil {
			return fmt.Errorf("copy %s: %w", remotePath, err)
		}
	}

	log.Info("Restored %s to %s (%d files)", remotePrefix, destPath, len(files))
	return nil
}

// restoreDailyDataDirExcludingKnown 恢复 data/ 目录但跳过已单独恢复的子目录
func restoreDailyDataDirExcludingKnown(backupStorage storage.ObjectStorage, dateDir string) error {
	prefix := dateDir + "/data/"
	var files []string
	err := backupStorage.IterateObjects(prefix, func(path string, obj storage.Object) error {
		files = append(files, path)
		return nil
	})
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}

	alreadyRestored := map[string]bool{
		"lfs":         true,
		"attachments": true,
		"packages":    true,
	}

	for _, remotePath := range files {
		relPath := strings.TrimPrefix(remotePath, prefix)
		parts := strings.SplitN(relPath, "/", 2)
		if alreadyRestored[parts[0]] {
			continue
		}

		localPath := filepath.Join(setting.AppDataPath, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(localPath), os.ModePerm); err != nil {
			return err
		}

		obj, err := backupStorage.Open(remotePath)
		if err != nil {
			log.Warn("Failed to open %s: %v", remotePath, err)
			continue
		}

		dst, err := os.Create(localPath)
		if err != nil {
			obj.Close()
			return err
		}

		_, err = io.Copy(dst, obj)
		obj.Close()
		dst.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

// restoreDailyDatabase 从每日备份恢复数据库
func restoreDailyDatabase(ctx context.Context, backupStorage storage.ObjectStorage, dateDir string) error {
	obj, err := backupStorage.Open(dateDir + "/gitea-db.sql")
	if err != nil {
		return fmt.Errorf("database dump not found in backup: %w", err)
	}
	defer obj.Close()

	destPath := filepath.Join(setting.AppDataPath, "gitea-db-restore.sql")
	dst, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create restore sql file: %w", err)
	}

	if _, err := io.Copy(dst, obj); err != nil {
		dst.Close()
		return fmt.Errorf("failed to copy database dump: %w", err)
	}
	dst.Close()

	if err := executeSQLDump(ctx, destPath); err != nil {
		log.Error("Failed to execute database dump from %s: %v", destPath, err)
		log.Warn("SQL dump file is kept at %s for manual restore", destPath)
		return fmt.Errorf("failed to execute database dump: %w", err)
	}

	os.Remove(destPath)
	log.Info("Database restored successfully from %s", dateDir)
	return nil
}

// RestoreFromZipBackup 从旧的 zip 归档格式恢复数据
func RestoreFromZipBackup(ctx context.Context, backupStorage storage.ObjectStorage, backupPath string) error {
	ResetRestoreProgress()
	UpdateRestoreProgress("downloading backup", 5)

	tmpDir := filepath.Join(setting.AppDataPath, "tmp", "restore")
	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		SetRestoreFailed(fmt.Sprintf("failed to create temp dir: %v", err))
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	backupFile := filepath.Join(tmpDir, filepath.Base(backupPath))
	if err := downloadBackup(ctx, backupStorage, backupPath, backupFile); err != nil {
		SetRestoreFailed(fmt.Sprintf("failed to download backup: %v", err))
		return fmt.Errorf("failed to download backup: %w", err)
	}
	UpdateRestoreProgress("download complete, extracting archive", 25)

	archiveFS, err := archives.FileSystem(ctx, backupFile, nil)
	if err != nil {
		SetRestoreFailed(fmt.Sprintf("failed to open archive: %v", err))
		return fmt.Errorf("failed to open archive: %w", err)
	}

	UpdateRestoreProgress("restoring configuration", 30)
	if err := restoreSingleFile(archiveFS, "app.ini", setting.CustomConf); err != nil {
		log.Error("Failed to restore app.ini: %v", err)
	}

	UpdateRestoreProgress("restoring custom directory", 35)
	if err := restoreFromArchiveFS(archiveFS, "custom", setting.CustomPath); err != nil {
		log.Error("Failed to restore custom: %v", err)
	}

	UpdateRestoreProgress("restoring repositories", 45)
	if err := restoreFromArchiveFS(archiveFS, "repos", setting.RepoRootPath); err != nil {
		log.Error("Failed to restore repos: %v", err)
	}

	UpdateRestoreProgress("restoring LFS data", 55)
	lfsPath := setting.LFS.Storage.Path
	if lfsPath == "" {
		lfsPath = filepath.Join(setting.AppDataPath, "lfs")
	}
	if err := restoreFromArchiveFS(archiveFS, "data/lfs", lfsPath); err != nil {
		log.Error("Failed to restore LFS: %v", err)
	}

	UpdateRestoreProgress("restoring attachments", 65)
	attachPath := setting.Attachment.Storage.Path
	if attachPath == "" {
		attachPath = filepath.Join(setting.AppDataPath, "attachments")
	}
	if err := restoreFromArchiveFS(archiveFS, "data/attachments", attachPath); err != nil {
		log.Error("Failed to restore attachments: %v", err)
	}

	UpdateRestoreProgress("restoring packages", 72)
	pkgPath := setting.Packages.Storage.Path
	if pkgPath == "" {
		pkgPath = filepath.Join(setting.AppDataPath, "packages")
	}
	if err := restoreFromArchiveFS(archiveFS, "data/packages", pkgPath); err != nil {
		log.Error("Failed to restore packages: %v", err)
	}

	UpdateRestoreProgress("restoring data directory", 80)
	if err := restoreDataDirExcludingKnown(archiveFS); err != nil {
		log.Error("Failed to restore data directory: %v", err)
	}

	UpdateRestoreProgress("restoring database", 90)
	if err := restoreDatabaseFromArchive(ctx, archiveFS); err != nil {
		SetRestoreFailed(fmt.Sprintf("failed to restore database: %v", err))
		log.Error("Failed to restore database: %v", err)
		return err
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
	if _, err := fs.ReadDir(archiveFS, subDir); err != nil {
		log.Info("Directory %s not found in backup, skipping", subDir)
		return nil
	}

	if err := os.MkdirAll(destPath, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create %s: %w", destPath, err)
	}

	return fs.WalkDir(archiveFS, subDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath := strings.TrimPrefix(path, subDir+"/")
		if relPath == subDir {
			return nil
		}
		destFile := filepath.Join(destPath, filepath.FromSlash(relPath))

		if d.IsDir() {
			return os.MkdirAll(destFile, os.ModePerm)
		}

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
	if _, err := fs.ReadDir(archiveFS, "data"); err != nil {
		log.Info("Directory data not found in backup, skipping")
		return nil
	}

	alreadyRestored := map[string]bool{
		"lfs":         true,
		"attachments": true,
		"packages":    true,
	}

	return fs.WalkDir(archiveFS, "data", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath := strings.TrimPrefix(path, "data/")
		if relPath == "data" {
			return nil
		}

		parts := strings.SplitN(relPath, "/", 2)
		if alreadyRestored[parts[0]] {
			if len(parts) == 1 && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		destFile := filepath.Join(setting.AppDataPath, filepath.FromSlash(relPath))

		if d.IsDir() {
			return os.MkdirAll(destFile, os.ModePerm)
		}

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
func restoreDatabaseFromArchive(ctx context.Context, archiveFS fs.FS) error {
	src, err := archiveFS.Open("gitea-db.sql")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("database dump gitea-db.sql not found in backup")
		}
		return fmt.Errorf("failed to open database dump gitea-db.sql: %w", err)
	}
	defer src.Close()

	destPath := filepath.Join(setting.AppDataPath, "gitea-db-restore.sql")
	dst, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create restore sql file: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return fmt.Errorf("failed to copy database dump: %w", err)
	}
	dst.Close()

	if err := executeSQLDump(ctx, destPath); err != nil {
		log.Error("Failed to execute database dump from %s: %v", destPath, err)
		log.Warn("SQL dump file is kept at %s for manual restore", destPath)
		return fmt.Errorf("failed to execute database dump: %w", err)
	}

	os.Remove(destPath)
	log.Info("Database restored successfully from %s", destPath)
	return nil
}

// executeSQLDump 读取 SQL dump 文件并执行其中的语句
func executeSQLDump(ctx context.Context, dumpPath string) error {
	f, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("failed to open dump file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	const maxScanTokenSize = 1024 * 1024 * 10 // 10MB
	buf := make([]byte, maxScanTokenSize)
	scanner.Buffer(buf, maxScanTokenSize)

	var statements []string
	var currentStmt strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "/*") {
			continue
		}

		currentStmt.WriteString(line)
		currentStmt.WriteString("\n")

		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			stmt := strings.TrimSpace(currentStmt.String())
			if stmt != "" {
				statements = append(statements, stmt)
			}
			currentStmt.Reset()
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read dump file: %w", err)
	}

	if currentStmt.Len() > 0 {
		stmt := strings.TrimSpace(currentStmt.String())
		if stmt != "" {
			statements = append(statements, stmt)
		}
	}

	if len(statements) == 0 {
		log.Info("No SQL statements found in dump file")
		return nil
	}

	log.Info("Executing %d SQL statements from dump", len(statements))

	e := db.GetEngine(ctx)
	sess := e.Context(ctx)

	if err := sess.Begin(); err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	for i, stmt := range statements {
		if _, err := sess.Exec(stmt); err != nil {
			sess.Rollback()
			return fmt.Errorf("failed to execute statement %d: %w", i+1, err)
		}
	}

	if err := sess.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Info("All %d SQL statements executed successfully", len(statements))
	return nil
}
